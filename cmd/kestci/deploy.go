package main

import (
	"fmt"
	"os"
	"time"

	"github.com/deepak-science/kestrel/internal/deploy"
	"github.com/deepak-science/kestrel/internal/guard"
	"github.com/deepak-science/kestrel/internal/runner"
	"github.com/spf13/cobra"
)

var (
	deployAllApps    bool
	deployTargetFltr string
	deployDiff       bool
	deployWait       bool
	deployWaitTO     time.Duration
	deployForceConf  bool
	deploySnapshot   bool
	deploySnapTO     time.Duration
)

var deployCmd = &cobra.Command{
	Use:   "deploy [app] [-- extra args]",
	Short: "Deploy an app (helm chart or raw manifests) to its target cluster",
	Long: `Deploy an app defined under deploys: in .kestconfig, the non-interactive
sibling of 'kest deploy'. Same resolution; credentials and kubeconfig come from
the environment (ambient). Guards are always enforced — there is no --force.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := requireCI(); err != nil {
			return err
		}
		if deployAllApps {
			return ciDeployAllApps(args)
		}
		if len(args) == 0 {
			return fmt.Errorf("app name required (or use --all)\navailable: %v", cfg.DeployNames())
		}
		return ciDeploySingleApp(args[0], args[1:])
	},
}

func ciDeploySingleApp(name string, extra []string) error {
	d, ok := cfg.Deploys[name]
	if !ok {
		return fmt.Errorf("deploy %q not found in .kestconfig\navailable: %v", name, cfg.DeployNames())
	}
	if err := d.Validate(name); err != nil {
		return err
	}

	action := deploy.ActionApply
	if deployDiff {
		action = deploy.ActionDiff
	}

	// Guards always enforced in CI (no --force); read-only diff skips them.
	if action == deploy.ActionApply {
		if err := guard.CheckCleanWorktree(); err != nil {
			return err
		}
		if err := guard.CheckBranch(d.Target); err != nil {
			return err
		}
	}

	res, err := deploy.Resolve(cfg, d.Target)
	if err != nil {
		return err
	}

	if action == deploy.ActionApply {
		// Before deploy scripts, which may run migrations.
		if deploySnapshot {
			if err := deploy.Snapshot(cfg, name, d, res, deploy.Policy{Ambient: true}, deploySnapTO); err != nil {
				return fmt.Errorf("pre-deploy snapshot: %w", err)
			}
		}
		for _, script := range cfg.EffectiveDeployScriptsForDeploy(d) {
			fmt.Fprintf(os.Stderr, "info: running deploy script %s\n", script)
			if err := runner.Run("bash", script); err != nil {
				return fmt.Errorf("deploy script %s failed: %w", script, err)
			}
		}
	}

	_, err = deploy.Execute(cfg, name, d, res, action, deploy.Policy{
		Ambient:        true,
		PrintContext:   true,
		Wait:           waitTimeout(),
		ForceConflicts: deployForceConf,
	}, extra)
	return err
}

func ciDeployAllApps(extra []string) error {
	names := cfg.DeployNames()
	if deployTargetFltr != "" {
		names = cfg.DeploysForTarget(deployTargetFltr)
	}
	if len(names) == 0 {
		return fmt.Errorf("no deploys found")
	}

	fmt.Fprintf(os.Stderr, "info: deploying %d app(s): %v\n", len(names), names)
	for _, name := range names {
		if err := ciDeploySingleApp(name, extra); err != nil {
			return fmt.Errorf("deploy %s: %w", name, err)
		}
	}
	return nil
}

func init() {
	deployCmd.Flags().BoolVar(&deployAllApps, "all", false, "deploy all configured apps")
	deployCmd.Flags().StringVar(&deployTargetFltr, "target", "", "filter apps by target (used with --all)")
	deployCmd.Flags().BoolVar(&deployDiff, "diff", false, "preview changes (kubectl diff / helm --dry-run) without applying")
	deployCmd.Flags().BoolVar(&deployWait, "wait", true, "after a manifest apply, wait for each Deployment/StatefulSet/DaemonSet rollout and print its images (--wait=false to skip)")
	deployCmd.Flags().DurationVar(&deployWaitTO, "wait-timeout", 5*time.Minute, "per-workload rollout timeout for --wait")
	deployCmd.Flags().BoolVar(&deployForceConf, "force-conflicts", false, "server_side deploys: take ownership of fields another field manager holds, reverting live edits to them")
	deployCmd.Flags().BoolVar(&deploySnapshot, "snapshot", false, "before applying, snapshot every PVC in the app's namespace; abort if it fails")
	deployCmd.Flags().DurationVar(&deploySnapTO, "snapshot-timeout", 10*time.Minute, "how long --snapshot waits for snapshots to become readyToUse")
	rootCmd.AddCommand(deployCmd)
}

// waitTimeout is the rollout wait for deploy.Policy; zero when --wait is unset.
func waitTimeout() time.Duration {
	if !deployWait {
		return 0
	}
	return deployWaitTO
}
