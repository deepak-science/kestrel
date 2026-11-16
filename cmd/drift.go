package cmd

import (
	"errors"

	"github.com/deepak-science/kestrel/internal/deploy"
	"github.com/deepak-science/kestrel/internal/drift"
	"github.com/deepak-science/kestrel/internal/swoop"
	"github.com/spf13/cobra"
)

var (
	driftFilter   drift.Filter
	driftInit     bool
	driftParallel int
	driftJSON     bool
)

var driftCmd = &cobra.Command{
	Use:   "drift",
	Short: "Check every terraform root and deploy for drift, read-only",
	Long: `Plan every terraform root and diff every deploy against the live cluster,
then print one table. Nothing is applied, and plans don't take the state lock.

Roots: plan -detailed-exitcode. Manifest deploys: kubectl diff, plus fields set
by live kubectl edits (edit, patch, scale, ...), which a re-apply doesn't
revert. Helm deploys: release status and chart version, then the same two
checks on the chart as a dry-run upgrade renders it.

Exits 0 when everything is clean, 2 when anything drifted, 1 when a check failed.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		err := runDrift()
		if errors.Is(err, swoop.ErrChanges) {
			cmd.SilenceErrors = true
		}
		return err
	},
	GroupID: "deploy",
}

func runDrift() error {
	if err := driftFilter.Validate(); err != nil {
		return err
	}
	baseDir, err := resolveBaseDir()
	if err != nil {
		return err
	}
	var roots []swoop.Root
	if driftFilter.WantRoots() {
		if roots, err = discoverRoots(baseDir); err != nil {
			return err
		}
	}
	roots, deploys, err := drift.Select(cfg, roots, driftFilter)
	if err != nil {
		return err
	}
	if err := driftLogins(roots, deploys); err != nil {
		return err
	}

	items := drift.Sweep{
		Cfg:         cfg,
		BaseDir:     baseDir,
		Environment: environment,
		Init:        driftInit,
		Parallel:    driftParallel,
	}.Run(roots, deploys)
	return drift.Report(items, driftJSON)
}

// driftLogins refreshes each SSO profile once, before parallel checks could each prompt.
func driftLogins(roots []swoop.Root, deploys []string) error {
	var profiles []string
	for _, r := range roots {
		res := swoop.EffectiveProfiles(cfg, r, environment)
		profiles = append(profiles, res.ProviderProfile, res.BackendProfile)
	}
	for _, name := range deploys {
		if res, err := deploy.Resolve(cfg, cfg.Deploys[name].Target); err == nil {
			profiles = append(profiles, res.AwsProfile)
		}
	}
	seen := map[string]bool{"": true}
	for _, p := range profiles {
		if seen[p] {
			continue
		}
		seen[p] = true
		if err := ensureSSOSession(p); err != nil {
			return err
		}
	}
	return nil
}

func init() {
	driftCmd.Flags().StringVar(&driftFilter.Only, "only", "", "check only terraform roots or only deploys (terraform|deploys)")
	driftCmd.Flags().StringVar(&driftFilter.Dir, "dir", "", "only roots under this top-level directory")
	driftCmd.Flags().StringVar(&driftFilter.Target, "target", "", "only deploys to this target")
	driftCmd.Flags().BoolVar(&driftInit, "init", false, "run terraform init on each root first, one at a time")
	driftCmd.Flags().IntVar(&driftParallel, "parallel", 4, "checks to run at once")
	driftCmd.Flags().BoolVar(&driftJSON, "json", false, "print results as JSON instead of a table")
	rootCmd.AddCommand(driftCmd)
}
