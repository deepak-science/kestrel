package cmd

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"

	"github.com/deepak-science/kestrel/internal/guard"
	"github.com/deepak-science/kestrel/internal/swoop"
	"github.com/spf13/cobra"
)

var (
	swoopChanged   string
	swoopActionDir string
	swoopSavePlan  bool
	swoopSummary   bool
	swoopApplyPlan bool
)

func withPlanFlags(pol swoop.Policy) swoop.Policy {
	pol.SavePlan = swoopSavePlan
	pol.Summary = swoopSummary
	pol.ApplyPlan = swoopApplyPlan
	return pol
}

var swoopInitCmd = &cobra.Command{
	Use:   "init [target]",
	Short: "Run terraform init in target root(s)",
	Long: `Initialize terraform root directories.

Target can be an exact path, glob pattern, or substring match.
If the target matches a single root, it runs immediately.
If it matches multiple roots (via glob), they run sequentially.

Use --changed to target roots with modified .tf files.
Use --dir to filter by top-level directory.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		target := ""
		if len(args) > 0 {
			target = args[0]
		}
		return runSwoopActionCmd("init", target)
	},
}

var swoopPlanCmd = &cobra.Command{
	Use:   "plan [target]",
	Short: "Run terraform plan in target root(s)",
	Long: `Plan changes for terraform root directories.

Target can be an exact path, glob pattern, or substring match.
If the target matches a single root, it runs immediately.
If it matches multiple roots (via glob), they run sequentially with a summary.

Use --changed to target roots with modified .tf files.
Use --dir to filter by top-level directory.

Use --out to save the plan to .terraform/kest.tfplan in each root, for
"kest swoop apply --plan". --summary prints one line per changed resource
(with changed attributes) instead of the full plan, and also saves it.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		target := ""
		if len(args) > 0 {
			target = args[0]
		}
		return runSwoopActionCmd("plan", target)
	},
}

var swoopApplyCmd = &cobra.Command{
	Use:   "apply [target]",
	Short: "Run terraform apply in target root(s)",
	Long: `Apply changes for terraform root directories.

Target can be an exact path, glob pattern, or substring match.
If the target matches a single root, it runs immediately.
If it matches multiple roots (via glob), they run sequentially with a summary.

Use --changed to target roots with modified .tf files.
Use --dir to filter by top-level directory.

Use --plan to apply the plan saved by "kest swoop plan --out" instead of
re-planning. Terraform applies a saved plan without prompting. kest refuses
if the planfile is missing, or if HEAD, the root's config, or the AWS
profile changed since the plan.

Apply has additional safety guards:
- Requires --force when not in CI
- Checks for a clean git worktree`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		target := ""
		if len(args) > 0 {
			target = args[0]
		}
		return runSwoopActionCmd("apply", target)
	},
}

// runSwoopActionCmd resolves targets (including --changed and --dir flags)
// and dispatches to single or batch execution.
func runSwoopActionCmd(action, target string) error {
	baseDir, err := resolveBaseDir()
	if err != nil {
		return err
	}

	roots, err := discoverRoots(baseDir)
	if err != nil {
		return err
	}
	if len(roots) == 0 {
		return fmt.Errorf("no terraform roots found under %s", baseDir)
	}

	matches, matchType, err := resolveTargets(roots, baseDir, target)
	if err != nil {
		return err
	}

	if len(matches) == 0 {
		return fmt.Errorf("no matching roots found")
	}

	// Fuzzy matches require user confirmation.
	if matchType == swoop.MatchFuzzy {
		if !confirmFuzzyMatch(matches, target) {
			return fmt.Errorf("aborted")
		}
	}

	// Single root — use the direct path.
	if len(matches) == 1 {
		return runSwoopAction(action, matches[0].Path)
	}

	// Multiple roots — batch mode.
	// For non-glob substring matches that are ambiguous, error out.
	if !isBatchTarget(target) && swoopChanged == "" && swoopActionDir == "" {
		return ambiguousTargetError(matches, target)
	}

	return runSwoopBatch(action, matches, baseDir)
}

