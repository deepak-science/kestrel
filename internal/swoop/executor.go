package swoop

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/deepak-science/kestrel/internal/runner"
)

// ExecResult captures the outcome of a terraform execution.
type ExecResult struct {
	// ExitCode is the process exit code.
	ExitCode int

	// PlanSummary is the parsed plan summary line, if any (e.g. "1 to add, 0 to change, 0 to destroy").
	// Empty for non-plan commands or if parsing fails.
	PlanSummary string

	// Changes is set when plan ran with -detailed-exitcode and exited 2.
	Changes bool
}

// TFVersionCheck is the result of checking the terraform version for a root.
type TFVersionCheck struct {
	// OK is true if the version matches or no version constraint exists.
	OK bool

	// Required is the version from .terraform-version (empty if none).
	Required string

	// Installed is the version terraform reports from the root dir.
	Installed string

	// VersionManagerAvailable is true when the configured version manager
	// (e.g. tfenv, tofuenv) is on PATH and usable.
	VersionManagerAvailable bool
}

// RunTerraform executes the given terraform-compatible command with the given
// args in the root's directory. Output is streamed to stdout/stderr. The
// awsProfile is injected as AWS_PROFILE if non-empty. Plan output is also
// captured so the summary line can be parsed.
func RunTerraform(command string, root Root, awsProfile string, args ...string) (*ExecResult, error) {
	return runTerraform(command, root, awsProfile, false, args...)
}

// runTerraform is RunTerraform with an option to keep plan stdout off the terminal.
func runTerraform(command string, root Root, awsProfile string, quiet bool, args ...string) (*ExecResult, error) {
	isPlan := len(args) > 0 && args[0] == "plan"

	res, err := runner.RunWithOpts(command, args, runner.Options{
		Dir:           root.AbsPath,
		Env:           profileEnv(awsProfile),
		CaptureStdout: isPlan,
		Silent:        isPlan && quiet,
	})

	out := &ExecResult{ExitCode: res.ExitCode}
	if isPlan {
		out.PlanSummary = parsePlanSummary(res.Stdout)
	}
	return out, err
}

func profileEnv(awsProfile string) map[string]string {
	if awsProfile == "" {
		return nil
	}
	return map[string]string{"AWS_PROFILE": awsProfile}
}

// ShowPlanSummary renders the root's saved planfile via `show -json`.
func ShowPlanSummary(command string, root Root, awsProfile string) (string, error) {
	res, err := runner.RunWithOpts(command, []string{"show", "-json", PlanPath(root)}, runner.Options{
		Dir:           root.AbsPath,
		Env:           profileEnv(awsProfile),
		CaptureStdout: true,
		Silent:        true,
	})
	if err != nil {
		return "", fmt.Errorf("%s show -json: %w", command, err)
	}
	return FormatPlanSummary([]byte(res.Stdout))
}

// parsePlanSummary extracts the plan summary from terraform plan output.
var planSummaryRe = regexp.MustCompile(`Plan: (.+?)\.$`)

func parsePlanSummary(output string) string {
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.Contains(line, "No changes") {
			return "no changes"
		}
		if m := planSummaryRe.FindStringSubmatch(line); len(m) > 1 {
			return m[1]
		}
	}
	return ""
}

// CheckTFVersion checks whether the version of the given terraform-compatible
// command available in the root directory matches the .terraform-version
// requirement. The manager argument is the version-manager CLI (e.g. "tfenv",
// "tofuenv"); the special value "off" disables version-manager probing.
func CheckTFVersion(command, manager string, root Root) TFVersionCheck {
	if root.TFVersion == "" {
		return TFVersionCheck{OK: true}
	}

	// Run from the root dir so tfenv/tofuenv picks up .terraform-version.
	cmd := exec.Command(command, "version")
	cmd.Dir = root.AbsPath
	out, err := cmd.Output()
	if err != nil {
		return TFVersionCheck{
			Required:                root.TFVersion,
			VersionManagerAvailable: hasVersionManager(manager),
		}
	}

	installed := ParseTFVersion(string(out))
	if installed == "" {
		return TFVersionCheck{OK: true, Required: root.TFVersion}
	}

	return TFVersionCheck{
		OK:                      installed == root.TFVersion,
		Required:                root.TFVersion,
		Installed:               installed,
		VersionManagerAvailable: hasVersionManager(manager),
	}
}

// InstallTFVersion runs `<manager> install <version>` with output streamed to
// stderr. Returns an error if the manager is "off" or unavailable.
func InstallTFVersion(manager, version string) error {
	if manager == "" || manager == "off" {
		return fmt.Errorf("no terraform version manager configured")
	}
	cmd := exec.Command(manager, "install", version)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// hasVersionManager reports whether the named version manager is available on
// PATH. Returns false for "" or "off".
func hasVersionManager(manager string) bool {
	if manager == "" || manager == "off" {
		return false
	}
	_, err := exec.LookPath(manager)
	return err == nil
}

// tfVersionRe matches both Terraform ("Terraform v1.9.2") and OpenTofu
// ("OpenTofu v1.8.0") version output.
var tfVersionRe = regexp.MustCompile(`(?:Terraform|OpenTofu) v(\d+\.\d+\.\d+)`)

// ParseTFVersion extracts the semver from `terraform version` or
// `tofu version` output.
func ParseTFVersion(output string) string {
	m := tfVersionRe.FindStringSubmatch(output)
	if len(m) > 1 {
		return m[1]
	}
	return ""
}

// FormatTFVersionCommand returns the command string that would install the
// pinned version via the given manager (e.g. "tfenv install 1.9.2").
func FormatTFVersionCommand(manager, version string) string {
	return fmt.Sprintf("%s install %s", manager, version)
}

// VersionFileFor returns the version-pin filename to use for the given
// version manager: ".opentofu-version" for tofuenv, ".terraform-version"
// otherwise (including when the manager is "off" or empty).
func VersionFileFor(manager string) string {
	if manager == "tofuenv" {
		return ".opentofu-version"
	}
	return ".terraform-version"
}

// EnsureTFVersion writes a version-pin file into the root directory if one
// does not already exist. The filename is chosen based on the manager (see
// VersionFileFor). If preferredVersion is non-empty it is used directly;
// otherwise the currently active version of the given command is detected.
// Returns the basename written and the version, or ("", "", nil) if a pin
// file already existed.
func EnsureTFVersion(command, manager string, root Root, preferredVersion string) (string, string, error) {
	if root.TFVersion != "" {
		return "", "", nil
	}

	version := preferredVersion
	if version == "" {
		v, err := ActiveTFVersion(command, root.AbsPath)
		if err != nil {
			return "", "", err
		}
		version = v
	}

	filename, err := PinTFVersion(root, manager, version)
	if err != nil {
		return "", "", err
	}
	return filename, version, nil
}
