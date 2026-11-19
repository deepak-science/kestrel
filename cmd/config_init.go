package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/deepak-science/kestrel/internal/config"
	"github.com/deepak-science/kestrel/internal/swoop"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var configInitForce bool

const configInitLong = `Walks the project and writes infrastructure facts to .kestconfig:

  - terraform.iac_dir   the directory containing the terraform roots
  - terraform.command   tofu, when roots pin via .opentofu-version
  - directories         dir → AWS account, for dirs that map to one account
  - targets             one per env, when the …/live/<env>/ convention is used
  - deploys             helm charts and manifest dirs found in the repo

An existing .kestconfig is merged into, not replaced; --force overwrites its mappings.
Nothing is written until you confirm the preview.

Machine-side access (AWS profiles, kube contexts) lives in the global config: see 'kest config autoconfigure'.`

var configInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Set up this project: detect terraform roots and deploys, write .kestconfig",
	Long:  configInitLong,
	RunE:  runConfigInit,
}

var swoopSetupCmd = &cobra.Command{
	Use:        "setup",
	Short:      "Alias for 'kest config init'",
	Long:       configInitLong,
	Hidden:     true,
	Deprecated: "use 'kest config init'",
	RunE:       runConfigInit,
}

func init() {
	for _, c := range []*cobra.Command{configInitCmd, swoopSetupCmd} {
		c.Flags().BoolVar(&configInitForce, "force", false, "overwrite existing mappings in .kestconfig")
	}
	configCmd.AddCommand(configInitCmd)
	swoopCmd.AddCommand(swoopSetupCmd)
}

