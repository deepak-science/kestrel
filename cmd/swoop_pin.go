package cmd

import (
	"fmt"
	"os"

	"github.com/deepak-science/kestrel/internal/swoop"
	"github.com/spf13/cobra"
)

var swoopPinVersion string

var swoopPinCmd = &cobra.Command{
	Use:   "pin [target]",
	Short: "Set the terraform/tofu version pin in target root(s)",
	Long: `Write the version-pin file (.opentofu-version or .terraform-version) in
each matching root, replacing any existing pin.

The version defaults to the active terraform/tofu binary; set one with
--version. With no target, every root is pinned. Target, --dir and
--changed select roots as for plan/apply.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		target := ""
		if len(args) > 0 {
			target = args[0]
		}
		return runSwoopPin(target)
	},
}

func runSwoopPin(target string) error {
	baseDir, err := resolveBaseDir()
	if err != nil {
		return err
	}
	roots, err := discoverRoots(baseDir)
	if err != nil {
		return err
	}
	if len(roots) == 0 {
		return fmt.Errorf("no terraform roots found under %s", baseDir)
	}
	// No target and no filters pins every root, where plan/apply would call that ambiguous.
	matches := roots
	if target != "" || swoopChanged != "" || swoopActionDir != "" {
		var matchType swoop.MatchType
		matches, matchType, err = swoop.Select(roots, baseDir, swoop.Selection{Target: target, Changed: swoopChanged, Dir: swoopActionDir})
		if err != nil {
			return err
		}
		if matchType == swoop.MatchFuzzy && !confirmFuzzyMatch(matches, target) {
			return fmt.Errorf("aborted")
		}
	}

	command, manager := cfg.TerraformCommand(), cfg.TerraformVersionManager()
	version := swoopPinVersion
	if version == "" {
		// Detect outside any root so a version manager can't report the old pin back.
		if version, err = swoop.ActiveTFVersion(command, os.TempDir()); err != nil {
			return err
		}
	}

	for _, root := range matches {
		if root.TFVersion == version {
			fmt.Fprintf(os.Stderr, "%s: already %s\n", root.Path, version)
			continue
		}
		file, err := swoop.PinTFVersion(root, manager, version)
		if err != nil {
			return fmt.Errorf("%s: %w", root.Path, err)
		}
		from := root.TFVersion
		if from == "" {
			from = "unpinned"
		}
		fmt.Fprintf(os.Stderr, "%s: %s %s → %s\n", root.Path, file, from, version)
	}

	if dv := cfg.Terraform.DefaultVersion; cfg.Terraform.WriteVersion && dv != "" && dv != version {
		fmt.Fprintf(os.Stderr, "\nnote: terraform.default_version is %s, so roots without a pin will still get %s\n", dv, dv)
	}
	return nil
}

func init() {
	swoopPinCmd.Flags().StringVar(&swoopPinVersion, "version", "", "version to pin (default: the active binary's)")
	swoopCmd.AddCommand(swoopPinCmd)
}
