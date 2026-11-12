package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/charmbracelet/lipgloss"
	"github.com/deepak-science/kestrel/internal/awsconfig"
	"github.com/deepak-science/kestrel/internal/config"
	"github.com/deepak-science/kestrel/internal/kubeconfig"
	"github.com/deepak-science/kestrel/internal/logging"
	"github.com/deepak-science/kestrel/internal/runner"
	"github.com/deepak-science/kestrel/internal/swoop"
	"github.com/spf13/cobra"
)

// doctorConfigErr captures any error from config loading so we can report it
// without aborting the command.
var doctorConfigErr error

var doctorCmd = &cobra.Command{
	Use:     "doctor",
	Short:   "Check system health and configuration",
	GroupID: "config",
	Long: `Run diagnostic checks on kestrel's dependencies and configuration.

Verifies that required CLI tools are installed, configuration files are
present and consistent, AWS SSO sessions are active, and project-level
settings are in order.`,
	// Override root's PersistentPreRunE so doctor survives broken config.
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true

		cleanup, err := logging.Init()
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not initialize logging: %v\n", err)
		} else {
			logCleanup = cleanup
		}

		cfg, err = config.Load()
		if err != nil {
			doctorConfigErr = err
			cfg = nil
		}
		return nil
	},
	RunE: runDoctor,
}

func init() {
	rootCmd.AddCommand(doctorCmd)
}

// --- types ---

type checkStatus int

const (
	statusPass checkStatus = iota
	statusFail
	statusWarn
	statusSkip
)

type checkResult struct {
	status checkStatus
	label  string
	detail string
}

type checkSection struct {
	name   string
	checks []checkResult
}

// --- styles ---

var (
	doctorTitleStyle  = lipgloss.NewStyle().Bold(true).Foreground(colorAccent)
	doctorPassStyle   = lipgloss.NewStyle().Foreground(colorSuccess)
	doctorFailStyle   = lipgloss.NewStyle().Foreground(colorDanger)
	doctorWarnStyle   = lipgloss.NewStyle().Foreground(colorWarn)
	doctorSkipStyle   = lipgloss.NewStyle().Foreground(colorDim)
	doctorDetailStyle = lipgloss.NewStyle().Foreground(colorDim)
)

// --- output ---

func doctorIndicator(s checkStatus) (string, lipgloss.Style) {
	switch s {
	case statusPass:
		return "✓", doctorPassStyle
	case statusFail:
		return "✗", doctorFailStyle
	case statusWarn:
		return "!", doctorWarnStyle
	default:
		return "-", doctorSkipStyle
	}
}

func printDoctorSection(s checkSection) {
	fmt.Println(doctorTitleStyle.Render(s.name))
	for _, c := range s.checks {
		ind, style := doctorIndicator(c.status)
		label := fmt.Sprintf("  %s %s", ind, c.label)
		if c.detail != "" {
			label += " " + doctorDetailStyle.Render(c.detail)
		}
		fmt.Println(style.Render(label))
	}
	fmt.Println()
}

func countDoctorResults(sections []checkSection) (pass, fail, warn, skip int) {
	for _, s := range sections {
		for _, c := range s.checks {
			switch c.status {
			case statusPass:
				pass++
			case statusFail:
				fail++
			case statusWarn:
				warn++
			case statusSkip:
				skip++
			}
		}
	}
	return
}

// --- main ---

func runDoctor(cmd *cobra.Command, args []string) error {
	var sections []checkSection

	awsFound := toolExists("aws")

	sections = append(sections, checkTools(awsFound))
	sections = append(sections, checkDoctorConfig())

	if awsFound && cfg != nil && len(cfg.AWS.Accounts) > 0 {
		sections = append(sections, checkSessions())
	}

	sections = append(sections, checkDoctorProject())

	fmt.Println()
	for _, s := range sections {
		printDoctorSection(s)
	}

	pass, fail, warn, skip := countDoctorResults(sections)
	summary := fmt.Sprintf("%d passed, %d failed, %d warnings, %d skipped", pass, fail, warn, skip)
	if fail > 0 {
		fmt.Println(doctorFailStyle.Render(summary))
	} else {
		fmt.Println(doctorPassStyle.Render(summary))
	}

	return nil
}

// --- tool checks ---