// resolveTargets combines target string, --changed, and --dir into a set of roots.
// Returns the matched roots and the match type used for the target resolution.
func resolveTargets(roots []swoop.Root, baseDir, target string) ([]swoop.Root, swoop.MatchType, error) {
	matches := roots

	// Filter by directory first.
	if swoopActionDir != "" {
		matches = swoop.ResolveByDir(matches, swoopActionDir)
	}

	// Filter by --changed.
	if swoopChanged != "" {
		ref := ""
		if swoopChanged != "true" {
			ref = swoopChanged
		}
		changed, err := swoop.ChangedRoots(matches, baseDir, ref)
		if err != nil {
			return nil, swoop.MatchAll, fmt.Errorf("detecting changed roots: %w", err)
		}
		matches = changed
	}

	// Filter by target string.
	matchType := swoop.MatchAll
	if target != "" {
		matches, matchType = swoop.ResolveWithType(matches, target)
	}

	return matches, matchType, nil
}

// isBatchTarget returns true if the target looks like a glob pattern
// (contains * or ?) indicating the user intended multi-root matching.
func isBatchTarget(target string) bool {
	return strings.ContainsAny(target, "*?[")
}

// runSwoopAction executes a terraform action against a single root.
func runSwoopAction(action, target string) error {
	baseDir, err := resolveBaseDir()
	if err != nil {
		return err
	}

	roots, err := discoverRoots(baseDir)
	if err != nil {
		return err
	}
	if len(roots) == 0 {
		return fmt.Errorf("no terraform roots found under %s", baseDir)
	}

	matches, matchType := swoop.ResolveWithType(roots, target)
	if len(matches) == 0 {
		return fmt.Errorf("no roots matching %q", target)
	}
	if len(matches) > 1 {
		return ambiguousTargetError(matches, target)
	}

	if matchType == swoop.MatchFuzzy {
		if !confirmFuzzyMatch(matches, target) {
			return fmt.Errorf("aborted")
		}
	}

	root := matches[0]
	return executeSingle(action, root, baseDir)
}

func executeSingle(action string, root swoop.Root, baseDir string) error {
	// Apply-specific guards.
	if action == "apply" {
		if err := applyGuards(); err != nil {
			return err
		}
	}

	// Resolve credentials (provider + S3-backend fallback), shared with kestci.
	res := swoop.EffectiveProfiles(cfg, root, environment)

	if err := ensureSSOSession(res.ProviderProfile); err != nil {
		return err
	}
	if res.BackendProfile != "" {
		if err := ensureSSOSession(res.BackendProfile); err != nil {
			return err
		}
	}

	tfCommand := cfg.TerraformCommand()
	tfManager := cfg.TerraformVersionManager()

	// Write the version-pin file if missing and configured.
	if cfg != nil && cfg.Terraform.WriteVersion && root.TFVersion == "" {
		if file, v, err := swoop.EnsureTFVersion(tfCommand, tfManager, root, cfg.Terraform.DefaultVersion); err != nil {
			slog.Warn("ensure terraform version", "root", root.Path, "err", err)
			fmt.Fprintf(os.Stderr, "warning: %v\n", err)
		} else if v != "" {
			root.TFVersion = v
			slog.Info("wrote version pin", "root", root.Path, "file", file, "version", v)
			fmt.Fprintf(os.Stderr, "wrote %s: %s\n", file, v)
		}
	}

	// Version-manager preflight check.
	if err := handleTFVersionCheck(tfCommand, tfManager, root); err != nil {
		return err
	}

	slog.Info("swoop action",
		"action", action,
		"root", root.Path,
		"dir", root.Dir,
		"aws_profile", res.Effective,
		"tf_version", root.TFVersion,
	)

	result, err := swoop.Execute(cfg, root, baseDir, action, res, withPlanFlags(swoop.Policy{PrintContext: true}))
	if err != nil {
		slog.Warn("terraform action failed", "action", action, "root", root.Path, "err", err)
		return fmt.Errorf("terraform %s failed: %w", action, err)
	}
	slog.Info("terraform action complete",
		"action", action,
		"root", root.Path,
		"exit_code", result.ExitCode,
		"plan_summary", result.PlanSummary,
	)
	return nil
}

