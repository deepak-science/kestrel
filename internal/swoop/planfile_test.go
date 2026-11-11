package swoop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func planRoot(t *testing.T) Root {
	t.Helper()
	root := writeRoot(t, "live/dev", "terraform {}\n")
	if err := os.MkdirAll(filepath.Dir(PlanPath(root)), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func savePlan(t *testing.T, root Root, profile string) {
	t.Helper()
	if err := os.WriteFile(PlanPath(root), []byte("plan"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writePlanMeta(root, profile); err != nil {
		t.Fatal(err)
	}
}

func TestCheckPlan_Fresh(t *testing.T) {
	root := planRoot(t)
	savePlan(t, root, "homelab")
	if _, err := CheckPlan(root, "homelab"); err != nil {
		t.Fatalf("fresh plan rejected: %v", err)
	}
}

func TestCheckPlan_Missing(t *testing.T) {
	root := planRoot(t)
	_, err := CheckPlan(root, "")
	if err == nil || !strings.Contains(err.Error(), "kest swoop plan live/dev --out") {
		t.Fatalf("want missing-plan error with replan command, got %v", err)
	}
}

func TestCheckPlan_NoMeta(t *testing.T) {
	root := planRoot(t)
	if err := os.WriteFile(PlanPath(root), []byte("plan"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckPlan(root, ""); err == nil {
		t.Fatal("planfile without kest metadata accepted")
	}
}

func TestCheckPlan_ConfigChanged(t *testing.T) {
	root := planRoot(t)
	savePlan(t, root, "")
	if err := os.WriteFile(filepath.Join(root.AbsPath, "vars.tfvars"), []byte("x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := CheckPlan(root, "")
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("want stale error, got %v", err)
	}
}

func TestCheckPlan_ProfileChanged(t *testing.T) {
	root := planRoot(t)
	savePlan(t, root, "homelab")
	if _, err := CheckPlan(root, "other"); err == nil {
		t.Fatal("plan made under a different profile accepted")
	}
}

// Files terraform doesn't read must not invalidate a plan.
func TestRootFingerprint_IgnoresUnrelatedFiles(t *testing.T) {
	root := planRoot(t)
	before, err := rootFingerprint(root.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root.AbsPath, "README.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := rootFingerprint(root.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Error("README change altered the fingerprint")
	}
}

func TestComparePlanMeta_HeadMoved(t *testing.T) {
	err := comparePlanMeta(planMeta{GitHead: "aaaaaaaaaaaaaaaa"}, planMeta{GitHead: "bbbbbbbbbbbbbbbb"})
	if err == nil || !strings.Contains(err.Error(), "aaaaaaaaaaaa to bbbbbbbbbbbb") {
		t.Fatalf("got %v", err)
	}
}