var (
	reTerraformVer = regexp.MustCompile(`(?:Terraform|OpenTofu) v(\d+\.\d+\.\d+)`)
	reHelmVer      = regexp.MustCompile(`v(\d+\.\d+\.\d+)`)
	reKubectlVer   = regexp.MustCompile(`"gitVersion":\s*"v(\d+\.\d+\.\d+)`)
	reAWSVer       = regexp.MustCompile(`aws-cli/(\d+\.\d+\.\d+)`)
)

func toolExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func checkToolWithVersion(name string, versionArgs []string, re *regexp.Regexp) checkResult {
	if !toolExists(name) {
		return checkResult{status: statusFail, label: name, detail: "not found"}
	}
	if versionArgs == nil {
		return checkResult{status: statusPass, label: name}
	}
	out, err := runner.Output(name, versionArgs...)
	if err != nil {
		return checkResult{status: statusPass, label: name, detail: "(version unknown)"}
	}
	if m := re.FindStringSubmatch(out); len(m) > 1 {
		return checkResult{status: statusPass, label: name, detail: "v" + m[1]}
	}
	return checkResult{status: statusPass, label: name, detail: "(version unknown)"}
}

func checkTools(awsFound bool) checkSection {
	checks := []checkResult{
		checkToolWithVersion(cfg.TerraformCommand(), []string{"version"}, reTerraformVer),
		checkToolWithVersion("helm", []string{"version", "--short"}, reHelmVer),
		checkToolWithVersion("kubectl", []string{"version", "--client", "-o", "json"}, reKubectlVer),
	}

	// AWS: reuse the existence check we already did.
	if awsFound {
		out, err := runner.Output("aws", "--version")
		detail := "(version unknown)"
		if err == nil {
			if m := reAWSVer.FindStringSubmatch(out); len(m) > 1 {
				detail = "v" + m[1]
			}
		}
		checks = append(checks, checkResult{status: statusPass, label: "aws", detail: detail})
	} else {
		checks = append(checks, checkResult{status: statusFail, label: "aws", detail: "not found"})
	}

	// Version manager (tfenv / tofuenv / off) — optional.
	if manager := cfg.TerraformVersionManager(); manager != "off" {
		if toolExists(manager) {
			checks = append(checks, checkResult{status: statusPass, label: manager})
		} else {
			checks = append(checks, checkResult{status: statusWarn, label: manager, detail: "not found (optional)"})
		}
	}

	return checkSection{name: "Tools", checks: checks}
}

// --- config checks ---

func checkDoctorConfig() checkSection {
	var checks []checkResult

	// Global kest config.
	globalPath := config.GlobalConfigPath()
	if doctorConfigErr != nil {
		checks = append(checks, checkResult{
			status: statusFail,
			label:  "Global config",
			detail: doctorConfigErr.Error(),
		})
	} else if cfg != nil && cfg.Sources.Global != "" {
		checks = append(checks, checkResult{
			status: statusPass,
			label:  "Global config",
			detail: globalPath,
		})
	} else {
		checks = append(checks, checkResult{
			status: statusWarn,
			label:  "Global config",
			detail: "not found at " + globalPath,
		})
	}

	// Accounts and contexts from kest config.
	if cfg != nil {
		acctCount := len(cfg.AWS.Accounts)
		if acctCount > 0 {
			checks = append(checks, checkResult{
				status: statusPass,
				label:  fmt.Sprintf("%d AWS account(s) configured", acctCount),
			})
		} else {
			checks = append(checks, checkResult{
				status: statusWarn,
				label:  "No AWS accounts configured",
				detail: "— run kest config autoconfigure",
			})
		}

		ctxCount := len(cfg.Kubernetes.Contexts)
		if ctxCount > 0 {
			checks = append(checks, checkResult{
				status: statusPass,
				label:  fmt.Sprintf("%d kube context(s) configured", ctxCount),
			})
		} else {
			checks = append(checks, checkResult{
				status: statusWarn,
				label:  "No kube contexts configured",
				detail: "— run kest config autoconfigure",
			})
		}
	}

	// AWS config file.
	profiles, err := awsconfig.ReadProfiles()
	if err != nil {
		checks = append(checks, checkResult{
			status: statusWarn,
			label:  "AWS config (~/.aws/config)",
			detail: "not readable",
		})
	} else {
		checks = append(checks, checkResult{
			status: statusPass,
			label:  fmt.Sprintf("AWS config: %d profile(s)", len(profiles)),
		})
	}

	// Kubeconfig.
	contexts, err := kubeconfig.ReadContexts()
	if err != nil {
		checks = append(checks, checkResult{
			status: statusWarn,
			label:  "Kubeconfig",
			detail: "not readable",
		})
	} else {
		checks = append(checks, checkResult{
			status: statusPass,
			label:  fmt.Sprintf("Kubeconfig: %d context(s)", len(contexts)),
		})
	}

	return checkSection{name: "Config", checks: checks}
}

