package config

import (
	"fmt"
	"os"
	"path"
	"path/filepath"

	"github.com/adrg/xdg"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Helm       HelmConfig       `yaml:"helm,omitempty"`
	Terraform  TerraformConfig  `yaml:"terraform,omitempty"`
	Swoop      SwoopConfig      `yaml:"swoop,omitempty"`
	AWS        AWSConfig        `yaml:"aws,omitempty"`
	Kubernetes KubernetesConfig `yaml:"kubernetes,omitempty"`

	// Targets are named deployment targets for helm (project config, committed).
	// Each maps a name (= values file) to a cluster.
	Targets map[string]TargetConfig `yaml:"targets,omitempty"`

	// Directories maps top-level directory names to AWS account IDs (project config).
	// Used for centralized IaC repos where auto-discovery might miss some dirs.
	Directories map[string]string `yaml:"directories,omitempty"`

	// Deploys are named app deploys (project config). Each deploys one app to a
	// target via a helm chart or a directory of raw manifests; the executor is
	// inferred from which source is set. See internal/deploy.
	Deploys map[string]Deploy `yaml:"deploys,omitempty"`

	// Raw layers preserved for source-aware commands (not serialised).
	ProjectTargets map[string]TargetConfig `yaml:"-"`
	Sources        Sources                 `yaml:"-"`
}

// TargetConfig defines a deployment target in project config.
type TargetConfig struct {
	Cluster    string `yaml:"cluster,omitempty"`
	AWSAccount string `yaml:"aws_account,omitempty"` // 12-digit AWS account ID
	Region     string `yaml:"region,omitempty"`      // AWS region (e.g. us-east-1)

	// Kubeconfig is an optional explicit kubeconfig file (path relative to the
	// project root), e.g. one written by a terraform output. Empty uses the
	// ambient kubeconfig ($KUBECONFIG / ~/.kube/config).
	Kubeconfig string `yaml:"kubeconfig,omitempty"`
}

// Deploy defines a single named app deploy. Exactly one source must be set:
// Chart (helm executor) or Manifests (kubectl executor); see Kind.
type Deploy struct {
	// --- helm source (Chart set) ---
	// Chart is a helm chart: a local path (charts/app), an OCI ref
	// (oci://ghcr.io/org/chart), or — with Repo — a chart name in a repo.
	Chart string `yaml:"chart,omitempty"`
	// Repo is a helm chart repository URL (passed as --repo). Optional; used
	// with a bare chart name for third-party charts not referenced by OCI.
	Repo string `yaml:"repo,omitempty"`
	// Version pins the chart version (--version). Optional.
	Version string `yaml:"version,omitempty"`
	// ReleaseName overrides the helm release name. Empty defaults to the
	// deploy's key in the deploys map.
	ReleaseName string `yaml:"release_name,omitempty"`
	// Set holds inline --set key=value overrides. Optional.
	Set map[string]string `yaml:"set,omitempty"`

	// --- manifest source (Manifests set) ---
	// Manifests is a directory or file applied with `kubectl apply -f`.
	Manifests string `yaml:"manifests,omitempty"`
	// ServerSide applies with `kubectl apply --server-side --field-manager=kest`.
	ServerSide bool `yaml:"server_side,omitempty"`

	// --- common ---
	// Target is the deployment target (key in targets:). Required.
	Target string `yaml:"target"`
	// Namespace overrides the namespace (helm --namespace / kubectl
	// --namespace). Empty lets the chart/manifests carry their own.
	Namespace string `yaml:"namespace,omitempty"`
	// Values lists helm values files (paths relative to the project root),
	// layered in order. Helm-only.
	Values []string `yaml:"values,omitempty"`
	// DeployScripts overrides the top-level helm.deploy_scripts for this
	// deploy. nil = inherit; [] = skip.
	DeployScripts *[]string `yaml:"deploy_scripts,omitempty"`
	// SnapshotClass is the VolumeSnapshotClass for `kest snapshot` / `deploy --snapshot`.
	// Empty picks, per PVC, the default (or only) class for its storage driver.
	SnapshotClass string `yaml:"snapshot_class,omitempty"`
	// SnapshotSkip lists PVCs to leave out of snapshots, as globs on the PVC name or namespace/name.
	SnapshotSkip []string `yaml:"snapshot_skip,omitempty"`
}