// versionMismatchWarning names the fix: repin when the active binary is newer than the pin, else install the pinned version.
func versionMismatchWarning(binary string, root swoop.Root, check swoop.TFVersionCheck, indent string) string {
	pinFile := swoop.PinFileFor(root, "")
	var b strings.Builder
	if check.Installed == "" {
		fmt.Fprintf(&b, "%swarning: %s pins %s %s (%s), and %s version failed\n", indent, root.Path, binary, check.Required, pinFile, binary)
	} else {
		fmt.Fprintf(&b, "%swarning: %s pins %s %s (%s) but %s is active\n", indent, root.Path, binary, check.Required, pinFile, check.Installed)
	}
	switch {
	case check.Installed != "" && swoop.VersionNewer(check.Installed, check.Required):
		fmt.Fprintf(&b, "%s  to move the pin to %s: kest swoop pin %s\n", indent, check.Installed, root.Path)
	case !check.VersionManagerAvailable:
		fmt.Fprintf(&b, "%s  install %s %s, or set terraform.version_manager to one that's on PATH\n", indent, binary, check.Required)
	}
	return b.String()
}

// handleTFVersionCheck checks the terraform version and offers to install via
// the configured version manager if there's a mismatch.
func handleTFVersionCheck(binary, manager string, root swoop.Root) error {
	check := swoop.CheckTFVersion(binary, manager, root)
	if check.OK {
		return nil
	}

	slog.Warn("terraform version mismatch",
		"root", root.Path,
		"required", check.Required,
		"installed", check.Installed,
	)
	fmt.Fprint(os.Stderr, versionMismatchWarning(binary, root, check, ""))
	if !check.VersionManagerAvailable {
		return nil // warn but don't block
	}

	// Auto-install if configured (skip in CI where manager use is implicit).
	if cfg != nil && cfg.Terraform.AutoInstallPinned && !guard.IsCI() {
		fmt.Fprintf(os.Stderr, "  auto-installing terraform %s via %s...\n\n", check.Required, manager)
		if err := swoop.InstallTFVersion(manager, check.Required); err != nil {
			return fmt.Errorf("%s install failed: %w", manager, err)
		}
		fmt.Fprintln(os.Stderr)
		return nil
	}

	installCmd := swoop.FormatTFVersionCommand(manager, check.Required)
	fmt.Fprintf(os.Stderr, "\n  %s\n\n", installCmd)
	fmt.Fprintf(os.Stderr, "Install now? [y/N] ")

	var answer string
	_, _ = fmt.Scanln(&answer)
	answer = strings.TrimSpace(strings.ToLower(answer))

	if answer != "y" && answer != "yes" {
		fmt.Fprintln(os.Stderr, "Continuing with current terraform version.")
		return nil
	}

	fmt.Fprintln(os.Stderr)
	if err := swoop.InstallTFVersion(manager, check.Required); err != nil {
		return fmt.Errorf("%s install failed: %w", manager, err)
	}
	fmt.Fprintln(os.Stderr)
	return nil
}

// batchResult tracks the outcome of one root in a batch run.
type batchResult struct {
	root    swoop.Root
	err     error
	summary string
}

