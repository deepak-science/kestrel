package swoop

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// PlanPath is where `swoop plan --out` writes a root's planfile.
// .terraform/ is conventionally gitignored, which matters: planfiles hold sensitive values in plaintext.
func PlanPath(root Root) string {
	return filepath.Join(root.AbsPath, ".terraform", "kest.tfplan")
}

func planMetaPath(root Root) string { return PlanPath(root) + ".json" }

// planMeta records what a planfile was made from.
// Terraform already rejects a saved plan once state has moved; this catches config edits after the plan, which terraform would silently leave out of the apply.
type planMeta struct {
	Fingerprint string    `json:"fingerprint"`
	GitHead     string    `json:"git_head,omitempty"`
	AWSProfile  string    `json:"aws_profile,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// fingerprintFiles reports whether a root-dir file is part of the plan's input.
func fingerprintFiles(name string) bool {
	switch {
	case name == ".terraform.lock.hcl":
		return true
	case strings.HasSuffix(name, ".tf"), strings.HasSuffix(name, ".tf.json"),
		strings.HasSuffix(name, ".tfvars"), strings.HasSuffix(name, ".tfvars.json"):
		return true
	}
	return false
}

// rootFingerprint hashes the names and contents of the root's own config files.
// Local modules outside the root are covered by GitHead plus the clean-worktree guard.
func rootFingerprint(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() && fingerprintFiles(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	h := sha256.New()
	for _, n := range names {
		f, err := os.Open(filepath.Join(dir, n))
		if err != nil {
			return "", err
		}
		_, _ = fmt.Fprintf(h, "%s\x00", n)
		_, err = io.Copy(h, f)
		_ = f.Close()
		if err != nil {
			return "", err
		}
		_, _ = h.Write([]byte{0})
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func gitHead(dir string) string {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func currentPlanMeta(root Root, profile string, now time.Time) (planMeta, error) {
	fp, err := rootFingerprint(root.AbsPath)
	if err != nil {
		return planMeta{}, fmt.Errorf("fingerprint %s: %w", root.Path, err)
	}
	return planMeta{Fingerprint: fp, GitHead: gitHead(root.AbsPath), AWSProfile: profile, CreatedAt: now}, nil
}

// clearPlan removes a saved plan so a failed re-plan can't leave an older one behind.
func clearPlan(root Root) {
	_ = os.Remove(PlanPath(root))
	_ = os.Remove(planMetaPath(root))
}

func writePlanMeta(root Root, profile string) error {
	_ = os.Chmod(PlanPath(root), 0o600)
	m, err := currentPlanMeta(root, profile, time.Now())
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(planMetaPath(root), append(data, '\n'), 0o600)
}

// CheckPlan returns the saved plan's metadata, or an error if the planfile is missing or was made from different inputs.
func CheckPlan(root Root, profile string) (planMeta, error) {
	replan := fmt.Sprintf("re-run: kest swoop plan %s --out", root.Path)
	if _, err := os.Stat(PlanPath(root)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return planMeta{}, fmt.Errorf("no saved plan for %s — run: kest swoop plan %s --out", root.Path, root.Path)
		}
		return planMeta{}, err
	}
	data, err := os.ReadFile(planMetaPath(root))
	if err != nil {
		return planMeta{}, fmt.Errorf("saved plan for %s has no kest metadata — %s", root.Path, replan)
	}
	var saved planMeta
	if err := json.Unmarshal(data, &saved); err != nil {
		return planMeta{}, fmt.Errorf("read plan metadata: %w — %s", err, replan)
	}
	now, err := currentPlanMeta(root, profile, time.Now())
	if err != nil {
		return planMeta{}, err
	}
	if err := comparePlanMeta(saved, now); err != nil {
		return planMeta{}, fmt.Errorf("saved plan for %s is stale: %w — %s", root.Path, err, replan)
	}
	return saved, nil
}

func comparePlanMeta(saved, now planMeta) error {
	switch {
	case saved.GitHead != now.GitHead:
		return fmt.Errorf("HEAD moved from %s to %s", short(saved.GitHead), short(now.GitHead))
	case saved.Fingerprint != now.Fingerprint:
		return errors.New("root config changed since the plan")
	case saved.AWSProfile != now.AWSProfile:
		return fmt.Errorf("planned with AWS profile %q, now %q", saved.AWSProfile, now.AWSProfile)
	}
	return nil
}

func short(sha string) string {
	if sha == "" {
		return "(none)"
	}
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