// Deploy executor kinds.
const (
	DeployHelm     = "helm"
	DeployManifest = "manifest"
)

// Kind reports which executor a deploy uses: DeployHelm when Chart is set,
// DeployManifest when Manifests is set, or "" when the deploy is invalid
// (neither or both sources set). Validate surfaces the invalid case.
func (d Deploy) Kind() string {
	switch {
	case d.Chart != "" && d.Manifests == "":
		return DeployHelm
	case d.Manifests != "" && d.Chart == "":
		return DeployManifest
	default:
		return ""
	}
}

// Validate returns an error if the deploy is not exactly one of helm/manifest
// or is missing a target. name is the deploy's key, for the message.
func (d Deploy) Validate(name string) error {
	switch {
	case d.Chart == "" && d.Manifests == "":
		return fmt.Errorf("deploy %q: set either chart: (helm) or manifests: (kubectl)", name)
	case d.Chart != "" && d.Manifests != "":
		return fmt.Errorf("deploy %q: set only one of chart: or manifests:, not both", name)
	case d.Target == "":
		return fmt.Errorf("deploy %q: target is required", name)
	case d.ServerSide && d.Chart != "":
		return fmt.Errorf("deploy %q: server_side applies to manifests: deploys only", name)
	}
	for _, pat := range d.SnapshotSkip {
		if _, err := path.Match(pat, ""); err != nil {
			return fmt.Errorf("deploy %q: snapshot_skip %q: %w", name, pat, err)
		}
	}
	return nil
}

// AWSConfig holds AWS-specific user configuration.
type AWSConfig struct {
	// Accounts maps AWS account IDs to access credentials (user config).
	Accounts map[string]AWSAccountConfig `yaml:"accounts,omitempty"`

	// AutoSSOLogin enables automatic aws sso login when a session is expired.
	AutoSSOLogin bool `yaml:"auto_sso_login,omitempty"`
}

// AWSAccountConfig maps an AWS account ID to access credentials.
type AWSAccountConfig struct {
	AwsProfile string `yaml:"aws_profile,omitempty"`
}

// KubernetesConfig holds Kubernetes-specific user configuration.
type KubernetesConfig struct {
	// Contexts maps cluster names to kube context strings (user config).
	// If empty, cluster names are matched against kubeconfig at autoconfigure time.
	Contexts map[string]string `yaml:"contexts,omitempty"`
}

// Sources records the file paths that contributed to the loaded config.
type Sources struct {
	Global  string // XDG config path (may be empty if not found)
	Project string // project-level .kestconfig (may be empty if not found)
}

// HelmConfig holds helm settings shared across deploys.
type HelmConfig struct {
	// DeployScripts run before a deploy; deploys inherit these unless they set
	// their own deploy_scripts.
	DeployScripts []string `yaml:"deploy_scripts,omitempty"`
}

type TerraformConfig struct {
	IACDir string `yaml:"iac_dir,omitempty"`

	// Command is the terraform-compatible CLI to invoke (e.g. "terraform"
	// or "tofu"). Empty defaults to "terraform". Overridden at runtime by
	// the $KEST_TERRAFORM_COMMAND environment variable.
	Command string `yaml:"command,omitempty"`

	// VersionManager is the version-manager CLI kest uses for
	// .terraform-version handling: "tfenv", "tofuenv", or "off" to disable
	// kest's version-manager integration entirely. Empty auto-detects:
	// "tofuenv" when Command is "tofu", else "tfenv". Overridden by
	// $KEST_TERRAFORM_VERSION_MANAGER.
	VersionManager string `yaml:"version_manager,omitempty"`

	// WriteVersion writes a .terraform-version file into roots that lack
	// one, pinning DefaultVersion or the currently active terraform version.
	WriteVersion bool `yaml:"write_version,omitempty"`

	// DefaultVersion is the version written to .terraform-version when
	// WriteVersion is enabled. If empty, the currently active terraform
	// version is detected and used instead.
	DefaultVersion string `yaml:"default_version,omitempty"`

	// AutoInstallPinned automatically installs the pinned terraform
	// version (from .terraform-version) via the configured VersionManager
	// when a mismatch is detected, without prompting. Skipped in CI.
	AutoInstallPinned bool `yaml:"auto_install_pinned,omitempty"`
}