func runSwoopBatch(action string, roots []swoop.Root, baseDir string) error {
	// Apply-specific guards (once, not per root).
	if action == "apply" {
		if err := applyGuards(); err != nil {
			return err
		}
	}

	fmt.Fprintf(os.Stderr, "Running terraform %s on %d root(s)...\n\n", action, len(roots))

	// Check SSO sessions once per unique profile before starting the batch.
	// Includes both the apply profile and (if different) the backend profile
	// derived from the root's `backend "s3"` block.
	checkedProfiles := map[string]bool{}
	for _, root := range roots {
		res := swoop.EffectiveProfiles(cfg, root, environment)
		for _, p := range []string{res.ProviderProfile, res.BackendProfile} {
			if p != "" && !checkedProfiles[p] {
				if err := ensureSSOSession(p); err != nil {
					return err
				}
				checkedProfiles[p] = true
			}
		}
	}

	tfCommand := cfg.TerraformCommand()
	tfManager := cfg.TerraformVersionManager()
	results := make([]batchResult, len(roots))

	for i, root := range roots {
		res := swoop.EffectiveProfiles(cfg, root, environment)

		// Print header.
		fmt.Fprintf(os.Stderr, "━━━ [%d/%d] %s ━━━\n", i+1, len(roots), root.Path)

		// Write the version-pin file if missing and configured.
		if cfg != nil && cfg.Terraform.WriteVersion && root.TFVersion == "" {
			if file, v, err := swoop.EnsureTFVersion(tfCommand, tfManager, root, cfg.Terraform.DefaultVersion); err != nil {
				slog.Warn("ensure terraform version", "root", root.Path, "err", err)
				fmt.Fprintf(os.Stderr, "  warning: %v\n", err)
			} else if v != "" {
				root.TFVersion = v
				slog.Info("wrote version pin", "root", root.Path, "file", file, "version", v)
				fmt.Fprintf(os.Stderr, "  wrote %s: %s\n", file, v)
			}
		}

		check := swoop.CheckTFVersion(tfCommand, tfManager, root)
		if !check.OK {
			slog.Warn("terraform version mismatch",
				"root", root.Path,
				"required", check.Required,
				"installed", check.Installed,
			)
			fmt.Fprint(os.Stderr, versionMismatchWarning(tfCommand, root, check, "  "))
		}

		slog.Info("swoop batch action",
			"action", action,
			"root", root.Path,
			"aws_profile", res.Effective,
		)
		execResult, err := swoop.Execute(cfg, root, baseDir, action, res, withPlanFlags(swoop.Policy{}))

		br := batchResult{root: root, err: err}
		if execResult != nil {
			br.summary = execResult.PlanSummary
		}
		results[i] = br

		if err != nil {
			slog.Warn("swoop batch action failed", "action", action, "root", root.Path, "err", err)
			fmt.Fprintf(os.Stderr, "  FAILED: %v\n", err)
		}
		fmt.Fprintln(os.Stderr)
	}

	// Print summary.
	printBatchSummary(action, results)

	// Return error if any root failed.
	var failed int
	for _, r := range results {
		if r.err != nil {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d root(s) failed", failed, len(results))
	}
	return nil
}

func printBatchSummary(action string, results []batchResult) {
	fmt.Fprintf(os.Stderr, "━━━ Swoop %s summary ━━━\n", action)
	w := tabwriter.NewWriter(os.Stderr, 0, 2, 2, ' ', 0)
	for _, r := range results {
		status := "OK"
		if r.err != nil {
			status = "FAILED"
		}
		detail := ""
		if r.summary != "" {
			detail = r.summary
		}
		if detail != "" {
			_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\n", r.root.Path, status, detail)
		} else {
			_, _ = fmt.Fprintf(w, "  %s\t%s\n", r.root.Path, status)
		}
	}
	_ = w.Flush()
}

func applyGuards() error {
	if force {
		return nil
	}
	if err := guard.CheckCI(); err != nil {
		return err
	}
	if err := guard.CheckCleanWorktree(); err != nil {
		return err
	}
	return nil
}

// confirmFuzzyMatch prints the resolved root(s) and prompts the user for
// confirmation. Returns true if the user confirms.
func confirmFuzzyMatch(matches []swoop.Root, target string) bool {
	fmt.Fprintf(os.Stderr, "fuzzy match for %q resolved to:\n", target)
	for _, m := range matches {
		fmt.Fprintf(os.Stderr, "  %s\n", m.Path)
	}
	fmt.Fprintf(os.Stderr, "\nProceed? [y/N] ")

	var answer string
	_, _ = fmt.Scanln(&answer)
	answer = strings.TrimSpace(strings.ToLower(answer))
	return answer == "y" || answer == "yes"
}

func ambiguousTargetError(matches []swoop.Root, target string) error {
	msg := fmt.Sprintf("target %q matches %d roots — be more specific or use a glob pattern:\n", target, len(matches))
	for _, m := range matches {
		msg += fmt.Sprintf("  %s\n", m.Path)
	}
	return fmt.Errorf("%s", msg)
}

var swoopEditCmd = &cobra.Command{
	Use:   "edit <target>",
	Short: "Open $EDITOR in a terraform root's directory",
	Long: `Open your editor in the directory of a terraform root.

Uses swoop.editor from config, then $EDITOR, then $VISUAL.
Target can be an exact path, glob pattern, or substring match.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSwoopSingleCmd("edit", args[0])
	},
}

var swoopCDCmd = &cobra.Command{
	Use:   "cd <target>",
	Short: "Print a cd command for a terraform root (use with eval)",
	Long: `Outputs a cd (or pushd) command for a terraform root's directory.

Usage:
  eval "$(kest swoop cd <target>)"

The shell command (cd or pushd) is controlled by swoop.cd_mode in config.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSwoopSingleCmd("cd", args[0])
	},
}

