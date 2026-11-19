package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/deepak-science/kestrel/internal/config"
	"github.com/deepak-science/kestrel/internal/swoop"
)

func keys(m map[string]config.Deploy) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("kind: ConfigMap\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDetectManifestDeploys_OrderedSubdirs(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "k8s", "00-namespace", "ns.yaml"))
	writeFile(t, filepath.Join(root, "k8s", "10-authentik", "deploy.yaml"))

	deploys := detectManifestDeploys(root, "homelab")
	if len(deploys) != 2 {
		t.Fatalf("expected 2 deploys, got %d: %v", len(deploys), deploys)
	}

	ns, ok := deploys["namespace"]
	if !ok {
		t.Fatalf("expected deploy 'namespace' (NN- prefix stripped), got %v", keys(deploys))
	}
	if ns.Manifests != filepath.Join("k8s", "00-namespace") {
		t.Errorf("namespace.manifests = %q", ns.Manifests)
	}
	if ns.Target != "homelab" {
		t.Errorf("namespace.target = %q, want homelab", ns.Target)
	}
	if _, ok := deploys["authentik"]; !ok {
		t.Errorf("expected deploy 'authentik', got %v", keys(deploys))
	}
}

func TestDetectManifestDeploys_RootLevelYAML(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "manifests", "app.yaml"))

	deploys := detectManifestDeploys(root, "")
	d, ok := deploys["manifests"]
	if !ok || d.Manifests != "manifests" {
		t.Fatalf("expected single deploy named after the root dir, got %v", deploys)
	}
}

func TestDetectChartDeploys_ValuesConvention(t *testing.T) {
	root := t.TempDir()
	// A local chart so chart: gets filled in.
	writeFile(t, filepath.Join(root, "charts", "app", "Chart.yaml"))
	for _, f := range []string{"shared.yaml", "dev.yaml", "dev-customer.yaml", "prod.yaml"} {
		writeFile(t, filepath.Join(root, "charts", f))
	}

	deploys := detectChartDeploys(root, []string{"dev", "prod"})

	// dev has an instance file → one deploy per instance, layering dev.yaml first.
	cust, ok := deploys["customer"]
	if !ok {
		t.Fatalf("expected deploy 'customer', got %v", keys(deploys))
	}
	if cust.Chart != "charts/app" {
		t.Errorf("customer.chart = %q, want charts/app", cust.Chart)
	}
	if cust.Target != "dev" {
		t.Errorf("customer.target = %q, want dev", cust.Target)
	}
	want := []string{filepath.Join("charts", "dev.yaml"), filepath.Join("charts", "dev-customer.yaml")}
	if len(cust.Values) != 2 || cust.Values[0] != want[0] || cust.Values[1] != want[1] {
		t.Errorf("customer.values = %v, want %v", cust.Values, want)
	}

	// prod has only a bare prod.yaml → one deploy named after the target.
	if prod, ok := deploys["prod"]; !ok || prod.Target != "prod" {
		t.Errorf("expected deploy 'prod' targeting prod, got %v", deploys["prod"])
	}
}

func TestCommonRootDir(t *testing.T) {
	mk := func(paths ...string) []swoop.Root {
		rs := make([]swoop.Root, len(paths))
		for i, p := range paths {
			rs[i] = swoop.Root{Path: p}
		}
		return rs
	}
	cases := []struct {
		name  string
		roots []swoop.Root
		want  string
	}{
		{"underscore dir, flat", mk("iac_live/proxmox", "iac_live/cloudflare"), "iac_live"},
		{"nested at varying depth", mk("iac_live/cluster/proxmox", "iac_live/edge/cf"), "iac_live"},
		{"live convention parent", mk("misc/iac/live/dev", "misc/iac/live/prod"), "misc/iac/live"},
		{"root at repo top → empty", mk("proxmox", "iac_live/cf"), ""},
		{"single nested root", mk("iac_live/cluster/proxmox"), filepath.Join("iac_live", "cluster")},
	}
	for _, tc := range cases {
		if got := commonRootDir(tc.roots); got != tc.want {
			t.Errorf("%s: commonRootDir = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestUsesOpenTofu(t *testing.T) {
	root := t.TempDir()
	tofuRoot := filepath.Join(root, "cluster")
	plainRoot := filepath.Join(root, "edge")
	writeFile(t, filepath.Join(tofuRoot, ".opentofu-version"))
	writeFile(t, filepath.Join(plainRoot, ".terraform-version"))

	if !usesOpenTofu([]swoop.Root{{AbsPath: tofuRoot}, {AbsPath: plainRoot}}) {
		t.Error("expected tofu detection when a root has .opentofu-version")
	}
	if usesOpenTofu([]swoop.Root{{AbsPath: plainRoot}}) {
		t.Error("did not expect tofu detection for .terraform-version only")
	}
}

func TestStripOrderPrefix(t *testing.T) {
	cases := map[string]string{
		"00-namespace": "namespace",
		"10-authentik": "authentik",
		"authentik":    "authentik",
		"3-app":        "app",
		"123":          "123", // digits with no trailing dash are left alone
		"-leading":     "-leading",
	}
	for in, want := range cases {
		if got := stripOrderPrefix(in); got != want {
			t.Errorf("stripOrderPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}