// SwoopConfig holds user preferences for the swoop subsystem.
type SwoopConfig struct {
	// CDMode controls which shell command swoop cd emits: "cd" (default) or "pushd".
	CDMode string `yaml:"cd_mode,omitempty"`

	// Editor overrides $EDITOR for swoop edit. Empty means use $EDITOR.
	Editor string `yaml:"editor,omitempty"`

	// SortOrder controls root ordering: "recent" (default) or "alpha".
	SortOrder string `yaml:"sort_order,omitempty"`
}

// ResolvedTarget holds the resolved access methods for a target.
type ResolvedTarget struct {
	KubeContext string
	AwsProfile  string
	AccountID   string // raw AWS account ID (for CI, swoop bridging)
	Region      string // from target config
	Cluster     string // from target config
}

const (
	appName        = "kest"
	configFileName = ".kestconfig"
	globalFileName = "config.yaml"
)

// globalPathOverride, when non-empty, replaces the XDG-derived global config path.
var globalPathOverride string

// SetGlobalConfigPath overrides the default global config path.
// Pass "" to reset to the default XDG path.
func SetGlobalConfigPath(path string) {
	globalPathOverride = path
}

// GlobalConfigPath returns the expected path for the global config file:
// $XDG_CONFIG_HOME/kest/config.yaml (typically ~/.config/kest/config.yaml),
// unless overridden via SetGlobalConfigPath.
func GlobalConfigPath() string {
	if globalPathOverride != "" {
		return globalPathOverride
	}
	return filepath.Join(xdg.ConfigHome, appName, globalFileName)
}

// Load reads the global XDG config (user access methods) and the project-level
// .kestconfig (infrastructure facts), composing them into a resolved config.
func Load() (*Config, error) {
	globalPath := GlobalConfigPath()
	user, err := loadFile(globalPath)
	if err != nil {
		return nil, fmt.Errorf("loading global config: %w", err)
	}

	projectPath, _ := findProjectConfig()
	project, err := loadFile(projectPath)
	if err != nil {
		return nil, fmt.Errorf("loading project config: %w", err)
	}

	out := compose(user, project)

	// Record which files actually existed.
	if fileExists(globalPath) {
		out.Sources.Global = globalPath
	}
	if projectPath != "" && fileExists(projectPath) {
		out.Sources.Project = projectPath
	}

	return out, nil
}

func findProjectConfig() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}

	for {
		candidate := filepath.Join(dir, configFileName)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no %s found", configFileName)
		}
		dir = parent
	}
}

