package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/deepak-science/kestrel/internal/awsconfig"
	"github.com/deepak-science/kestrel/internal/config"
	"github.com/deepak-science/kestrel/internal/kubeconfig"
	"github.com/deepak-science/kestrel/internal/swoop"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var configAutoconfigureCmd = &cobra.Command{
	Use:   "autoconfigure",
	Short: "Set up this machine: map AWS profiles and kube contexts, write the global config",
	Long: `Reads AWS profiles from ~/.aws/config and kube contexts from
~/.kube/config, then maps them to kest accounts and clusters.

When a project .kestconfig defines targets (with cluster names), autoconfigure
matches kube contexts to those clusters.

When directories or targets reference AWS accounts, autoconfigure matches
AWS profiles by sso_account_id.

Writes only the global config. To scaffold a project's .kestconfig, see 'kest config init'.`,
	RunE: runAutoconfigure,
}

func init() {
	configCmd.AddCommand(configAutoconfigureCmd)
}

// collectAccountIDsFromProfiles extracts unique sso_account_id values from
// AWS profiles. This is project-independent — it looks at your ~/.aws/config.
func collectAccountIDsFromProfiles(profiles []awsconfig.Profile) []string {
	seen := make(map[string]bool)
	var ids []string
	for _, p := range profiles {
		ssoID := profileField(p, "sso_account_id")
		if ssoID != "" && !seen[ssoID] {
			seen[ssoID] = true
			ids = append(ids, ssoID)
		}
	}
	sort.Strings(ids)
	return ids
}

// profilesForAccount returns all AWS profiles that have the given sso_account_id.
func profilesForAccount(profiles []awsconfig.Profile, accountID string) []awsconfig.Profile {
	var matches []awsconfig.Profile
	for _, p := range profiles {
		if profileField(p, "sso_account_id") == accountID {
			matches = append(matches, p)
		}
	}
	return matches
}

