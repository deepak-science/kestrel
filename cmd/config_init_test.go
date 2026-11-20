package cmd

import (
	"os"
	"path/filepath"
	"strings"
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

func TestDetectOpenTofu(t *testing.T) {
	const tofuLock = "provider \"registry.opentofu.org/hashicorp/aws\" {\n  version = \"5.0.0\"\n}\n"
	const tfLock = "provider \"registry.terraform.io/hashicorp/aws\" {\n  version = \"5.0.0\"\n}\n"

	cases := []struct {
		name  string
		files map[string]string
		roots []string
		want  string
	}{
		{"pin in root", map[string]string{"iac/cluster/.opentofu-version": "1.8.0\n"}, []string{"iac/edge", "iac/cluster"}, "iac/cluster/.opentofu-version"},
		{"pin in ancestor", map[string]string{"iac/.opentofu-version": "1.8.0\n"}, []string{"iac/cluster"}, "iac/.opentofu-version"},
		{"pin at project root", map[string]string{".opentofu-version": "1.8.0\n"}, []string{"iac/cluster"}, ".opentofu-version"},
		{"root at project root", map[string]string{".opentofu-version": "1.8.0\n"}, []string{"."}, ".opentofu-version"},
		{"opentofu lock", map[string]string{"iac/cluster/.terraform.lock.hcl": tofuLock}, []string{"iac/cluster"}, "iac/cluster/.terraform.lock.hcl uses registry.opentofu.org"},
		{"tofu source file", map[string]string{"iac/cluster/override.tofu": ""}, []string{"iac/cluster"}, "iac/cluster/override.tofu"},
		{"terraform pin and lock", map[string]string{"iac/cluster/.terraform-version": "1.9.2\n", "iac/cluster/.terraform.lock.hcl": tfLock}, []string{"iac/cluster"}, ""},
		{"pin outside the roots' ancestry", map[string]string{"other/.opentofu-version": "1.8.0\n"}, []string{"iac/cluster"}, ""},
		{"no roots", map[string]string{".opentofu-version": "1.8.0\n"}, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			project := t.TempDir()
			for rel, content := range tc.files {
				path := filepath.Join(project, rel)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var roots []swoop.Root
			for _, rel := range tc.roots {
				roots = append(roots, swoop.Root{Path: rel, AbsPath: filepath.Join(project, rel)})
			}
			want := tc.want
			if before, after, ok := strings.Cut(want, " "); ok {
				want = filepath.FromSlash(before) + " " + after
			} else {
				want = filepath.FromSlash(want)
			}
			if got := detectOpenTofu(roots, project); got != want {
				t.Errorf("detectOpenTofu = %q, want %q", got, want)
			}
		})
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

func TestDetectChartDeploys_ChartOnly(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "charts", "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "charts", "app", "Chart.yaml"), []byte("name: homepage\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "charts", "app", "values.yaml"))

	deploys := detectChartDeploys(root, []string{"homelab"})

	d, ok := deploys["homepage"]
	if !ok || len(deploys) != 1 {
		t.Fatalf("expected one deploy named after the chart, got %v", keys(deploys))
	}
	if d.Chart != "charts/app" || d.Target != "homelab" || len(d.Values) != 0 {
		t.Errorf("deploy = %+v, want chart charts/app, target homelab, no values", d)
	}
}

func TestChartName_FallsBackToDir(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "charts", "app", "Chart.yaml"))
	if got := chartName(root, "charts/app"); got != "app" {
		t.Errorf("chartName = %q, want app", got)
	}
}

func TestBindSoleTarget(t *testing.T) {
	mk := func() map[string]config.Deploy {
		return map[string]config.Deploy{
			"gitea": {Manifests: "k8s/gitea"},
			"web":   {Chart: "charts/app", Target: "other"},
		}
	}

	one := mk()
	bindSoleTarget(one, map[string]config.TargetConfig{"homelab": {Cluster: "my-cluster"}})
	if one["gitea"].Target != "homelab" || one["web"].Target != "other" {
		t.Errorf("sole target: got gitea=%q web=%q", one["gitea"].Target, one["web"].Target)
	}

	several := mk()
	bindSoleTarget(several, map[string]config.TargetConfig{"a": {}, "b": {}})
	if several["gitea"].Target != "" {
		t.Errorf("several targets: gitea bound to %q, want unbound", several["gitea"].Target)
	}
	if countUnboundDeploys(several) != 1 {
		t.Errorf("countUnboundDeploys = %d, want 1", countUnboundDeploys(several))
	}
}