func runConfigInit(cmd *cobra.Command, args []string) error {
	// Step 1: Discover roots from project root (not iac_dir).
	projectRoot, err := resolveProjectRoot()
	if err != nil {
		return err
	}

	roots, err := swoop.Discover(projectRoot)
	if err != nil {
		return fmt.Errorf("discovering roots: %w", err)
	}
	// Step 2: find iac_dir. layout.IACDir is set only for the …/live/<env>/
	// convention, where downstream joins iac_dir/live/<target>; otherwise it's
	// the roots' common ancestor.
	layout := swoop.DetectLayout(roots)
	iacDir := layout.IACDir
	if iacDir == "" {
		iacDir = commonRootDir(roots)
	}

	fmt.Printf("Found %d terraform root(s)\n", len(roots))
	switch {
	case len(roots) == 0:
	case iacDir != "":
		fmt.Printf("terraform.iac_dir: %s\n", iacDir)
	default:
		fmt.Println("terraform.iac_dir: (repo root)")
	}
	if layout.Type == "service" {
		fmt.Printf("Env targets (live/<env>): %s\n", strings.Join(layout.EnvNames, ", "))
	}

	// Step 3: Inspect roots for account IDs.
	dirInfos := swoop.InspectDirs(roots, projectRoot)
	swoop.EnrichWithAccountIDs(roots, projectRoot)

	for _, p := range dirInfos {
		if len(p.AccountIDs) > 0 {
			fmt.Printf("  Account IDs in %s: %s\n", p.Name, strings.Join(p.AccountIDs, ", "))
		}
	}

	// Build per-root account ID and region maps for service repos.
	// For service layout, the env dir is the part after "live/" in the root path.
	rootAccountIDs := make(map[string]string) // env name → account ID
	rootRegions := make(map[string]string)    // env name → AWS region
	for _, r := range roots {
		envName := layout.EnvNameFromRoot(r)
		if envName == "" {
			continue
		}
		if r.AccountID != "" {
			if _, exists := rootAccountIDs[envName]; !exists {
				rootAccountIDs[envName] = r.AccountID
			}
		}
		if _, exists := rootRegions[envName]; !exists {
			if region := swoop.ExtractRegion(r.AbsPath); region != "" {
				rootRegions[envName] = region
			}
		}
	}

	// Step 4: Build proposed config.
	proposed := &config.Config{}
	proposed.Terraform.IACDir = iacDir

	// Seed a target per env under the …/live/<env>/ convention.
	if layout.Type == "service" {
		proposed.Targets = make(map[string]config.TargetConfig)
		for _, envName := range layout.EnvNames {
			proposed.Targets[envName] = config.TargetConfig{
				AWSAccount: rootAccountIDs[envName],
				Region:     rootRegions[envName],
			}
		}
	}

	// dir → account, only where unambiguous: a multi-account dir would override
	// per-root resolution (AWSProfileForRoot checks Directories first).
	dirs := make(map[string]string)
	for _, p := range dirInfos {
		if len(p.AccountIDs) == 1 {
			dirs[p.Name] = p.AccountIDs[0]
		}
	}
	if len(dirs) > 0 {
		proposed.Directories = dirs
	}

	targetHint := mapKeys(proposed.Targets)
	if len(targetHint) == 0 && cfg != nil {
		targetHint = cfg.TargetNames()
	}
	if deploys := detectDeploys(projectRoot, targetHint); len(deploys) > 0 {
		proposed.Deploys = deploys
	}
	if len(roots) == 0 && len(proposed.Deploys) == 0 {
		return fmt.Errorf("nothing to set up: no terraform roots, charts or manifest dirs found under %s", projectRoot)
	}

	if usesOpenTofu(roots) {
		proposed.Terraform.Command = "tofu"
		fmt.Println("Detected: OpenTofu (.opentofu-version) → terraform.command: tofu")
	}

	if cfg != nil && len(proposed.Targets) == 0 && deploysNeedTarget(proposed.Deploys) {
		if len(cfg.Kubernetes.Contexts) > 0 {
			picked, err := promptDeployTargets()
			if err != nil {
				return err
			}
			if len(picked) > 0 {
				proposed.Targets = picked
				bindSoleTarget(proposed.Deploys, picked)
			}
		} else {
			fmt.Println("\nDetected deploys but no kube contexts are configured.")
			fmt.Println("Run 'kest config autoconfigure' to register contexts, then add a target.")
		}
	}

	// Determine project config path and load existing.
	configPath, existing, err := resolveProjectConfigPath(projectRoot)
	if err != nil {
		return err
	}

	// Merge with existing project config if present.
	if existing != nil {
		proposed = mergeSetupResult(existing, proposed, configInitForce)
	}

	// Step 5: Preview and confirm.
	for {
		fmt.Println("\nProposed .kestconfig:")
		fmt.Println(strings.Repeat("─", 40))
		out, _ := yaml.Marshal(proposed)
		fmt.Print(string(out))
		fmt.Println(strings.Repeat("─", 40))
		fmt.Printf("Path: %s\n", configPath)
		if n := countUnboundDeploys(proposed.Deploys); n > 0 {
			fmt.Printf("Note: %d deploy(s) have no target, and kest deploy refuses those. Choose edit to set one.\n", n)
		}

		cm := confirmModel{}
		result, err := runTUI(cm)
		if err != nil {
			return err
		}
		choice := result.(confirmModel).choice

		switch choice {
		case choiceCancel:
			fmt.Println("Cancelled.")
			return nil
		case choiceEdit:
			edited, err := editConfigInEditor(proposed)
			if err != nil {
				return err
			}
			if edited == nil {
				fmt.Println("Editor returned empty file, keeping previous version.")
				continue
			}
			proposed = edited
			continue
		case choiceWrite:
			// fall through
		}
		break
	}

	if err := config.WriteToPath(proposed, configPath); err != nil {
		return err
	}
	fmt.Printf("\nProject config written to %s\n", configPath)

	fmt.Println("\nTo configure access credentials (aws_profile, kube_context), run:")
	fmt.Println("  kest config autoconfigure")
	return nil
}

// resolveProjectRoot returns the project root directory.
func resolveProjectRoot() (string, error) {
	if cfg != nil && cfg.Sources.Project != "" {
		return filepath.Abs(filepath.Dir(cfg.Sources.Project))
	}
	return os.Getwd()
}

// resolveProjectConfigPath determines where to write the .kestconfig and
// loads any existing config from that path.
func resolveProjectConfigPath(projectRoot string) (string, *config.Config, error) {
	if cfg != nil && cfg.Sources.Project != "" {
		existing, err := config.LoadFromPath(cfg.Sources.Project)
		if err != nil {
			return "", nil, fmt.Errorf("reading existing .kestconfig: %w", err)
		}
		return cfg.Sources.Project, existing, nil
	}

	path := filepath.Join(projectRoot, ".kestconfig")
	return path, nil, nil
}

