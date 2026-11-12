package cmd

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/deepak-science/kestrel/internal/config"
	"github.com/deepak-science/kestrel/internal/deploy"
	"github.com/deepak-science/kestrel/internal/guard"
	"github.com/deepak-science/kestrel/internal/runner"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	deployAllApps    bool
	deployTargetFltr string
	deployDiff       bool
	deployWait       bool
	deployWaitTO     time.Duration
	deployForceConf  bool
	deploySnapshot   bool
	deployConfirm    bool
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
	if deployDiff && !deployConfirm {
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

	printContext := true
	if deployConfirm {
		apply, err := diffAndConfirm(name, d, res, extra)
		if err != nil || !apply {
			return err
		}
		printContext = false
	}

	if action == deploy.ActionApply {
		// Before deploy scripts, which may run migrations.
		if deploySnapshot {
			if err := deploy.Snapshot(cfg, name, d, res, deploy.Policy{}, snapshotTimeout); err != nil {
				return fmt.Errorf("pre-deploy snapshot: %w", err)
			}
		}
		if err := runDeployScripts(name, cfg.EffectiveDeployScriptsForDeploy(d)); err != nil {
			return err
		}
	}

	slog.Info("deploying app", "app", name, "kind", d.Kind(), "target", d.Target, "action", action)
	_, err = deploy.Execute(cfg, name, d, res, action, deploy.Policy{PrintContext: printContext, Wait: waitTimeout(), ForceConflicts: deployForceConf}, extra)
	return err
}

// diffAndConfirm runs the diff and asks before applying.
// A manifest diff with no differences skips the apply; helm's dry-run can't tell, so it always asks.
func diffAndConfirm(name string, d config.Deploy, res deploy.Resolution, extra []string) (bool, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return false, fmt.Errorf("--confirm needs a terminal to ask; run --diff, then deploy without it")
	}
	out, err := deploy.Execute(cfg, name, d, res, deploy.ActionDiff, deploy.Policy{PrintContext: true}, extra)
	if err != nil {
		return false, err
	}
	if d.Kind() == config.DeployManifest && out.ExitCode == 0 {
		fmt.Fprintf(os.Stderr, "%s: no changes, nothing to apply\n", name)
		return false, nil
	}

	fmt.Fprintf(os.Stderr, "\nApply %s to %s? [y/N] ", name, d.Target)
	var answer string
	_, _ = fmt.Scanln(&answer)
	answer = strings.TrimSpace(strings.ToLower(answer))
	if answer != "y" && answer != "yes" {
		fmt.Fprintf(os.Stderr, "%s: not applied\n", name)
		return false, nil
	}
	fmt.Fprintln(os.Stderr)
	return true, nil
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
	deployCmd.Flags().BoolVar(&deployWait, "wait", true, "after a manifest apply, wait for each Deployment/StatefulSet/DaemonSet rollout and print its images (--wait=false to skip)")
	deployCmd.Flags().BoolVar(&deployConfirm, "confirm", false, "show the diff, ask, then apply in one run (needs a terminal)")
	deployCmd.Flags().DurationVar(&deployWaitTO, "wait-timeout", 5*time.Minute, "per-workload rollout timeout for --wait")
	deployCmd.Flags().BoolVar(&deployForceConf, "force-conflicts", false, "server_side deploys: take ownership of fields another field manager holds, reverting live edits to them")
	deployCmd.Flags().BoolVar(&deploySnapshot, "snapshot", false, "before applying, snapshot every PVC in the app's namespace (see kest snapshot); abort if it fails")
	deployCmd.Flags().DurationVar(&snapshotTimeout, "snapshot-timeout", 10*time.Minute, "how long --snapshot waits for snapshots to become readyToUse")
	rootCmd.AddCommand(deployCmd)
}

// waitTimeout is the rollout wait for deploy.Policy; zero when --wait is unset.
func waitTimeout() time.Duration {
	if !deployWait {
		return 0
	}
	return deployWaitTO
}
