package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/deepak-science/kestrel/internal/config"
	"github.com/deepak-science/kestrel/internal/execlog"
	"github.com/deepak-science/kestrel/internal/guard"
	"github.com/deepak-science/kestrel/internal/logging"
	"github.com/spf13/cobra"
)

var (
	environment    string
	verbose        bool
	cfg            *config.Config
	logCleanup     func()
	execlogCleanup func()
)

var rootCmd = &cobra.Command{
	Use:   "kestci",
	Short: "Kestrel CI — deterministic helm & terraform orchestration for CI/CD",
	Long: `A non-interactive CLI for CI/CD pipelines that reads .kestconfig and
executes deploy and terraform commands. Credentials and kubeconfig come from
the environment (OIDC, env vars, ambient ~/.kube/config).`,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		// Args are validated by now; later errors are runtime failures, not misuse.
		cmd.SilenceUsage = true

		cleanup, err := logging.Init()
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not initialize logging: %v\n", err)
		} else {
			logCleanup = cleanup
		}

		elCleanup, err := execlog.Init()
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not initialize exec log: %v\n", err)
		} else {
			execlogCleanup = elCleanup
		}

		cfg, err = config.Load()
		if err != nil {
			return err
		}

		slog.Debug("config loaded", "iac_dir", cfg.Terraform.IACDir, "deploys", len(cfg.Deploys))
		if verbose {
			fmt.Fprintf(os.Stderr, "debug: config loaded (iac_dir=%s, deploys=%d)\n",
				cfg.Terraform.IACDir, len(cfg.Deploys))
		}

		slog.Info("kestci invoked",
			"command", cmd.CommandPath(),
			"args", args,
			"environment", environment,
			"ci", guard.IsCI(),
		)

		return nil
	},
	PersistentPostRun: func(cmd *cobra.Command, args []string) {
		if execlogCleanup != nil {
			execlogCleanup()
		}
		if logCleanup != nil {
			logCleanup()
		}
	},
}

// requireCI returns an error if not running in a CI environment.
// kestci is meant for CI only — use kest for local development.
func requireCI() error {
	if !guard.IsCI() {
		return fmt.Errorf("kestci is designed for CI/CD pipelines; use 'kest' for local development\n" +
			"  Set CI=true to override (e.g. for local testing of CI workflows)")
	}
	return nil
}

// SetBuildInfo wires build-time version/commit/date into the root command,
// surfaced via `kestci --version`. Stamped by hanko via ldflags at build time.
func SetBuildInfo(version, commit, date string) {
	rootCmd.Version = fmt.Sprintf("%s (commit %s, built %s)", version, commit, date)
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&environment, "environment", "e", "", "target environment (required)")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "enable verbose/debug output")
}