func editConfigInEditor(c *config.Config) (*config.Config, error) {
	data, err := yaml.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("marshalling config: %w", err)
	}

	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = os.Getenv("VISUAL")
	}
	if editor == "" {
		editor = "vi"
	}

	tmpDir, err := os.MkdirTemp("", "kest-swoop-setup-*")
	if err != nil {
		return nil, fmt.Errorf("creating temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	tmpFile := filepath.Join(tmpDir, ".kestconfig")
	if err := os.WriteFile(tmpFile, data, 0o644); err != nil {
		return nil, fmt.Errorf("writing temp file: %w", err)
	}

	fmt.Println("\nOpening config in editor for review...")
	editorCmd := exec.Command(editor, tmpFile)
	editorCmd.Stdin = os.Stdin
	editorCmd.Stdout = os.Stdout
	editorCmd.Stderr = os.Stderr
	if err := editorCmd.Run(); err != nil {
		return nil, fmt.Errorf("editor exited with error: %w", err)
	}

	edited, err := os.ReadFile(tmpFile)
	if err != nil {
		return nil, fmt.Errorf("reading edited file: %w", err)
	}
	if len(strings.TrimSpace(string(edited))) == 0 {
		return nil, nil
	}

	var result config.Config
	if err := yaml.Unmarshal(edited, &result); err != nil {
		return nil, fmt.Errorf("parsing edited config: %w", err)
	}
	return &result, nil
}

// detectDeploys proposes deploys: entries for the project — helm-chart deploys
// inferred from a values directory and manifest deploys from directories of raw
// kubernetes YAML. Detection is best-effort; the user reviews and edits the
// proposal before it's written. Returns nil when nothing is found.
func detectDeploys(projectRoot string, targetNames []string) map[string]config.Deploy {
	deploys := detectChartDeploys(projectRoot, targetNames)

	defaultTarget := ""
	if len(targetNames) == 1 {
		defaultTarget = targetNames[0]
	}
	for name, d := range detectManifestDeploys(projectRoot, defaultTarget) {
		if _, exists := deploys[name]; !exists {
			deploys[name] = d
		}
	}

	if len(deploys) == 0 {
		return nil
	}
	return deploys
}

// Directories detectDeploys scans, relative to the project root: chart
// directories (hold a Chart.yaml), values directories (hold env/instance value
// files), and manifest directories (hold raw kubernetes YAML).
var (
	chartCandidates    = []string{"charts/app", "charts", "chart", "misc/chart", "deploy/chart", "helm"}
	valuesCandidates   = []string{"deploys", "values", "charts", "misc/chart", "chart", "deploy/chart", "helm"}
	manifestCandidates = []string{"k8s-manifests", "k8s", "manifests", "kubernetes"}
)

// structuralChartFiles are value-dir entries that belong to the chart itself,
// not to a target or instance.
var structuralChartFiles = map[string]bool{"shared": true, "Chart": true, "values": true}

// detectChartDeploys infers helm-chart deploys from a values directory whose
// files follow the convention:
//
//	<target>.yaml              — target-level values
//	<target>-<instance>.yaml   — instance-specific values
//
// Each instance and each bare target-level file becomes one deploy. A file
// matching no known target stands alone, targeting the sole configured target
// when there is exactly one, else its own name. chart: is set to a detected
// local chart path when one exists, else left blank for the user to fill.
func detectChartDeploys(projectRoot string, targetNames []string) map[string]config.Deploy {
	deploys := make(map[string]config.Deploy)
	chart := detectLocalChart(projectRoot)

	defaultTarget := ""
	if len(targetNames) == 1 {
		defaultTarget = targetNames[0]
	}

	for _, dir := range valuesCandidates {
		entries, err := os.ReadDir(filepath.Join(projectRoot, dir))
		if err != nil {
			continue
		}

		var yamlFiles []string
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
				continue
			}
			if name := strings.TrimSuffix(e.Name(), ".yaml"); !structuralChartFiles[name] {
				yamlFiles = append(yamlFiles, name)
			}
		}
		if len(yamlFiles) == 0 {
			continue
		}

		targetSet := make(map[string]bool, len(targetNames))
		for _, t := range targetNames {
			targetSet[t] = true
		}

		// Files matching a target name exactly are target-level value layers.
		targetFiles := make(map[string]bool)
		for _, name := range yamlFiles {
			if targetSet[name] {
				targetFiles[name] = true
			}
		}

		valuesPath := func(file string) string { return filepath.Join(dir, file+".yaml") }
		targetsWithInstances := make(map[string]bool)

		for _, name := range yamlFiles {
			if targetFiles[name] {
				continue
			}
			matched := false
			for _, t := range targetNames {
				instance, ok := strings.CutPrefix(name, t+"-")
				if !ok || instance == "" {
					continue
				}
				targetsWithInstances[t] = true
				var values []string
				if targetFiles[t] {
					values = append(values, valuesPath(t))
				}
				values = append(values, valuesPath(name))
				deploys[instance] = config.Deploy{Chart: chart, Target: t, Values: values}
				matched = true
				break
			}
			// A file matching no known target stands alone.
			if !matched && !targetSet[name] {
				target := name
				if defaultTarget != "" {
					target = defaultTarget
				}
				deploys[name] = config.Deploy{Chart: chart, Target: target, Values: []string{valuesPath(name)}}
			}
		}

		// Targets with only a bare <target>.yaml get one deploy named after them.
		for _, t := range targetNames {
			if !targetsWithInstances[t] && targetFiles[t] {
				deploys[t] = config.Deploy{Chart: chart, Target: t, Values: []string{valuesPath(t)}}
			}
		}

		return deploys
	}
	// A chart with no per-target values files still deploys with its own defaults.
	if chart != "" {
		deploys[chartName(projectRoot, chart)] = config.Deploy{Chart: chart, Target: defaultTarget}
	}
	return deploys
}