func runAutoconfigure(cmd *cobra.Command, args []string) error {
	// Read both sources.
	profiles, err := awsconfig.ReadProfileDetails()
	if err != nil {
		return fmt.Errorf("reading AWS profiles: %w", err)
	}

	kubeContexts, kubeErr := kubeconfig.ReadContexts()

	if len(profiles) == 0 && (kubeErr != nil || len(kubeContexts) == 0) {
		return fmt.Errorf("no AWS profiles or kube contexts found")
	}

	if len(profiles) > 0 {
		names := make([]string, len(profiles))
		for i, p := range profiles {
			names[i] = p.Name
		}
		fmt.Printf("Found %d AWS profile(s): %s\n", len(profiles), strings.Join(names, ", "))
	}

	if kubeErr != nil {
		fmt.Printf("Note: could not read kubeconfig: %v\n", kubeErr)
	} else if len(kubeContexts) > 0 {
		names := make([]string, len(kubeContexts))
		for i, c := range kubeContexts {
			names[i] = kubeconfig.ShortName(c.Name)
		}
		fmt.Printf("Found %d kube context(s): %s\n", len(kubeContexts), strings.Join(names, ", "))
	}
	fmt.Println()

	// Load existing global config to preserve fields.
	global, err := config.LoadGlobal()
	if err != nil {
		global = &config.Config{}
	}
	if global.AWS.Accounts == nil {
		global.AWS.Accounts = make(map[string]config.AWSAccountConfig)
	}
	if global.Kubernetes.Contexts == nil {
		global.Kubernetes.Contexts = make(map[string]string)
	}

	// --- AWS profiles: one prompt per unique account ID found in ~/.aws/config ---
	accountIDs := collectAccountIDsFromProfiles(profiles)
	if len(accountIDs) > 0 {
		fmt.Println("Matching AWS profiles to accounts...")
		fmt.Println()

		for _, accountID := range accountIDs {
			matching := profilesForAccount(profiles, accountID)
			matchNames := make([]string, len(matching))
			for i, p := range matching {
				matchNames[i] = p.Name
			}

			fmt.Printf("Account %s  (profiles: %s)\n", accountID, strings.Join(matchNames, ", "))

			if len(matching) == 1 {
				// Only one profile for this account — use it automatically.
				global.AWS.Accounts[accountID] = config.AWSAccountConfig{
					AwsProfile: matching[0].Name,
				}
				fmt.Printf("  aws_profile: %s (auto, only match)\n", matching[0].Name)
				fmt.Println()
				continue
			}

			// Multiple profiles for this account — let user pick.
			preselect := 0

			// Check existing config.
			if acct, ok := global.AWS.Accounts[accountID]; ok && acct.AwsProfile != "" {
				for i, p := range matching {
					if p.Name == acct.AwsProfile {
						preselect = i + 1
						break
					}
				}
			}

			m := singleSelectModel{
				title:  fmt.Sprintf("AWS profile for account %s", accountID),
				cursor: preselect,
			}
			m.items = make([]selectItem, 0, len(matching)+1)
			m.items = append(m.items, selectItem{name: "(none)"})
			for _, p := range matching {
				m.items = append(m.items, selectItem{
					name:    p.Name,
					preview: formatProfilePreview(p),
				})
			}

			result, err := runTUI(m)
			if err != nil {
				return err
			}
			sm := result.(singleSelectModel)
			if sm.cancelled {
				fmt.Println("Cancelled.")
				return nil
			}

			if sm.cursor > 0 {
				selectedProfile := matching[sm.cursor-1].Name
				global.AWS.Accounts[accountID] = config.AWSAccountConfig{
					AwsProfile: selectedProfile,
				}
				fmt.Printf("  aws_profile: %s\n", selectedProfile)
			}
			fmt.Println()
		}
	}

	// --- Kube contexts: select which contexts kestrel will target ---
	if kubeErr == nil && len(kubeContexts) > 0 {
		// Pre-select contexts that are already configured.
		preselected := make(map[int]bool)
		for _, existingCtx := range global.Kubernetes.Contexts {
			for i, c := range kubeContexts {
				if c.Name == existingCtx {
					preselected[i] = true
					break
				}
			}
		}

		// On first run (no global config yet), explain what this picker does.
		if _, statErr := os.Stat(config.GlobalConfigPath()); os.IsNotExist(statErr) {
			fmt.Println("Pick the kube contexts kestrel should target.")
			fmt.Println("Selected ones get recorded in kestrel's config so .kestconfig targets can resolve to them.")
			fmt.Println("Your kubeconfig is read-only here — nothing gets added, removed, or modified there.")
			fmt.Println("(a kestrel's gotta know what it's swooping on — SQUAAAWK)")
			fmt.Println()
		}

		m := multiSelectModel{
			title:    "Targets for kestrel to swoop on",
			items:    make([]selectItem, len(kubeContexts)),
			selected: preselected,
		}
		for i, c := range kubeContexts {
			m.items[i] = selectItem{
				name:    kubeconfig.ShortName(c.Name),
				preview: formatContextPreview(c),
			}
		}

		result, err := runTUI(m)
		if err != nil {
			return err
		}
		ms := result.(multiSelectModel)
		if ms.cancelled {
			fmt.Println("Cancelled.")
			return nil
		}

		// Build contexts map: short name (cluster) → full context name.
		global.Kubernetes.Contexts = make(map[string]string)
		for i, c := range kubeContexts {
			if ms.selected[i] {
				shortName := kubeconfig.ShortName(c.Name)
				global.Kubernetes.Contexts[shortName] = c.Name
				fmt.Printf("  %s → %s\n", shortName, c.Name)
			}
		}
		fmt.Println()
	}

	// --- Preferences ---
	fmt.Println("Preferences")
	fmt.Println()

	// auto_sso_login
	global.AWS.AutoSSOLogin, err = boolPrompt("Auto SSO login on expired sessions?", global.AWS.AutoSSOLogin)
	if err != nil {
		return err
	}

	// terraform preferences (only if the configured command is available)
	tfCommand := global.TerraformCommand()
	if toolExists(tfCommand) {
		// version manager: suggest tofuenv when command is tofu, else tfenv.
		// "off" disables kest's version-manager integration entirely.
		managerDefault := "tfenv"
		if tfCommand == "tofu" {
			managerDefault = "tofuenv"
		}
		global.Terraform.VersionManager, err = choicePrompt(
			"Terraform version manager",
			[]string{"tfenv", "tofuenv", "off"},
			global.Terraform.VersionManager,
			managerDefault,
		)
		if err != nil {
			return err
		}

		manager := global.TerraformVersionManager()
		if manager != "off" {
			prompt := fmt.Sprintf("Auto-install pinned terraform version via %s on mismatch?", manager)
			global.Terraform.AutoInstallPinned, err = boolPrompt(prompt, global.Terraform.AutoInstallPinned)
			if err != nil {
				return err
			}
		}

		pinFile := swoop.VersionFileFor(manager)
		global.Terraform.WriteVersion, err = boolPrompt(
			fmt.Sprintf("Write %s into roots that lack one?", pinFile),
			global.Terraform.WriteVersion,
		)
		if err != nil {
			return err
		}

		if global.Terraform.WriteVersion {
			global.Terraform.DefaultVersion, err = defaultVersionPrompt(tfCommand, pinFile, global.Terraform.DefaultVersion)
			if err != nil {
				return err
			}
		}
	}

	// swoop sort order
	global.Swoop.SortOrder, err = choicePrompt("Swoop sort order", []string{"git", "recent", "alpha"}, global.Swoop.SortOrder, "git")
	if err != nil {
		return err
	}

	// swoop cd mode
	global.Swoop.CDMode, err = choicePrompt("Swoop cd mode", []string{"cd", "pushd"}, global.Swoop.CDMode, "cd")
	if err != nil {
		return err
	}

	fmt.Println()

	// Assemble config for writing, preserving all sections.
	toWrite := &config.Config{
		AWS:        global.AWS,
		Kubernetes: global.Kubernetes,
		Terraform:  global.Terraform,
		Swoop:      global.Swoop,
	}

	for {
		fmt.Println("\nProposed global config:")
		fmt.Println(strings.Repeat("─", 40))
		out, _ := yaml.Marshal(toWrite)
		fmt.Print(string(out))
		fmt.Println(strings.Repeat("─", 40))

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
			edited, err := editInEditor(toWrite)
			if err != nil {
				return err
			}
			if edited == nil {
				fmt.Println("Editor returned empty file, keeping previous version.")
				continue
			}
			toWrite = edited
			continue
		case choiceWrite:
			// fall through
		}
		break
	}

	if err := config.WriteGlobal(toWrite); err != nil {
		return err
	}
	fmt.Printf("\nConfig written to %s\n", config.GlobalConfigPath())
	return nil
}

