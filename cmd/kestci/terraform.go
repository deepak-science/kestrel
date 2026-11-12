package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/deepak-science/kestrel/internal/guard"
	"github.com/deepak-science/kestrel/internal/swoop"
	"github.com/spf13/cobra"
)

var (
	tfInitCmd  = terraformCmd("init")
	tfPlanCmd  = terraformCmd("plan")
	tfApplyCmd = terraformCmd("apply")
)

func terraformCmd(action string) *cobra.Command {
	return &cobra.Command{
		Use:   action + " [target] [-- terraform flags]",
		Short: "Run terraform " + action + " in matching root(s)",
		Long: `Target is an exact path, a glob, or a substring matching one root; fuzzy
matches are refused. --changed and --dir select roots instead of, or as well
as, a target. Several roots run in sequence with a summary.

Flags after -- go to terraform, as -name=value. With plan -- -detailed-exitcode,
kestci exits 2 when any root has changes and none failed.`,
		Args: targetArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			pos, extra := swoop.SplitPassthrough(args, cmd.ArgsLenAtDash())
			target := ""
			if len(pos) > 0 {
				target = pos[0]
			}
			err := runTerraformAction(action, target, extra)
			if errors.Is(err, swoop.ErrChanges) {
				// Exit 2 is the signal; terraform's output already shows the changes.
				cmd.SilenceErrors = true
			}
			return err
		},
	}
}

// targetArgs allows one target before "--" and anything after it.
func targetArgs(cmd *cobra.Command, args []string) error {
	pos, _ := swoop.SplitPassthrough(args, cmd.ArgsLenAtDash())
	return cobra.MaximumNArgs(1)(cmd, pos)
}

func runTerraformAction(action, target string, extra []string) error {
	if err := requireCI(); err != nil {
		return err
	}
	if target == "" && tfChanged == "" && tfDir == "" {
		return fmt.Errorf("target required (or select roots with --changed / --dir)")
	}
	if err := swoop.CheckPassthrough(extra); err != nil {
		return err
	}

	// Apply-specific guards (always enforced, no --force).
	if action == "apply" {
		if err := guard.CheckCleanWorktree(); err != nil {
			return err
		}
	}

	baseDir, err := ciResolveBaseDir()
	if err != nil {
		return err
	}

	roots, err := swoop.Discover(baseDir)
	if err != nil {
		return fmt.Errorf("discovering roots: %w", err)
	}
	if len(roots) == 0 {
		return fmt.Errorf("no terraform roots found under %s", baseDir)
	}
	swoop.EnrichWithAccountIDs(roots, baseDir)

	matches, matchType, err := swoop.Select(roots, baseDir, swoop.Selection{Target: target, Changed: tfChanged, Dir: tfDir})
	if errors.Is(err, swoop.ErrNoMatches) && tfChanged != "" {
		fmt.Fprintln(os.Stderr, "no roots changed; nothing to do")
		return nil
	}
	if err != nil {
		return err
	}
	if matchType == swoop.MatchFuzzy {
		return fmt.Errorf("target %q only matched fuzzily; use an exact path, substring or glob in CI", target)
	}

	// Same resolution as `kest swoop`, under the ambient-credential policy: the resolved profile is echoed but never injected as AWS_PROFILE.
	r := swoop.Runner{
		Cfg:         cfg,
		BaseDir:     baseDir,
		Environment: environment,
		Policy: swoop.Policy{
			Ambient:      true,
			PrintContext: true,
			SavePlan:     tfSavePlan,
			Summary:      tfSummary,
			ApplyPlan:    tfApplyPlan,
			ExtraArgs:    extra,
		},
		BatchContext: true,
		Preflight:    ciVersionPreflight(action),
	}
	return r.Run(action, matches)
}

// ciVersionPreflight checks the root's pin without writing or installing anything.
// A mismatch fails apply, since applying with another version restamps state; init and plan only warn.
func ciVersionPreflight(action string) func(*swoop.Root, bool) error {
	return func(root *swoop.Root, batch bool) error {
		binary := cfg.TerraformCommand()
		check := swoop.CheckTFVersion(binary, cfg.TerraformVersionManager(), *root)
		if check.OK || check.Installed == "" {
			return nil
		}
		if action == "apply" {
			return fmt.Errorf("%s pins %s %s but %s is active; refusing to apply with a different version than the pin", root.Path, binary, check.Required, check.Installed)
		}
		indent := ""
		if batch {
			indent = "  "
		}
		fmt.Fprint(os.Stderr, swoop.VersionMismatchWarning(binary, *root, check, indent))
		return nil
	}
}

// ciResolveBaseDir determines the terraform discovery base directory.
func ciResolveBaseDir() (string, error) {
	if cfg != nil && cfg.Terraform.IACDir != "" {
		if cfg.Sources.Project != "" {
			projectRoot := filepath.Dir(cfg.Sources.Project)
			return filepath.Abs(filepath.Join(projectRoot, cfg.Terraform.IACDir))
		}
		return filepath.Abs(cfg.Terraform.IACDir)
	}
	if cfg != nil && cfg.Sources.Project != "" {
		return filepath.Abs(filepath.Dir(cfg.Sources.Project))
	}
	return os.Getwd()
}

var (
	tfSavePlan  bool
	tfSummary   bool
	tfApplyPlan bool
	tfChanged   string
	tfDir       string
)

func init() {
	for _, c := range []*cobra.Command{tfInitCmd, tfPlanCmd, tfApplyCmd} {
		c.Flags().StringVar(&tfChanged, "changed", "", "only roots whose config changed vs the merge-base with main/master, or vs a ref (--changed=origin/main); needs git history")
		c.Flags().Lookup("changed").NoOptDefVal = "true"
		c.Flags().StringVar(&tfDir, "dir", "", "only roots under this top-level directory")
	}
	tfPlanCmd.Flags().BoolVar(&tfSavePlan, "out", false, "save the plan to .terraform/kest.tfplan for apply --plan")
	tfPlanCmd.Flags().BoolVar(&tfSummary, "summary", false, "print a resource-level summary instead of the full plan (implies --out)")
	tfApplyCmd.Flags().BoolVar(&tfApplyPlan, "plan", false, "apply the saved plan from plan --out instead of re-planning")

	rootCmd.AddCommand(tfInitCmd)
	rootCmd.AddCommand(tfPlanCmd)
	rootCmd.AddCommand(tfApplyCmd)
}
