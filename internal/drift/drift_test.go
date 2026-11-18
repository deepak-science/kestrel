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
get|apply) echo '{"kind":"List","items":[]}' ;;
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

func rootPaths(roots []swoop.Root) string {
	var p []string
	for _, r := range roots {
		p = append(p, r.Path)
	}
	return strings.Join(p, ",")
}

func TestSelect(t *testing.T) {
	cfg := &config.Config{Deploys: map[string]config.Deploy{
		"web": {Target: "dev", Manifests: "m"},
		"db":  {Target: "prod", Manifests: "m"},
	}}
	roots := []swoop.Root{{Path: "dev/vpc", Dir: "dev"}, {Path: "prd/vpc", Dir: "prd"}}
	check := func(name string, f Filter, wantRoots, wantDeploys string, wantSkipped int) {
		t.Helper()
		sel, err := Select(cfg, roots, f)
		if err != nil || rootPaths(sel.Roots) != wantRoots || strings.Join(sel.Deploys, ",") != wantDeploys || len(sel.Skipped) != wantSkipped {
			t.Errorf("%s = %+v, %v; want roots %q, deploys %q, %d skipped", name, sel, err, wantRoots, wantDeploys, wantSkipped)
		}
	}
	check("dir and target", Filter{Dir: "dev", Target: "prod"}, "dev/vpc", "db", 0)
	check("--only deploys", Filter{Only: "deploys"}, "", "db,web", 0)
	check("--only terraform", Filter{Only: "terraform"}, "dev/vpc,prd/vpc", "", 0)
	check("excludes", Filter{ExcludeRoots: []string{"prd/"}, ExcludeDeploys: []string{"db"}}, "dev/vpc", "web", 2)
	// An excluded root outside the --dir selection is still a known root, not a typo, and isn't reported as skipped.
	check("exclude outside --dir", Filter{Dir: "dev", ExcludeRoots: []string{"prd/vpc"}, Only: "terraform"}, "dev/vpc", "", 0)

	if _, err := Select(cfg, nil, Filter{Only: "terraform"}); err == nil {
		t.Error("empty selection wasn't an error")
	}
	if err := (Filter{Only: "helm"}).Validate(); err == nil {
		t.Error("--only helm accepted")
	}
	// A filter that matches nothing must not shrink the sweep to whatever else is left.
	for name, f := range map[string]Filter{
		"unmatched --exclude-root":   {ExcludeRoots: []string{"prd/vp"}},
		"unmatched --exclude-deploy": {ExcludeDeploys: []string{"typo"}},
		"--exclude-root, apps only":  {Only: "deploys", ExcludeRoots: []string{"prd"}},
		"--exclude-deploy, tf only":  {Only: "terraform", ExcludeDeploys: []string{"db"}},
		"everything excluded":        {Only: "terraform", ExcludeRoots: []string{"dev", "prd"}},
		"unmatched --dir":            {Dir: "typo"},
		"unmatched --target":         {Target: "typo"},
		"--dir with only deploys":    {Only: "deploys", Dir: "dev"},
		"--target with only tf":      {Only: "terraform", Target: "dev"},
		"unmatched --dir, tf only":   {Only: "terraform", Dir: "typo"},
		"unmatched --target, apps":   {Only: "deploys", Target: "typo"},
	} {
		if _, err := Select(cfg, roots, f); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestSelect_ConfiguredExclusions(t *testing.T) {
	deploys := map[string]config.Deploy{
		"web": {Target: "dev", Manifests: "m"},
		"db":  {Target: "prod", Manifests: "m"},
	}
	cfg := &config.Config{Deploys: deploys, Drift: config.DriftConfig{
		ExcludeRoots: []config.RootExclusion{
			{Path: "prd", Reason: "CI role can't plan prd", Only: "kestci"},
			{Path: "dev/dns", Reason: "provider needs a VPN"},
		},
		ExcludeDeploys: []config.DeployExclusion{{Name: "db", Reason: "password generated per render", Only: "kest"}},
	}}
	roots := []swoop.Root{{Path: "dev/vpc", Dir: "dev"}, {Path: "dev/dns", Dir: "dev"}, {Path: "prd/vpc", Dir: "prd"}}
	skipped := func(sel Selection) string {
		var s []string
		for _, it := range sel.Skipped {
			if it.Status != Skipped {
				t.Errorf("skipped item has status %q", it.Status)
			}
			s = append(s, it.Kind+" "+it.Name+": "+it.Summary)
		}
		return strings.Join(s, "; ")
	}

	sel, err := Select(cfg, roots, Filter{})
	if err != nil || rootPaths(sel.Roots) != "dev/vpc,prd/vpc" || strings.Join(sel.Deploys, ",") != "web" {
		t.Errorf("kest = %+v, %v", sel, err)
	}
	if got, want := skipped(sel), "root dev/dns: provider needs a VPN; deploy db: password generated per render"; got != want {
		t.Errorf("kest skipped = %q, want %q", got, want)
	}

	sel, err = Select(cfg, roots, Filter{CI: true, ExcludeDeploys: []string{"web"}, ExcludeRoots: []string{"prd/vpc"}})
	if err != nil || rootPaths(sel.Roots) != "dev/vpc" || strings.Join(sel.Deploys, ",") != "db" {
		t.Errorf("kestci = %+v, %v", sel, err)
	}
	// The configured reason wins over a flag naming the same root.
	if got, want := skipped(sel), "root dev/dns: provider needs a VPN; root prd/vpc: CI role can't plan prd; deploy web: --exclude-deploy"; got != want {
		t.Errorf("kestci skipped = %q, want %q", got, want)
	}

	// Roots aren't discovered with --only deploys, so root exclusions aren't checked against them.
	if sel, err := Select(cfg, nil, Filter{Only: "deploys"}); err != nil || len(sel.Skipped) != 1 {
		t.Errorf("--only deploys = %+v, %v", sel, err)
	}

	for name, d := range map[string]config.DriftConfig{
		"unmatched root":        {ExcludeRoots: []config.RootExclusion{{Path: "prd/vp", Reason: "r"}}},
		"unmatched root, other": {ExcludeRoots: []config.RootExclusion{{Path: "typo", Reason: "r", Only: "kestci"}}},
		"unknown deploy":        {ExcludeDeploys: []config.DeployExclusion{{Name: "typo", Reason: "r"}}},
		"unknown deploy, other": {ExcludeDeploys: []config.DeployExclusion{{Name: "typo", Reason: "r", Only: "kestci"}}},
		"no reason":             {ExcludeRoots: []config.RootExclusion{{Path: "prd"}}},
		"blank reason":          {ExcludeDeploys: []config.DeployExclusion{{Name: "db", Reason: " "}}},
		"no path":               {ExcludeRoots: []config.RootExclusion{{Reason: "r"}}},
		"bad only":              {ExcludeRoots: []config.RootExclusion{{Path: "prd", Reason: "r", Only: "ci"}}},
	} {
		if _, err := Select(&config.Config{Deploys: deploys, Drift: d}, roots, Filter{}); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestSkippedItems(t *testing.T) {
	items := []Item{
		{Kind: "root", Name: "dev/vpc", Status: Clean, Summary: "no changes"},
		{Kind: "root", Name: "prd/vpc", Status: Skipped, Summary: "CI role can't plan prd"},
	}
	if err := Result(items); err != nil {
		t.Errorf("Result with a skipped item = %v, want nil", err)
	}
	var b bytes.Buffer
	PrintTable(&b, items)
	for _, want := range []string{"SKIPPED  CI role can't plan prd", "1 checked: 1 clean, 0 drifted, 0 errored; 1 skipped"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("table missing %q:\n%s", want, b.String())
		}
	}
}