// detectManifestDeploys infers manifest deploys from directories of raw
// kubernetes YAML. Each immediate subdirectory holding at least one manifest
// becomes a deploy (a leading NN- ordering prefix is stripped from the name).
// A candidate root holding manifests directly becomes a single deploy named
// after the root.
func detectManifestDeploys(projectRoot, target string) map[string]config.Deploy {
	deploys := make(map[string]config.Deploy)

	for _, root := range manifestCandidates {
		entries, err := os.ReadDir(filepath.Join(projectRoot, root))
		if err != nil {
			continue
		}

		rootHasManifests := false
		for _, e := range entries {
			if e.IsDir() {
				sub := filepath.Join(root, e.Name())
				if dirHasManifests(filepath.Join(projectRoot, sub)) {
					deploys[stripOrderPrefix(e.Name())] = config.Deploy{Manifests: sub, Target: target}
				}
				continue
			}
			if isManifestFile(e.Name()) {
				rootHasManifests = true
			}
		}
		if rootHasManifests {
			deploys[filepath.Base(root)] = config.Deploy{Manifests: root, Target: target}
		}
		if len(deploys) > 0 {
			return deploys
		}
	}
	return deploys
}

// detectLocalChart returns the first candidate directory containing a
// Chart.yaml, relative to the project root, or "" if none.
func detectLocalChart(projectRoot string) string {
	for _, dir := range chartCandidates {
		if _, err := os.Stat(filepath.Join(projectRoot, dir, "Chart.yaml")); err == nil {
			return dir
		}
	}
	return ""
}

// commonRootDir returns the directory containing all discovered roots — the
// common ancestor of their parent directories, relative to the project root.
// Returns "" when any root sits at the repo root (no narrower container exists).
func commonRootDir(roots []swoop.Root) string {
	var common []string
	for i, r := range roots {
		parent := filepath.Dir(r.Path)
		if parent == "." {
			return ""
		}
		segs := strings.Split(parent, string(filepath.Separator))
		if i == 0 {
			common = segs
			continue
		}
		common = commonPrefix(common, segs)
		if len(common) == 0 {
			return ""
		}
	}
	return filepath.Join(common...)
}

func commonPrefix(a, b []string) []string {
	n := min(len(a), len(b))
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return a[:i]
}

func deploysNeedTarget(deploys map[string]config.Deploy) bool {
	return countUnboundDeploys(deploys) > 0
}

func promptDeployTargets() (map[string]config.TargetConfig, error) {
	names := mapKeys(cfg.Kubernetes.Contexts)
	sort.Strings(names)

	items := make([]selectItem, len(names))
	for i, n := range names {
		items[i] = selectItem{
			name:    n,
			preview: fmt.Sprintf("%s = %s\n", acPreviewKey.Render("context"), cfg.Kubernetes.Contexts[n]),
		}
	}

	result, err := runTUI(multiSelectModel{
		title:    "Deploy targets to add to .kestconfig",
		items:    items,
		selected: make(map[int]bool),
	})
	if err != nil {
		return nil, err
	}
	ms := result.(multiSelectModel)
	if ms.cancelled {
		return nil, nil
	}

	targets := make(map[string]config.TargetConfig)
	for i, n := range names {
		if ms.selected[i] {
			targets[n] = config.TargetConfig{Cluster: n}
		}
	}
	return targets, nil
}

