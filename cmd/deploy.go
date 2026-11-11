package cmd

import (
	"fmt"
	"log/slog"
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
)

var deployCmd = &cobra.Command{
	Use:   "deploy [app] [-- extra args]",
	Short: "Deploy an app (helm chart or raw manifests) to its target cluster",
	Long: `Deploy an app defined under deploys: in .kestconfig. Each deploy is a
helm chart (chart:) or a directory of raw manifests (manifests:); kest picks
the executor automatically. The deploy's target determines the cluster — no
-e flag needed. Works against any cluster: named kube contexts, an explicit
kubeconfig, or EKS.

Use --all to deploy every app (optionally filtered by --target), --diff for a
read-only preview (kubectl diff / helm --dry-run), and --force to bypass guards.
Args after -- are passed through to helm/kubectl.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if deployAllApps {
			return deployAllAppsRun(args)
		}
		if len(args) == 0 {
			return fmt.Errorf("app name required (or use --all)\navailable: %v", cfg.DeployNames())
		}
		return deploySingleApp(args[0], args[1:])
	},
	ValidArgsFunction: completeDeployNames,
	GroupID:           "deploy",
}

func deploySingleApp(name string, extra []string) error {
	d, ok := cfg.Deploys[name]
	if !ok {
		return fmt.Errorf("deploy %q not found in .kestconfig\navailable: %v", name, cfg.DeployNames())
	}
	if err := d.Validate(name); err != nil {
		return err
	}

	res, err := deploy.Resolve(cfg, d.Target)
	if err != nil {
		return err
	}
	if err := ensureSSOSession(res.AwsProfile); err != nil {
		return err
	}

	action := deploy.ActionApply
	if deployDiff {
		action = deploy.ActionDiff
	}

	// Guards apply to mutating applies only; --diff is read-only. --force bypasses.
	if action == deploy.ActionApply && !force {
		if err := guard.CheckCI(); err != nil {
			return err
		}
		if err := guard.CheckCleanWorktree(); err != nil {
			return err
		}
		if err := guard.CheckBranch(d.Target); err != nil {
			return err
		}
	}

	if action == deploy.ActionApply {
		if err := runDeployScripts(name, cfg.EffectiveDeployScriptsForDeploy(d)); err != nil {
			return err
		}
	}

	slog.Info("deploying app", "app", name, "kind", d.Kind(), "target", d.Target, "action", action)
	_, err = deploy.Execute(cfg, name, d, res, action, deploy.Policy{PrintContext: true, Wait: waitTimeout()}, extra)
	return err
}

func deployAllAppsRun(extra []string) error {
	names := cfg.DeployNames()
	if deployTargetFltr != "" {
		names = cfg.DeploysForTarget(deployTargetFltr)
	}
	if len(names) == 0 {
		return fmt.Errorf("no deploys found")
	}

	slog.Info("batch deploying apps", "count", len(names), "apps", names)
	fmt.Fprintf(os.Stderr, "info: deploying %d app(s): %v\n", len(names), names)

	for _, name := range names {
		if err := deploySingleApp(name, extra); err != nil {
			return fmt.Errorf("deploy %s: %w", name, err)
		}
	}
	return nil
}

func runDeployScripts(name string, scripts []string) error {
	for _, script := range scripts {
		slog.Info("running deploy script", "deploy", name, "script", script)
		fmt.Fprintf(os.Stderr, "info: running deploy script %s\n", script)
		if err := runner.Run("bash", script); err != nil {
			return fmt.Errorf("deploy script %s failed: %w", script, err)
		}
	}
	return nil
}

func init() {
	deployCmd.Flags().BoolVar(&deployAllApps, "all", false, "deploy all configured apps")
	deployCmd.Flags().StringVar(&deployTargetFltr, "target", "", "filter apps by target (used with --all)")
	deployCmd.Flags().BoolVar(&deployDiff, "diff", false, "preview changes (kubectl diff / helm --dry-run) without applying")
	deployCmd.Flags().BoolVar(&deployWait, "wait", false, "after a manifest apply, wait for each Deployment/StatefulSet/DaemonSet rollout and print its images")
	deployCmd.Flags().DurationVar(&deployWaitTO, "wait-timeout", 5*time.Minute, "per-workload rollout timeout for --wait")
	rootCmd.AddCommand(deployCmd)
}

// waitTimeout is the rollout wait for deploy.Policy; zero when --wait is unset.
func waitTimeout() time.Duration {
	if !deployWait {
		return 0
	}
	return deployWaitTO
}