// editInEditor writes the config to a temp file, opens $EDITOR, and reads
// back the result. Returns nil if the user empties the file.
func editInEditor(cfg *config.Config) (*config.Config, error) {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = os.Getenv("VISUAL")
	}
	if editor == "" {
		editor = "vi"
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshalling config: %w", err)
	}

	tmpDir, err := os.MkdirTemp("", "kest-autoconfigure-*")
	if err != nil {
		return nil, fmt.Errorf("creating temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	tmpFile := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(tmpFile, data, 0o644); err != nil {
		return nil, fmt.Errorf("writing temp file: %w", err)
	}

	fmt.Println("\nOpening config in editor for review...")
	cmd := exec.Command(editor, tmpFile)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
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

func profileField(p awsconfig.Profile, key string) string {
	for _, f := range p.Fields {
		if f.Key == key {
			return f.Value
		}
	}
	return ""
}

func formatProfilePreview(p awsconfig.Profile) string {
	if len(p.Fields) == 0 {
		return ""
	}
	var b strings.Builder
	for _, f := range p.Fields {
		fmt.Fprintf(&b, "%s = %s\n", acPreviewKey.Render(f.Key), f.Value)
	}
	return b.String()
}

func formatContextPreview(c kubeconfig.Context) string {
	var b strings.Builder
	if c.Cluster != "" {
		fmt.Fprintf(&b, "%s = %s\n", acPreviewKey.Render("cluster"), c.Cluster)
	}
	if c.Namespace != "" {
		fmt.Fprintf(&b, "%s = %s\n", acPreviewKey.Render("namespace"), c.Namespace)
	}
	return b.String()
}

// boolPrompt asks a yes/no question using the single-select TUI.
func boolPrompt(title string, current bool) (bool, error) {
	preselect := 0 // No
	if current {
		preselect = 1 // Yes
	}
	m := singleSelectModel{
		title:  title,
		cursor: preselect,
		items: []selectItem{
			{name: "No"},
			{name: "Yes"},
		},
	}
	result, err := runTUI(m)
	if err != nil {
		return current, err
	}
	sm := result.(singleSelectModel)
	if sm.cancelled {
		return current, fmt.Errorf("cancelled")
	}
	chosen := sm.cursor == 1
	label := "no"
	if chosen {
		label = "yes"
	}
	fmt.Printf("  %s: %s\n", title, label)
	return chosen, nil
}

// choicePrompt asks the user to pick from a list of string options.
func choicePrompt(title string, options []string, current, defaultVal string) (string, error) {
	if current == "" {
		current = defaultVal
	}
	preselect := 0
	items := make([]selectItem, len(options))
	for i, opt := range options {
		label := opt
		if opt == defaultVal {
			label += " (default)"
		}
		items[i] = selectItem{name: label}
		if opt == current {
			preselect = i
		}
	}
	m := singleSelectModel{
		title:  title,
		cursor: preselect,
		items:  items,
	}
	result, err := runTUI(m)
	if err != nil {
		return current, err
	}
	sm := result.(singleSelectModel)
	if sm.cancelled {
		return current, fmt.Errorf("cancelled")
	}
	chosen := options[sm.cursor]
	fmt.Printf("  %s: %s\n", title, chosen)
	// Return empty string for default so it gets omitted from YAML.
	if chosen == defaultVal {
		return "", nil
	}
	return chosen, nil
}

// defaultVersionPrompt asks the user to pick a default terraform version.
// Detects the currently active version and offers it alongside "(none)".
// pinFile is the filename the version will be written to (used for the prompt
// label only).
func defaultVersionPrompt(command, pinFile, current string) (string, error) {
	items := []selectItem{{name: "(detect at runtime)"}}
	preselect := 0

	// Detect active terraform version.
	if out, err := exec.Command(command, "version").Output(); err == nil {
		if v := parseTFVersionOutput(string(out)); v != "" {
			items = append(items, selectItem{name: v})
			if current == v {
				preselect = 1
			}
		}
	}

	// If current is set but not the detected version, add it too.
	if current != "" && preselect == 0 {
		for i, item := range items {
			if item.name == current {
				preselect = i
				break
			}
		}
		if preselect == 0 {
			items = append(items, selectItem{name: current})
			preselect = len(items) - 1
		}
	}

	m := singleSelectModel{
		title:  fmt.Sprintf("Default terraform version for %s", pinFile),
		cursor: preselect,
		items:  items,
	}
	result, err := runTUI(m)
	if err != nil {
		return current, err
	}
	sm := result.(singleSelectModel)
	if sm.cancelled {
		return current, fmt.Errorf("cancelled")
	}
	if sm.cursor == 0 {
		fmt.Println("  default_version: (detect at runtime)")
		return "", nil
	}
	chosen := items[sm.cursor].name
	fmt.Printf("  default_version: %s\n", chosen)
	return chosen, nil
}

// parseTFVersionOutput extracts the version from `terraform version` or
// `tofu version` output.
func parseTFVersionOutput(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "Terraform v"); ok {
			return v
		}
		if v, ok := strings.CutPrefix(line, "OpenTofu v"); ok {
			return v
		}
	}
	return ""
}