// usesOpenTofu reports whether any discovered root pins its version with an
// .opentofu-version file (OpenTofu's native convention), signalling that the
// repo drives tofu rather than terraform.
func usesOpenTofu(roots []swoop.Root) bool {
	for _, r := range roots {
		if _, err := os.Stat(filepath.Join(r.AbsPath, ".opentofu-version")); err == nil {
			return true
		}
	}
	return false
}

func mapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func dirHasManifests(path string) bool {
	entries, err := os.ReadDir(path)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && isManifestFile(e.Name()) {
			return true
		}
	}
	return false
}

func isManifestFile(name string) bool {
	return strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
}

// stripOrderPrefix drops a leading NN- ordering prefix (e.g. "01-authentik" →
// "authentik"), leaving the name unchanged when there is none.
func stripOrderPrefix(name string) string {
	i := 0
	for i < len(name) && name[i] >= '0' && name[i] <= '9' {
		i++
	}
	if i > 0 && i < len(name) && name[i] == '-' {
		return name[i+1:]
	}
	return name
}

// mergeSetupResult merges setup's proposed config into an existing config.
func mergeSetupResult(existing, proposed *config.Config, force bool) *config.Config {
	result := *existing

	// Set iac_dir if proposed and not already set (or force).
	if proposed.Terraform.IACDir != "" && (result.Terraform.IACDir == "" || force) {
		result.Terraform.IACDir = proposed.Terraform.IACDir
	}
	if proposed.Terraform.Command != "" && (result.Terraform.Command == "" || force) {
		result.Terraform.Command = proposed.Terraform.Command
	}

	// Merge deploys.
	if len(proposed.Deploys) > 0 {
		if result.Deploys == nil {
			result.Deploys = make(map[string]config.Deploy)
		}
		for name, d := range proposed.Deploys {
			if _, exists := result.Deploys[name]; !exists || force {
				result.Deploys[name] = d
			}
		}
	}

	// Merge targets.
	if result.Targets == nil {
		result.Targets = make(map[string]config.TargetConfig)
	}
	for name, tc := range proposed.Targets {
		if existing, exists := result.Targets[name]; !exists || force {
			result.Targets[name] = tc
		} else {
			// Enrich existing target with new fields if they were empty.
			if existing.AWSAccount == "" && tc.AWSAccount != "" {
				existing.AWSAccount = tc.AWSAccount
			}
			if existing.Region == "" && tc.Region != "" {
				existing.Region = tc.Region
			}
			result.Targets[name] = existing
		}
	}

	// Merge directories.
	if result.Directories == nil && len(proposed.Directories) > 0 {
		result.Directories = make(map[string]string)
	}
	for name, accountID := range proposed.Directories {
		if _, exists := result.Directories[name]; !exists || force {
			result.Directories[name] = accountID
		}
	}

	return &result
}

// chartName returns the name: from the chart's Chart.yaml, falling back to its directory name.
func chartName(projectRoot, chart string) string {
	var meta struct {
		Name string `yaml:"name"`
	}
	if data, err := os.ReadFile(filepath.Join(projectRoot, chart, "Chart.yaml")); err == nil {
		if yaml.Unmarshal(data, &meta) == nil && meta.Name != "" {
			return meta.Name
		}
	}
	return filepath.Base(chart)
}

// bindSoleTarget points target-less deploys at the only target.
// With several targets the choice is the user's, so they stay unbound.
func bindSoleTarget(deploys map[string]config.Deploy, targets map[string]config.TargetConfig) {
	if len(targets) != 1 {
		return
	}
	target := mapKeys(targets)[0]
	for name, d := range deploys {
		if d.Target == "" {
			d.Target = target
			deploys[name] = d
		}
	}
}

func countUnboundDeploys(deploys map[string]config.Deploy) int {
	n := 0
	for _, d := range deploys {
		if d.Target == "" {
			n++
		}
	}
	return n
}
