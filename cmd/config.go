package cmd

import (
	"fmt"
	"strings"

	"github.com/deepak-science/kestrel/internal/config"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var configCmd = &cobra.Command{
	Use:     "config",
	Short:   "Show and set up configuration",
	GroupID: "config",
}

var configPathsCmd = &cobra.Command{
	Use:   "paths",
	Short: "Show config file locations and which ones are loaded",
	RunE: func(cmd *cobra.Command, args []string) error {
		globalPath := config.GlobalConfigPath()

		fmt.Println("Config file locations:")
		fmt.Println()

		if cfg.Sources.Global != "" {
			fmt.Printf("  global:  %s (loaded)\n", cfg.Sources.Global)
		} else {
			fmt.Printf("  global:  %s (not found)\n", globalPath)
		}

		if cfg.Sources.Project != "" {
			fmt.Printf("  project: %s (loaded)\n", cfg.Sources.Project)
		} else {
			fmt.Println("  project: (none found)")
		}

		fmt.Println()
		fmt.Println("Merge order: global -> project (project overrides global)")

		return nil
	},
}

var configShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Show the resolved (merged) configuration",
	RunE: func(cmd *cobra.Command, args []string) error {
		out, err := yaml.Marshal(cfg)
		if err != nil {
			return fmt.Errorf("marshalling config: %w", err)
		}
		fmt.Print(string(out))
		return nil
	},
}

var configTargetsCmd = &cobra.Command{
	Use:     "targets",
	Aliases: []string{"envs"},
	Short:   "List configured targets",
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(cfg.Targets) == 0 {
			fmt.Println("No targets configured.")
			return nil
		}

		targetNames := cfg.TargetNames()
		for _, name := range targetNames {
			tc := cfg.Targets[name]
			parts := []string{name}

			if tc.Cluster != "" {
				parts = append(parts, fmt.Sprintf("cluster=%s", tc.Cluster))
			}

			if resolved, err := cfg.ResolveTarget(name); err == nil {
				if resolved.KubeContext != "" {
					parts = append(parts, fmt.Sprintf("context=%s", resolved.KubeContext))
				}
				if resolved.AwsProfile != "" {
					parts = append(parts, fmt.Sprintf("aws=%s", resolved.AwsProfile))
				}
			} else {
				parts = append(parts, "(not fully configured)")
			}

			fmt.Println(strings.Join(parts, "  "))
		}

		return nil
	},
}

var configAccountsCmd = &cobra.Command{
	Use:   "accounts",
	Short: "List configured AWS account mappings",
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(cfg.AWS.Accounts) == 0 {
			fmt.Println("No accounts configured.")
			return nil
		}

		for id, acct := range cfg.AWS.Accounts {
			fmt.Printf("%s  aws_profile=%s\n", id, acct.AwsProfile)
		}
		return nil
	},
}

func init() {
	configCmd.AddCommand(configPathsCmd)
	configCmd.AddCommand(configShowCmd)
	configCmd.AddCommand(configTargetsCmd)
	configCmd.AddCommand(configAccountsCmd)
	rootCmd.AddCommand(configCmd)
}