func runTUI(m tea.Model) (tea.Model, error) {
	p := tea.NewProgram(m)
	return p.Run()
}

// ── styles ──

var (
	acTitleStyle  = lipgloss.NewStyle().Bold(true).Padding(0, 1)
	acCursorStyle = lipgloss.NewStyle().Bold(true).Foreground(colorAccent)
	acCheckStyle  = lipgloss.NewStyle().Foreground(colorSuccess)
	acPreviewHead = lipgloss.NewStyle().Bold(true).Foreground(colorAccent).Padding(1, 0, 0, 2)
	acPreviewKey  = lipgloss.NewStyle().Foreground(colorKey).PaddingLeft(4)
	acHelpStyle   = lipgloss.NewStyle().Foreground(colorHelp).Padding(1, 0, 0, 2)
)

// ── shared item type ──

type selectItem struct {
	name    string
	preview string
}

// ── multi-select model ──

type multiSelectModel struct {
	title     string
	items     []selectItem
	selected  map[int]bool
	cursor    int
	cancelled bool
}

func (m multiSelectModel) Init() tea.Cmd { return nil }

func (m multiSelectModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.items)-1 {
				m.cursor++
			}
		case " ":
			if m.selected[m.cursor] {
				delete(m.selected, m.cursor)
			} else {
				m.selected[m.cursor] = true
			}
		case "a":
			if len(m.selected) == len(m.items) {
				m.selected = make(map[int]bool)
			} else {
				for i := range m.items {
					m.selected[i] = true
				}
			}
		case "enter":
			return m, tea.Quit
		case "q", "esc", "ctrl+c":
			m.cancelled = true
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m multiSelectModel) View() string {
	var b strings.Builder

	b.WriteString(acTitleStyle.Render(m.title))
	b.WriteString("\n\n")

	for i, item := range m.items {
		cursor := "  "
		if i == m.cursor {
			cursor = "> "
		}
		check := "[ ] "
		if m.selected[i] {
			check = acCheckStyle.Render("[✓]") + " "
		}
		name := item.name
		if i == m.cursor {
			name = acCursorStyle.Render(name)
		}
		fmt.Fprintf(&b, "%s%s%s\n", cursor, check, name)
	}

	if preview := m.items[m.cursor].preview; preview != "" {
		b.WriteString(acPreviewHead.Render(fmt.Sprintf("── %s ──", m.items[m.cursor].name)))
		b.WriteString("\n")
		b.WriteString(preview)
	}

	b.WriteString(acHelpStyle.Render("space: toggle · a: all/none · enter: confirm · q: quit"))
	b.WriteString("\n")

	return b.String()
}