// --- AWS session checks ---

func checkAWSSessionValid(profile string) bool {
	cmd := exec.Command("aws", "sts", "get-caller-identity", "--profile", profile)
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run() == nil
}

func checkSessions() checkSection {
	// Collect unique profiles.
	type profileEntry struct {
		name      string
		accountID string
	}
	seen := make(map[string]bool)
	var entries []profileEntry
	// Sort account IDs for deterministic output.
	accountIDs := make([]string, 0, len(cfg.AWS.Accounts))
	for id := range cfg.AWS.Accounts {
		accountIDs = append(accountIDs, id)
	}
	sort.Strings(accountIDs)

	for _, id := range accountIDs {
		acct := cfg.AWS.Accounts[id]
		if acct.AwsProfile != "" && !seen[acct.AwsProfile] {
			seen[acct.AwsProfile] = true
			entries = append(entries, profileEntry{name: acct.AwsProfile, accountID: id})
		}
	}

	if len(entries) == 0 {
		return checkSection{
			name:   "AWS Sessions",
			checks: []checkResult{{status: statusSkip, label: "No AWS profiles to check"}},
		}
	}

	results := make([]checkResult, len(entries))
	var wg sync.WaitGroup
	for i, e := range entries {
		wg.Add(1)
		go func(idx int, ent profileEntry) {
			defer wg.Done()
			if checkAWSSessionValid(ent.name) {
				results[idx] = checkResult{
					status: statusPass,
					label:  ent.name,
					detail: "session active",
				}
			} else {
				results[idx] = checkResult{
					status: statusFail,
					label:  ent.name,
					detail: "session expired or invalid",
				}
			}
		}(i, e)
	}
	wg.Wait()

	return checkSection{name: "AWS Sessions", checks: results}
}

// --- project checks ---

func checkDoctorProject() checkSection {
	var checks []checkResult

	if cfg == nil || cfg.Sources.Project == "" {
		return checkSection{
			name:   "Project",
			checks: []checkResult{{status: statusSkip, label: "No .kestconfig found"}},
		}
	}

	checks = append(checks, checkResult{
		status: statusPass,
		label:  ".kestconfig found",
		detail: cfg.Sources.Project,
	})

	// Targets.
	targetCount := len(cfg.Targets)
	if targetCount > 0 {
		names := cfg.TargetNames()
		checks = append(checks, checkResult{
			status: statusPass,
			label:  fmt.Sprintf("%d target(s)", targetCount),
			detail: strings.Join(names, ", "),
		})
	}

	// Terraform roots.
	baseDir, err := resolveBaseDir()
	if err != nil {
		checks = append(checks, checkResult{
			status: statusWarn,
			label:  "Terraform roots",
			detail: "could not resolve base dir: " + err.Error(),
		})
		return checkSection{name: "Project", checks: checks}
	}

	roots, err := swoop.Discover(baseDir)
	if err != nil {
		checks = append(checks, checkResult{
			status: statusWarn,
			label:  "Terraform roots",
			detail: "discovery failed: " + err.Error(),
		})
		return checkSection{name: "Project", checks: checks}
	}

	if len(roots) == 0 {
		checks = append(checks, checkResult{
			status: statusSkip,
			label:  "No terraform roots found",
		})
		return checkSection{name: "Project", checks: checks}
	}

	initialized := 0
	for _, r := range roots {
		if r.Initialized {
			initialized++
		}
	}

	detail := fmt.Sprintf("%d initialized", initialized)
	if uninit := len(roots) - initialized; uninit > 0 {
		detail += fmt.Sprintf(", %d not initialized", uninit)
	}
	checks = append(checks, checkResult{
		status: statusPass,
		label:  fmt.Sprintf("%d terraform root(s)", len(roots)),
		detail: detail,
	})

	return checkSection{name: "Project", checks: checks}
}
