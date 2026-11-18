package main

import (
	"errors"
	"fmt"

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
	Long: `The CI sibling of 'kest drift', with ambient credentials. Nothing is
applied and plans don't take the state lock. A fresh checkout needs --init.

Exits 0 when everything is clean, 2 when anything drifted, 1 when a check failed.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		err := ciDrift()
		if errors.Is(err, swoop.ErrChanges) {
			cmd.SilenceErrors = true
		}
		return err
	},
}

func ciDrift() error {
	if err := requireCI(); err != nil {
		return err
	}
	if err := driftFilter.Validate(); err != nil {
		return err
	}
	baseDir, err := ciResolveBaseDir()
	if err != nil {
		return err
	}
	var roots []swoop.Root
	if driftFilter.WantRoots() {
		if roots, err = swoop.Discover(baseDir); err != nil {
			return fmt.Errorf("discovering roots: %w", err)
		}
		swoop.EnrichWithAccountIDs(roots, baseDir)
	}
	driftFilter.CI = true
	sel, err := drift.Select(cfg, roots, driftFilter)
	if err != nil {
		return err
	}

	items := drift.Sweep{
		Cfg:         cfg,
		BaseDir:     baseDir,
		Environment: environment,
		Ambient:     true,
		Init:        driftInit,
		Parallel:    driftParallel,
	}.Run(sel.Roots, sel.Deploys)
	return drift.Report(append(items, sel.Skipped...), driftJSON)
}

func init() {
	driftCmd.Flags().StringVar(&driftFilter.Only, "only", "", "check only terraform roots or only deploys (terraform|deploys)")
	driftCmd.Flags().StringVar(&driftFilter.Dir, "dir", "", "only roots under this top-level directory")
	driftCmd.Flags().StringVar(&driftFilter.Target, "target", "", "only deploys to this target")
	driftCmd.Flags().StringArrayVar(&driftFilter.ExcludeRoots, "exclude-root", nil, "skip this root, or every root under this directory, on top of drift.exclude_roots (repeatable)")
	driftCmd.Flags().StringArrayVar(&driftFilter.ExcludeDeploys, "exclude-deploy", nil, "skip this deploy, on top of drift.exclude_deploys (repeatable)")
	driftCmd.Flags().BoolVar(&driftInit, "init", false, "run terraform init on each root first, one at a time")
	driftCmd.Flags().IntVar(&driftParallel, "parallel", 4, "checks to run at once")
	driftCmd.Flags().BoolVar(&driftJSON, "json", false, "print results as JSON instead of a table")
	rootCmd.AddCommand(driftCmd)
}
