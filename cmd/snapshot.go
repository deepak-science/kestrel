package cmd

import (
	"fmt"
	"time"

	"github.com/deepak-science/kestrel/internal/deploy"
	"github.com/spf13/cobra"
)

var snapshotTimeout time.Duration

var snapshotCmd = &cobra.Command{
	Use:   "snapshot <app>",
	Short: "Take a CSI VolumeSnapshot of every PVC in an app's namespace",
	Long: `Create a VolumeSnapshot (snapshot.storage.k8s.io/v1) of every bound PVC in
the app's namespace and wait until each is readyToUse.

The namespace is the deploy's namespace:, or for manifest deploys without one,
the namespaces of the manifests' existing objects. Each PVC gets the deploy's
snapshot_class:, or the default (or only) VolumeSnapshotClass for its storage
driver. If any bound PVC can't be snapshotted, nothing is created.

Snapshots are labeled kest/deploy=<app>. kest doesn't delete old ones.`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeDeployNames,
	GroupID:           "deploy",
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
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
		return deploy.Snapshot(cfg, name, d, res, deploy.Policy{PrintContext: true}, snapshotTimeout)
	},
}

func init() {
	snapshotCmd.Flags().DurationVar(&snapshotTimeout, "timeout", 10*time.Minute, "how long to wait for snapshots to become readyToUse")
	rootCmd.AddCommand(snapshotCmd)
}
