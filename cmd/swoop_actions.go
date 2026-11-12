package cmd

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"

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
	Use:   "init [target] [-- terraform flags]",
	Short: "Run terraform init in target root(s)",
	Long: `Initialize terraform root directories.

Target can be an exact path, glob pattern, or substring match.
If the target matches a single root, it runs immediately.
If it matches multiple roots (via glob), they run sequentially.

Use --changed to target roots with modified .tf files.
Use --dir to filter by top-level directory.`,
	Args: targetArgs,
	RunE: swoopActionRunE("init"),
}

var swoopPlanCmd = &cobra.Command{
	Use:   "plan [target] [-- terraform flags]",
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
	Args: targetArgs,
	RunE: swoopActionRunE("plan"),
}

var swoopApplyCmd = &cobra.Command{
	Use:   "apply [target] [-- terraform flags]",
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
	Args: targetArgs,
	RunE: swoopActionRunE("apply"),
}

// targetArgs allows one target before "--" and anything after it.
func targetArgs(cmd *cobra.Command, args []string) error {
	pos, _ := swoop.SplitPassthrough(args, cmd.ArgsLenAtDash())
	return cobra.MaximumNArgs(1)(cmd, pos)
}

func swoopActionRunE(action string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		pos, extra := swoop.SplitPassthrough(args, cmd.ArgsLenAtDash())
		target := ""
		if len(pos) > 0 {
			target = pos[0]
		}
		err := runSwoopActionCmd(action, target, extra)
		if errors.Is(err, swoop.ErrChanges) {
			// Exit 2 is the signal; terraform's output already shows the changes.
			cmd.SilenceErrors = true
		}
		return err
	}
}

// runSwoopActionCmd selects roots from the target plus --changed/--dir and runs the action on them, forwarding extra to terraform.
func runSwoopActionCmd(action, target string, extra []string) error {
	if err := swoop.CheckPassthrough(extra); err != nil {
		return err
	}
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

	matches, matchType, err := swoop.Select(roots, baseDir, swoop.Selection{Target: target, Changed: swoopChanged, Dir: swoopActionDir})
	if err != nil {
		return err
	}
	if matchType == swoop.MatchFuzzy && !confirmFuzzyMatch(matches, target) {
		return fmt.Errorf("aborted")
	}

	if action == "apply" {
		if err := applyGuards(); err != nil {
			return err
		}
	}
	// Log in to every profile up front so a batch doesn't stop halfway on an expired session.
	checked := map[string]bool{}
	for _, root := range matches {
		res := swoop.EffectiveProfiles(cfg, root, environment)
		for _, p := range []string{res.ProviderProfile, res.BackendProfile} {
			if p != "" && !checked[p] {
				if err := ensureSSOSession(p); err != nil {
					return err
				}
				checked[p] = true
			}
		}
	}

	r := swoop.Runner{
		Cfg:         cfg,
		BaseDir:     baseDir,
		Environment: environment,
		Policy:      withPlanFlags(swoop.Policy{PrintContext: true, ExtraArgs: extra}),
		Preflight:   localVersionPreflight,
	}
	return r.Run(action, matches)
}

// localVersionPreflight writes a missing pin when write_version is on, then checks it: a single root may offer to install, a batch only warns.
func localVersionPreflight(root *swoop.Root, batch bool) error {
	tfCommand := cfg.TerraformCommand()
	tfManager := cfg.TerraformVersionManager()
	indent := ""
	if batch {
		indent = "  "
	}

	if cfg != nil && cfg.Terraform.WriteVersion && root.TFVersion == "" {
		if file, v, err := swoop.EnsureTFVersion(tfCommand, tfManager, *root, cfg.Terraform.DefaultVersion); err != nil {
			slog.Warn("ensure terraform version", "root", root.Path, "err", err)
			fmt.Fprintf(os.Stderr, "%swarning: %v\n", indent, err)
		} else if v != "" {
			root.TFVersion = v
			slog.Info("wrote version pin", "root", root.Path, "file", file, "version", v)
			fmt.Fprintf(os.Stderr, "%swrote %s: %s\n", indent, file, v)
		}
	}

	if !batch {
		return handleTFVersionCheck(tfCommand, tfManager, *root)
	}
	if check := swoop.CheckTFVersion(tfCommand, tfManager, *root); !check.OK {
		slog.Warn("terraform version mismatch", "root", root.Path, "required", check.Required, "installed", check.Installed)
		fmt.Fprint(os.Stderr, swoop.VersionMismatchWarning(tfCommand, *root, check, indent))
	}
	return nil
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
	fmt.Fprint(os.Stderr, swoop.VersionMismatchWarning(binary, root, check, ""))
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
		return swoop.AmbiguousError(matches, target)
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
