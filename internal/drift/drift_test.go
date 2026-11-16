package drift

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/adrg/xdg"
	"github.com/deepak-science/kestrel/internal/config"
	"github.com/deepak-science/kestrel/internal/swoop"
	"github.com/deepak-science/kestrel/internal/testutil"
)

// fakeTerraform plans from fake-plan.{out,code} in the root dir and fails init where fake-init-fails exists.
const fakeTerraform = `case "$1" in
init) [ -f fake-init-fails ] && { echo "Error: Failed to query available provider packages" >&2; exit 1; }; exit 0 ;;
plan) cat fake-plan.out; echo "│ Error: Unsupported attribute" >&2; exit "$(cat fake-plan.code)" ;;
esac
`

const fakeKubectl = `p="$3"
case "$1" in
diff) printf 'diff -u -N /tmp/LIVE-1/apps.v1.Deployment.media.web /tmp/MERGED-2/apps.v1.Deployment.media.web\n'; exit 1 ;;
get) echo '{"kind":"List","items":[]}' ;;
esac
`

func fakeRoot(t *testing.T, path string, code int, out string, initialized bool) swoop.Root {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{"fake-plan.code": strconv.Itoa(code), "fake-plan.out": out} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return swoop.Root{Path: path, AbsPath: dir, Dir: strings.Split(path, "/")[0], Initialized: initialized}
}

func setup(t *testing.T) {
	t.Helper()
	testutil.FakeBin(t, "terraform", fakeTerraform)
	testutil.FakeBin(t, "kubectl", fakeKubectl)
	orig := xdg.StateHome
	xdg.StateHome = t.TempDir()
	t.Cleanup(func() { xdg.StateHome = orig })
}

func TestSweep(t *testing.T) {
	setup(t)
	roots := []swoop.Root{
		fakeRoot(t, "dev/vpc", 0, "No changes. Your infrastructure matches the configuration.\n", true),
		fakeRoot(t, "prd/rds", 2, "Plan: 1 to add, 0 to change, 0 to destroy.\n", true),
		fakeRoot(t, "prd/broken", 1, "", true),
		fakeRoot(t, "prd/new", 0, "", false),
	}
	cfg := &config.Config{
		Targets: map[string]config.TargetConfig{"dev": {}},
		Deploys: map[string]config.Deploy{"web": {Target: "dev", Manifests: "manifests/web"}},
	}
	base := t.TempDir()
	var progress bytes.Buffer
	items := Sweep{Cfg: cfg, BaseDir: base, Parallel: 3, Progress: &progress}.Run(roots, []string{"web", "gone"})

	want := []struct{ name, status, text string }{
		{"dev/vpc", Clean, "no changes"},
		{"prd/rds", Drift, "1 to add, 0 to change, 0 to destroy"},
		{"prd/broken", Error, "terraform plan: Error: Unsupported attribute"},
		{"prd/new", Error, "not initialized"},
		{"web", Drift, "differs from git: apps.v1.Deployment.media.web"},
		{"gone", Error, "not found in .kestconfig"},
	}
	if len(items) != len(want) {
		t.Fatalf("got %d items, want %d", len(items), len(want))
	}
	for i, w := range want {
		it := items[i]
		text := it.Summary + it.Error + strings.Join(it.Details, ";")
		if it.Name != w.name || it.Status != w.status || !strings.Contains(text, w.text) {
			t.Errorf("item %d = %+v, want %s %s containing %q", i, it, w.name, w.status, w.text)
		}
	}
	if n := strings.Count(progress.String(), "\n"); n != len(want) {
		t.Errorf("progress has %d lines, want %d:\n%s", n, len(want), progress.String())
	}

	state, err := swoop.LoadState(base)
	if err != nil {
		t.Fatal(err)
	}
	if e := state.Entries["prd/rds"]; e == nil || e.LastPlan == nil {
		t.Error("drifted root's plan wasn't recorded")
	}
	if e := state.Entries["prd/broken"]; e != nil && e.LastPlan != nil {
		t.Error("failed plan was recorded")
	}

	if err := Result(items); err == nil || errors.Is(err, swoop.ErrChanges) {
		t.Errorf("Result with errors = %v, want a failure", err)
	}
	if err := Result(items[:2]); !errors.Is(err, swoop.ErrChanges) {
		t.Errorf("Result with drift only = %v, want ErrChanges", err)
	}
	if err := Result(items[:1]); err != nil {
		t.Errorf("Result clean = %v", err)
	}
}

func TestSweep_Init(t *testing.T) {
	setup(t)
	ok := fakeRoot(t, "dev/vpc", 0, "No changes.\n", false)
	bad := fakeRoot(t, "dev/dns", 0, "", false)
	if err := os.WriteFile(filepath.Join(bad.AbsPath, "fake-init-fails"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	items := Sweep{Cfg: &config.Config{}, BaseDir: t.TempDir(), Init: true, Progress: &bytes.Buffer{}}.Run([]swoop.Root{ok, bad}, nil)
	if items[0].Status != Clean {
		t.Errorf("initialized root = %+v, want clean", items[0])
	}
	if items[1].Status != Error || !strings.Contains(items[1].Error, "Failed to query available provider packages") {
		t.Errorf("failed init = %+v", items[1])
	}
}

func TestPrintTable(t *testing.T) {
	var b bytes.Buffer
	PrintTable(&b, []Item{
		{Kind: "root", Name: "dev/vpc", Status: Clean, Summary: "no changes"},
		{Kind: "deploy", Name: "web", Status: Drift, Details: []string{"differs from git: x", "kubectl-patch on deployment.apps/web: spec.replicas"}},
		{Kind: "deploy", Name: "db", Status: Error, Error: "kubectl diff: boom"},
	})
	got := b.String()
	for _, want := range []string{"DRIFT", "kubectl-patch on deployment.apps/web", "ERROR   kubectl diff: boom", "3 checked: 1 clean, 1 drifted, 1 errored"} {
		if !strings.Contains(got, want) {
			t.Errorf("table missing %q:\n%s", want, got)
		}
	}
}

func TestSelect(t *testing.T) {
	cfg := &config.Config{Deploys: map[string]config.Deploy{
		"web": {Target: "dev", Manifests: "m"},
		"db":  {Target: "prod", Manifests: "m"},
	}}
	roots := []swoop.Root{{Path: "dev/vpc", Dir: "dev"}, {Path: "prd/vpc", Dir: "prd"}}

	r, d, err := Select(cfg, roots, Filter{Dir: "dev", Target: "prod"})
	if err != nil || len(r) != 1 || r[0].Path != "dev/vpc" || len(d) != 1 || d[0] != "db" {
		t.Errorf("Select(dir, target) = %v, %v, %v", r, d, err)
	}
	if r, d, _ := Select(cfg, roots, Filter{Only: "deploys"}); len(r) != 0 || len(d) != 2 {
		t.Errorf("--only deploys = %v, %v", r, d)
	}
	if r, d, _ := Select(cfg, roots, Filter{Only: "terraform"}); len(r) != 2 || len(d) != 0 {
		t.Errorf("--only terraform = %v, %v", r, d)
	}
	if _, _, err := Select(cfg, nil, Filter{Only: "terraform"}); err == nil {
		t.Error("empty selection wasn't an error")
	}
	if err := (Filter{Only: "helm"}).Validate(); err == nil {
		t.Error("--only helm accepted")
	}
}
