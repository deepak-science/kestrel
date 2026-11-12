package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"text/tabwriter"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/deepak-science/kestrel/internal/swoop"
	"github.com/spf13/cobra"
)

var swoopDir string

var swoopCmd = &cobra.Command{
	Use:     "swoop",
	Short:   "Discover and operate on terraform roots",
	GroupID: "deploy",
	Long: `Find, list, and run terraform commands across all roots in a project.

Running without a subcommand opens an interactive picker to browse and
select a terraform root. Use arrow keys or type to filter, then press
enter to plan, ctrl+a to apply, or ctrl+i to init.`,
	RunE: runSwoopInteractive,
}

var swoopListCmd = &cobra.Command{
	Use:   "list [target]",
	Short: "List discovered terraform roots",
	Long: `Discover and display all terraform roots in the project.

Optionally provide a target to filter results (exact path, glob, or substring).
Use --dir to filter by top-level directory.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		baseDir, err := resolveBaseDir()
		if err != nil {
			return err
		}

		roots, err := discoverRoots(baseDir)
		if err != nil {
			return err
		}

		if len(roots) == 0 {
			fmt.Fprintf(os.Stderr, "No terraform roots found under %s\n", baseDir)
			return nil
		}

		// Apply filters.
		if swoopDir != "" {
			roots = swoop.ResolveByDir(roots, swoopDir)
		}
		if len(args) > 0 {
			roots = swoop.Resolve(roots, args[0])
		}

		if len(roots) == 0 {
			fmt.Fprintln(os.Stderr, "No matching roots found.")
			return nil
		}

		// Load local state for recency info.
		state, _ := swoop.LoadState(baseDir)

		// Sort: roots with recent activity first, then alphabetical.
		sortRoots(roots, state)

		printRootTable(roots, state)
		return nil
	},
}

// resolveBaseDir determines the directory to scan for terraform roots.
// It uses terraform.iac_dir from config if set, otherwise the project root
// (directory containing .kestconfig), otherwise cwd.
func resolveBaseDir() (string, error) {
	// If terraform.iac_dir is configured, use it relative to the project config location.
	if cfg != nil && cfg.Terraform.IACDir != "" {
		if cfg.Sources.Project != "" {
			projectRoot := filepath.Dir(cfg.Sources.Project)
			return filepath.Abs(filepath.Join(projectRoot, cfg.Terraform.IACDir))
		}
		return filepath.Abs(cfg.Terraform.IACDir)
	}

	// If a .kestconfig exists, use its directory as the base.
	if cfg != nil && cfg.Sources.Project != "" {
		return filepath.Abs(filepath.Dir(cfg.Sources.Project))
	}

	// Walk up looking for .git as a repo root signal.
	if root, err := findRepoRoot(); err == nil {
		return root, nil
	}

	// Fall back to cwd.
	return os.Getwd()
}

// findRepoRoot walks up from cwd looking for a .git directory.
func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no .git found")
		}
		dir = parent
	}
}

// discoverRoots discovers terraform roots and enriches them with account IDs,
// git status, and .tf modification times.
func discoverRoots(baseDir string) ([]swoop.Root, error) {
	roots, err := swoop.Discover(baseDir)
	if err != nil {
		return nil, fmt.Errorf("discovering roots: %w", err)
	}
	if len(roots) > 0 {
		swoop.EnrichWithAccountIDs(roots, baseDir)
		swoop.EnrichGitStatus(roots, baseDir)
		swoop.EnrichTFMtimes(roots)
	}
	return roots, nil
}

func sortRoots(roots []swoop.Root, state *swoop.State) {
	order := ""
	if cfg != nil {
		order = cfg.Swoop.SortOrder
	}

	switch order {
	case "alpha":
		sort.Slice(roots, func(i, j int) bool {
			return roots[i].Path < roots[j].Path
		})
	case "recent":
		sort.Slice(roots, func(i, j int) bool {
			return compareRecent(roots[i], roots[j], state)
		})
	default: // "" or "git" — git-first is the default.
		sort.Slice(roots, func(i, j int) bool {
			return compareGitFirst(roots[i], roots[j], state)
		})
	}
}

// compareGitFirst sorts: dirty first (by tf mtime), then by activity, then by tf mtime, then alpha.
func compareGitFirst(a, b swoop.Root, state *swoop.State) bool {
	// Dirty roots sort before clean roots.
	if a.GitDirty != b.GitDirty {
		return a.GitDirty
	}

	// Among same dirty status: prefer roots with recent terraform activity.
	ta := lastActivity(state, a.Path)
	tb := lastActivity(state, b.Path)
	if !ta.IsZero() && tb.IsZero() {
		return true
	}
	if ta.IsZero() && !tb.IsZero() {
		return false
	}
	if !ta.IsZero() && !tb.IsZero() && !ta.Equal(tb) {
		return ta.After(tb)
	}

	// Then by .tf file modification time.
	if !a.TFModified.IsZero() && !b.TFModified.IsZero() && !a.TFModified.Equal(b.TFModified) {
		return a.TFModified.After(b.TFModified)
	}
	if !a.TFModified.IsZero() && b.TFModified.IsZero() {
		return true
	}
	if a.TFModified.IsZero() && !b.TFModified.IsZero() {
		return false
	}

	return a.Path < b.Path
}

// compareRecent sorts by terraform activity recency, then alphabetical.
func compareRecent(a, b swoop.Root, state *swoop.State) bool {
	ta := lastActivity(state, a.Path)
	tb := lastActivity(state, b.Path)

	if !ta.IsZero() && tb.IsZero() {
		return true
	}
	if ta.IsZero() && !tb.IsZero() {
		return false
	}
	if !ta.IsZero() && !tb.IsZero() && !ta.Equal(tb) {
		return ta.After(tb)
	}
	return a.Path < b.Path
}

func printRootTable(roots []swoop.Root, state *swoop.State) {
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ROOT\tDIR\tAWS\tTF VERSION\tINIT\tDIRTY\tLAST ACTIVITY\tMODIFIED")

	for _, r := range roots {
		init := "-"
		if r.Initialized {
			init = "yes"
		}

		ver := r.TFVersion
		if ver == "" {
			ver = "-"
		}

		dirty := ""
		if r.GitDirty {
			dirty = "*"
		}

		activity := lastActivityStr(state, r.Path)

		// Effective profile the executor would use, incl. backend fallback.
		aws := swoop.EffectiveProfiles(cfg, r, environment).Effective
		if aws == "" {
			aws = "-"
		}

		modified := "-"
		if !r.TFModified.IsZero() {
			modified = relativeTime(r.TFModified)
		}

		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Path, r.Dir, aws, ver, init, dirty, activity, modified)
	}
	_ = w.Flush()

	fmt.Fprintf(os.Stderr, "\n%d root(s) found\n", len(roots))
}

func runSwoopInteractive(cmd *cobra.Command, args []string) error {
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

	state, _ := swoop.LoadState(baseDir)
	sortRoots(roots, state)

	m := newSwoopTUI(roots, state)
	p := tea.NewProgram(m, tea.WithAltScreen())
	result, err := p.Run()
	if err != nil {
		return err
	}

	action := result.(swoopTUIModel).result
	if action.action == "" {
		return nil // cancelled
	}

	fmt.Fprintf(os.Stderr, "\n%s\n\n",
		swoopHelpStyle.Render(
			fmt.Sprintf("hint: kest swoop %s %s", action.action, action.root.Path)))

	switch action.action {
	case "edit":
		return executeEdit(action.root)
	case "cd":
		return executeCD(action.root)
	default:
		return runSwoopActionCmd(action.action, action.root.Path, nil)
	}
}

func init() {
	swoopListCmd.Flags().StringVar(&swoopDir, "dir", "", "filter by top-level directory")
	swoopListCmd.ValidArgsFunction = completeSwoopRoots
	_ = swoopListCmd.RegisterFlagCompletionFunc("dir", completeSwoopDirs)

	swoopCmd.AddCommand(swoopListCmd)
	rootCmd.AddCommand(swoopCmd)
}