func loadFile(path string) (*Config, error) {
	if path == "" {
		return &Config{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{}, nil
		}
		return nil, err
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &cfg, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// compose combines user (global) and project configs into a resolved Config.
//
// Project provides: targets, directories, helm, terraform settings.
// User provides: accounts, contexts.
func compose(user, project *Config) *Config {
	out := *user

	// Helm deploy scripts come from project config.
	if len(project.Helm.DeployScripts) > 0 {
		out.Helm.DeployScripts = project.Helm.DeployScripts
	}

	// Terraform: IACDir comes from project; behaviour flags come from user
	// (already in out) but project can override non-zero values.
	if project.Terraform.IACDir != "" {
		out.Terraform.IACDir = project.Terraform.IACDir
	}
	if project.Terraform.Command != "" {
		out.Terraform.Command = project.Terraform.Command
	}
	if project.Terraform.VersionManager != "" {
		out.Terraform.VersionManager = project.Terraform.VersionManager
	}
	if project.Terraform.DefaultVersion != "" {
		out.Terraform.DefaultVersion = project.Terraform.DefaultVersion
	}

	// Targets come from project config.
	if len(project.Targets) > 0 {
		out.Targets = project.Targets
	}
	out.ProjectTargets = project.Targets

	// Directories come from project config.
	if len(project.Directories) > 0 {
		out.Directories = project.Directories
	}

	// Deploys come from project config.
	if len(project.Deploys) > 0 {
		out.Deploys = project.Deploys
	}

	// AWS and Kubernetes come from user config (already in out via *user).

	return &out
}

// ResolveTarget resolves a target name to kube_context + aws_profile.
// The target must exist in the Targets map.
// Kube context is resolved from Contexts map by cluster name.
// AWS profile is resolved from the target's explicit aws_account field,
// falling back to extracting account ID from the kube context ARN.
func (c *Config) ResolveTarget(name string) (ResolvedTarget, error) {
	target, ok := c.Targets[name]
	if !ok {
		return ResolvedTarget{}, fmt.Errorf("target %q not configured (check your .kestconfig targets)", name)
	}

	var resolved ResolvedTarget
	resolved.Cluster = target.Cluster
	resolved.Region = target.Region

	if target.Cluster != "" {
		resolved.KubeContext = c.ResolveClusterContext(target.Cluster)
		if resolved.KubeContext == "" {
			return ResolvedTarget{}, fmt.Errorf(
				"target %q has cluster %q but no kube context configured\n"+
					"  Run 'kest config autoconfigure' or add a contexts entry for %q to %s",
				name, target.Cluster, target.Cluster, GlobalConfigPath())
		}
	}

	// AWS profile: explicit account takes priority over ARN extraction.
	if target.AWSAccount != "" {
		resolved.AccountID = target.AWSAccount
		resolved.AwsProfile = c.ResolveAccountProfile(target.AWSAccount)
	} else if resolved.KubeContext != "" {
		if accountID := ExtractAccountIDFromARN(resolved.KubeContext); accountID != "" {
			resolved.AccountID = accountID
			resolved.AwsProfile = c.ResolveAccountProfile(accountID)
		}
	}

	return resolved, nil
}

// TerraformCommand returns the terraform-compatible CLI to invoke.
// Resolution order: $KEST_TERRAFORM_COMMAND → cfg.Terraform.Command → "terraform".
// Safe to call on a nil receiver.
func (c *Config) TerraformCommand() string {
	if env := os.Getenv("KEST_TERRAFORM_COMMAND"); env != "" {
		return env
	}
	if c != nil && c.Terraform.Command != "" {
		return c.Terraform.Command
	}
	return "terraform"
}

// TerraformVersionManager returns the version-manager CLI kest uses for
// .terraform-version handling. Possible return values:
//   - "off": user explicitly disabled version-manager integration
//   - "tfenv" / "tofuenv" / other: the CLI to invoke
//
// Resolution order: $KEST_TERRAFORM_VERSION_MANAGER → cfg.Terraform.VersionManager
// → auto-detect ("tofuenv" if the resolved terraform command is "tofu",
// else "tfenv"). Auto-detect respects $KEST_TERRAFORM_COMMAND so an env
// override of the command also flips the manager default.
// Safe to call on a nil receiver.
func (c *Config) TerraformVersionManager() string {
	if env := os.Getenv("KEST_TERRAFORM_VERSION_MANAGER"); env != "" {
		return env
	}
	if c != nil && c.Terraform.VersionManager != "" {
		return c.Terraform.VersionManager
	}
	if c.TerraformCommand() == "tofu" {
		return "tofuenv"
	}
	return "tfenv"
}

// ResolveAccountProfile resolves an account ID to an AWS profile name.
func (c *Config) ResolveAccountProfile(accountID string) string {
	if acct, ok := c.AWS.Accounts[accountID]; ok {
		return acct.AwsProfile
	}
	return ""
}

// ResolveClusterContext resolves a cluster name to a kube context string.
func (c *Config) ResolveClusterContext(cluster string) string {
	if ctx, ok := c.Kubernetes.Contexts[cluster]; ok {
		return ctx
	}
	return ""
}

// HasProjectTargets returns true if the config was loaded with project-level
// target definitions (i.e. a .kestconfig with targets).
func (c *Config) HasProjectTargets() bool {
	return len(c.ProjectTargets) > 0
}

// TargetNames returns a sorted list of target names.
func (c *Config) TargetNames() []string {
	return sortedKeys(c.Targets)
}

// DeployNames returns a sorted list of deploy keys.
func (c *Config) DeployNames() []string {
	return sortedKeys(c.Deploys)
}

// DeploysForTarget returns deploy keys that target the given target name,
// sorted.
func (c *Config) DeploysForTarget(target string) []string {
	var names []string
	for k, d := range c.Deploys {
		if d.Target == target {
			names = append(names, k)
		}
	}
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	return names
}

// EffectiveDeployScriptsForDeploy returns the deploy scripts for a deploy,
// falling back to the top-level HelmConfig scripts when the deploy does not
// override them (nil = inherit, [] = skip).
func (c *Config) EffectiveDeployScriptsForDeploy(d Deploy) []string {
	if d.DeployScripts != nil {
		return *d.DeployScripts
	}
	return c.Helm.DeployScripts
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// ExtractAccountIDFromARN extracts the AWS account ID from an EKS ARN.
// e.g. "arn:aws:eks:us-east-1:111122223333:cluster/eks-dev" → "111122223333"
// Returns empty string if the context is not an ARN.
func ExtractAccountIDFromARN(ctx string) string {
	// ARN format: arn:partition:service:region:account-id:resource
	if len(ctx) < 4 || ctx[:4] != "arn:" {
		return ""
	}
	parts := splitN(ctx, ':', 6)
	if len(parts) < 5 {
		return ""
	}
	accountID := parts[4]
	if len(accountID) == 12 {
		return accountID
	}
	return ""
}

func splitN(s string, sep byte, n int) []string {
	var parts []string
	for i := 0; i < n-1; i++ {
		idx := -1
		for j := 0; j < len(s); j++ {
			if s[j] == sep {
				idx = j
				break
			}
		}
		if idx < 0 {
			break
		}
		parts = append(parts, s[:idx])
		s = s[idx+1:]
	}
	parts = append(parts, s)
	return parts
}

const maxBackups = 3

// rotateBackups shifts existing .bak files up and copies the current file
// to .bak. Files beyond maxBackups are removed.
func rotateBackups(path string) {
	if _, err := os.Stat(path); err != nil {
		return // nothing to back up
	}

	// Remove oldest if it exists.
	oldest := fmt.Sprintf("%s.bak.%d", path, maxBackups)
	_ = os.Remove(oldest)

	// Shift .bak.N → .bak.N+1
	for i := maxBackups - 1; i >= 1; i-- {
		src := fmt.Sprintf("%s.bak.%d", path, i)
		dst := fmt.Sprintf("%s.bak.%d", path, i+1)
		_ = os.Rename(src, dst)
	}

	// .bak → .bak.1
	_ = os.Rename(path+".bak", path+".bak.1")

	// current → .bak
	data, err := os.ReadFile(path)
	if err == nil {
		_ = os.WriteFile(path+".bak", data, 0o644)
	}
}

// WriteGlobal writes the given config to the global config path,
// creating parent directories as needed. The previous file is rotated
// into .bak, .bak.1, .bak.2, etc.
func WriteGlobal(cfg *Config) error {
	path := GlobalConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	rotateBackups(path)

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshalling config: %w", err)
	}

	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// WriteToPath writes the given config to an arbitrary path with backup rotation.
func WriteToPath(cfg *Config, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	rotateBackups(path)

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshalling config: %w", err)
	}

	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// LoadFromPath reads a config from a specific file path.
func LoadFromPath(path string) (*Config, error) {
	return loadFile(path)
}

// LoadGlobal reads only the global config file (no project merge).
func LoadGlobal() (*Config, error) {
	return loadFile(GlobalConfigPath())
}
