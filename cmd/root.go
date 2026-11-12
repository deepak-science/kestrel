package cmd

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/deepak-science/kestrel/internal/awslogin"
	"github.com/deepak-science/kestrel/internal/config"
	"github.com/deepak-science/kestrel/internal/execlog"
	"github.com/deepak-science/kestrel/internal/guard"
	"github.com/deepak-science/kestrel/internal/logging"
	"github.com/deepak-science/kestrel/internal/profile"
	"github.com/spf13/cobra"
)

var (
	environment      string
	verbose          bool
	force            bool
	globalConfigPath string
	cfg              *config.Config
	logCleanup       func()
	execlogCleanup   func()
)

var rootCmd = &cobra.Command{
	Use:   "kest",
	Short: "Kestrel — unified helm & terraform orchestration",
	Long:  "A CLI that wraps Helm and Terraform workflows per-project, driven by .kestconfig files.",
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		// Args are validated by now; later errors are runtime failures, not misuse.
		cmd.SilenceUsage = true

		// Set up file logging (non-fatal if it fails)
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

		if globalConfigPath != "" {
			config.SetGlobalConfigPath(globalConfigPath)
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

		// Fall back to active profile when no -e flag (skip in CI)
		if environment == "" && !guard.IsCI() {
			active, err := profile.Read()
			if err == nil && active != "" {
				environment = active
				slog.Debug("using active profile", "profile", active)
				if verbose {
					fmt.Fprintf(os.Stderr, "debug: using active profile %q\n", active)
				}
			}
		}

		slog.Info("kest invoked",
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

// ensureSSOSession checks the AWS session for the given profile and runs
// sso login if needed. No-op when auto_sso_login is off or running in CI.
func ensureSSOSession(profile string) error {
	if cfg == nil || !cfg.AWS.AutoSSOLogin || guard.IsCI() {
		return nil
	}
	return awslogin.EnsureSession(profile)
}

// SetBuildInfo wires build-time version/commit/date into the root command,
// surfaced via `kest --version`. Stamped by hanko via ldflags at build time.
func SetBuildInfo(version, commit, date string) {
	rootCmd.Version = fmt.Sprintf("%s (commit %s, built %s)", version, commit, date)
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func init() {
	rootCmd.AddGroup(
		&cobra.Group{ID: "deploy", Title: "Deploy Commands:"},
		&cobra.Group{ID: "config", Title: "Configuration:"},
	)

	rootCmd.PersistentFlags().StringVarP(&environment, "environment", "e", "", "target environment (dev, stage, prod)")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "enable verbose/debug output")
	rootCmd.PersistentFlags().BoolVar(&force, "force", false, "bypass all safety guards (CI-only, clean worktree, branch restrictions)")
	rootCmd.PersistentFlags().StringVar(&globalConfigPath, "config", "", "override global config path (default: ~/.config/kest/config.yaml)")

	_ = rootCmd.RegisterFlagCompletionFunc("environment", completeTargetNames)
}