// runSwoopSingleCmd resolves a single root and dispatches to edit or cd.
func runSwoopSingleCmd(action, target string) error {
	baseDir, err := resolveBaseDir()
	if err != nil {
		return err
	}

	roots, err := discoverRoots(baseDir)
	if err != nil {
		return err
	}
	if len(roots) == 0 {
		return fmt.Errorf("no terraform roots found under %s", baseDir)
	}

	matches, matchType := swoop.ResolveWithType(roots, target)
	if len(matches) == 0 {
		return fmt.Errorf("no roots matching %q", target)
	}
	if len(matches) > 1 {
		return ambiguousTargetError(matches, target)
	}

	if matchType == swoop.MatchFuzzy {
		if !confirmFuzzyMatch(matches, target) {
			return fmt.Errorf("aborted")
		}
	}

	root := matches[0]
	switch action {
	case "edit":
		return executeEdit(root)
	case "cd":
		return executeCD(root)
	default:
		return fmt.Errorf("unknown action %q", action)
	}
}

// executeEdit opens the user's editor in the root's directory.
func executeEdit(root swoop.Root) error {
	editor := ""
	if cfg != nil {
		editor = cfg.Swoop.Editor
	}
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = os.Getenv("VISUAL")
	}
	if editor == "" {
		return fmt.Errorf("no editor configured — set $EDITOR or swoop.editor in config")
	}

	cmd := exec.Command(editor, ".")
	cmd.Dir = root.AbsPath
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// executeCD prints a shell cd/pushd command to stdout for eval consumption.
func executeCD(root swoop.Root) error {
	verb := "cd"
	if cfg != nil && cfg.Swoop.CDMode == "pushd" {
		verb = "pushd"
	}
	fmt.Printf("%s %s\n", verb, shellQuote(root.AbsPath))
	return nil
}

func init() {
	// Shared flags for all action commands.
	for _, c := range []*cobra.Command{swoopInitCmd, swoopPlanCmd, swoopApplyCmd, swoopPinCmd} {
		c.Flags().StringVar(&swoopChanged, "changed", "", "target roots with modified .tf files (optionally specify git ref, e.g. --changed=HEAD~3)")
		// Allow --changed without a value (defaults to "true" meaning "use merge-base").
		c.Flags().Lookup("changed").NoOptDefVal = "true"
		c.Flags().StringVar(&swoopActionDir, "dir", "", "filter by top-level directory")

		c.ValidArgsFunction = completeSwoopRoots
		_ = c.RegisterFlagCompletionFunc("dir", completeSwoopDirs)
	}

	swoopPlanCmd.Flags().BoolVar(&swoopSavePlan, "out", false, "save the plan to .terraform/kest.tfplan for apply --plan")
	swoopPlanCmd.Flags().BoolVar(&swoopSummary, "summary", false, "print a resource-level summary instead of the full plan (implies --out)")
	swoopApplyCmd.Flags().BoolVar(&swoopApplyPlan, "plan", false, "apply the saved plan from plan --out instead of re-planning")

	swoopCmd.AddCommand(swoopInitCmd)
	swoopCmd.AddCommand(swoopPlanCmd)
	swoopCmd.AddCommand(swoopApplyCmd)

	// Edit and cd don't share the batch flags.
	swoopEditCmd.ValidArgsFunction = completeSwoopRoots
	swoopCDCmd.ValidArgsFunction = completeSwoopRoots
	swoopCmd.AddCommand(swoopEditCmd)
	swoopCmd.AddCommand(swoopCDCmd)
}