// ── single-select model ──

type singleSelectModel struct {
	title     string
	items     []selectItem
	cursor    int
	cancelled bool
}

func (m singleSelectModel) Init() tea.Cmd { return nil }

func (m singleSelectModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.items)-1 {
				m.cursor++
			}
		case "enter":
			return m, tea.Quit
		case "q", "esc", "ctrl+c":
			m.cancelled = true
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m singleSelectModel) View() string {
	var b strings.Builder

	b.WriteString(acTitleStyle.Render(m.title))
	b.WriteString("\n\n")

	for i, item := range m.items {
		cursor := "  "
		if i == m.cursor {
			cursor = "> "
		}
		name := item.name
		if i == m.cursor {
			name = acCursorStyle.Render(name)
		}
		fmt.Fprintf(&b, "%s%s\n", cursor, name)
	}

	if preview := m.items[m.cursor].preview; preview != "" {
		b.WriteString(acPreviewHead.Render(fmt.Sprintf("── %s ──", m.items[m.cursor].name)))
		b.WriteString("\n")
		b.WriteString(preview)
	}

	b.WriteString(acHelpStyle.Render("enter: select · q: quit"))
	b.WriteString("\n")

	return b.String()
}

// ── confirm model (write / edit / cancel) ──

const (
	choiceWrite  = "write"
	choiceEdit   = "edit"
	choiceCancel = "cancel"
)

var confirmOptions = []struct {
	label string
	key   string
	value string
}{
	{"Write config", "w", choiceWrite},
	{"Edit in $EDITOR", "e", choiceEdit},
	{"Cancel", "c", choiceCancel},
}

type confirmModel struct {
	cursor int
	choice string
}

func (m confirmModel) Init() tea.Cmd { return nil }

func (m confirmModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(confirmOptions)-1 {
				m.cursor++
			}
		case "enter":
			m.choice = confirmOptions[m.cursor].value
			return m, tea.Quit
		case "w":
			m.choice = choiceWrite
			return m, tea.Quit
		case "e":
			m.choice = choiceEdit
			return m, tea.Quit
		case "q", "esc", "ctrl+c", "c":
			m.choice = choiceCancel
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m confirmModel) View() string {
	var b strings.Builder
	b.WriteString(acTitleStyle.Render("What next?"))
	b.WriteString("\n\n")

	for i, opt := range confirmOptions {
		cursor := "  "
		if i == m.cursor {
			cursor = "> "
		}
		label := opt.label
		if i == m.cursor {
			label = acCursorStyle.Render(label)
		}
		fmt.Fprintf(&b, "%s%s\n", cursor, label)
	}

	b.WriteString(acHelpStyle.Render("enter: select · w/e/c: quick pick"))
	b.WriteString("\n")
	return b.String()
}
