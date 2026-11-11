package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/deepak-science/kestrel/internal/guard"
	"github.com/deepak-science/kestrel/internal/swoop"
	"github.com/spf13/cobra"
)

var tfInitCmd = &cobra.Command{
	Use:   "init <path>",
	Short: "Run terraform init in a root",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runTerraformAction("init", args[0])
	},
}

var tfPlanCmd = &cobra.Command{
	Use:   "plan <path>",
	Short: "Run terraform plan in a root",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runTerraformAction("plan", args[0])
	},
}

var tfApplyCmd = &cobra.Command{
	Use:   "apply <path>",
	Short: "Run terraform apply in a root",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runTerraformAction("apply", args[0])
	},
}

func runTerraformAction(action, target string) error {
	if err := requireCI(); err != nil {
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

	// Resolve target — exact path or glob only, no fuzzy matching.
	matches, matchType := swoop.ResolveWithType(roots, target)
	if len(matches) == 0 {
		return fmt.Errorf("no roots matching %q", target)
	}
	if matchType == swoop.MatchFuzzy {
		return fmt.Errorf("ambiguous target %q — use an exact path or glob in CI", target)
	}

	// Execute each matched root.
	if len(matches) == 1 {
		return executeCIAction(action, matches[0], baseDir)
	}

	// Batch mode for globs.
	fmt.Fprintf(os.Stderr, "Running terraform %s on %d root(s)...\n\n", action, len(matches))
	var failed int
	for i, root := range matches {
		fmt.Fprintf(os.Stderr, "━━━ [%d/%d] %s ━━━\n", i+1, len(matches), root.Path)
		if err := executeCIAction(action, root, baseDir); err != nil {
			fmt.Fprintf(os.Stderr, "  FAILED: %v\n", err)
			failed++
		}
		fmt.Fprintln(os.Stderr)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d root(s) failed", failed, len(matches))
	}
	return nil
}

func executeCIAction(action string, root swoop.Root, baseDir string) error {
	// Same resolution as `kest swoop` (incl. the S3-backend assume_role
	// fallback), but executed under the ambient-credential policy: CI gets its
	// credentials from the environment (OIDC / env vars), so the resolved
	// profile is echoed for legibility but never injected as AWS_PROFILE.
	res := swoop.EffectiveProfiles(cfg, root, environment)
	_, err := swoop.Execute(cfg, root, baseDir, action, res, swoop.Policy{
		Ambient:      true,
		PrintContext: true,
		SavePlan:     tfSavePlan,
		Summary:      tfSummary,
		ApplyPlan:    tfApplyPlan,
	})
	if err != nil {
		return fmt.Errorf("terraform %s failed: %w", action, err)
	}
	return nil
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
)

func init() {
	tfPlanCmd.Flags().BoolVar(&tfSavePlan, "out", false, "save the plan to .terraform/kest.tfplan for apply --plan")
	tfPlanCmd.Flags().BoolVar(&tfSummary, "summary", false, "print a resource-level summary instead of the full plan (implies --out)")
	tfApplyCmd.Flags().BoolVar(&tfApplyPlan, "plan", false, "apply the saved plan from plan --out instead of re-planning")

	rootCmd.AddCommand(tfInitCmd)
	rootCmd.AddCommand(tfPlanCmd)
	rootCmd.AddCommand(tfApplyCmd)
}
