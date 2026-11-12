package deploy

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/deepak-science/kestrel/internal/config"
)

func TestHelmArgs_LocalChart(t *testing.T) {
	d := config.Deploy{
		Chart:     "charts/app",
		Values:    []string{"deploys/homepage.yaml"},
		Namespace: "homepage",
		Target:    "homelab",
	}
	res := Resolution{KubeContext: "my-cluster"}
	got := helmArgs("homepage", d, res, ActionApply, nil)
	want := []string{
		"upgrade", "--install", "homepage", "charts/app",
		"--namespace", "homepage", "--create-namespace",
		"--kube-context", "my-cluster",
		"--values", "deploys/homepage.yaml",
		"--atomic", "--cleanup-on-fail", "--timeout", "5m0s",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("helmArgs mismatch\n got: %v\nwant: %v", got, want)
	}
}

func TestHelmArgs_ThirdPartyRepoChart(t *testing.T) {
	d := config.Deploy{
		Chart:   "authentik/authentik",
		Repo:    "https://charts.goauthentik.io",
		Version: "2024.10.1",
		Values:  []string{"deploys/authentik.yaml"},
		Target:  "homelab",
	}
	res := Resolution{KubeContext: "my-cluster", Kubeconfig: "/tmp/kubeconfig"}
	got := helmArgs("authentik", d, res, ActionApply, []string{"--wait"})
	joined := strings.Join(got, " ")
	for _, want := range []string{
		"--repo https://charts.goauthentik.io",
		"--version 2024.10.1",
		"--kubeconfig /tmp/kubeconfig",
		"--values deploys/authentik.yaml",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("helmArgs missing %q in: %s", want, joined)
		}
	}
	if got[len(got)-1] != "--wait" {
		t.Errorf("extra arg not appended last: %v", got)
	}
}

func TestHelmArgs_DiffIsDryRun(t *testing.T) {
	d := config.Deploy{Chart: "charts/app", Target: "homelab"}
	got := helmArgs("app", d, Resolution{}, ActionDiff, nil)
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "--dry-run") {
		t.Errorf("diff should add --dry-run: %s", joined)
	}
	if strings.Contains(joined, "--atomic") {
		t.Errorf("diff must not add apply-only --atomic: %s", joined)
	}
}

func TestHelmArgs_SetDeterministic(t *testing.T) {
	d := config.Deploy{
		Chart:  "charts/app",
		Target: "homelab",
		Set:    map[string]string{"b": "2", "a": "1", "c": "3"},
	}
	got := helmArgs("app", d, Resolution{}, ActionApply, nil)
	// Keys must be emitted in sorted order regardless of map iteration.
	want := []string{"--set", "a=1", "--set", "b=2", "--set", "c=3"}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, strings.Join(want, " ")) {
		t.Errorf("--set not sorted/deterministic: %s", joined)
	}
}

func TestKubectlArgs_Apply(t *testing.T) {
	res := Resolution{KubeContext: "my-cluster"}
	got := kubectlArgs("k8s-manifests/gitea", res, "", kubectlOpts{}, nil)
	want := []string{"apply", "-f", "k8s-manifests/gitea", "--context", "my-cluster"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("kubectlArgs mismatch\n got: %v\nwant: %v", got, want)
	}
}

func TestKubectlArgs_Diff(t *testing.T) {
	res := Resolution{KubeContext: "my-cluster", Kubeconfig: "/tmp/kc"}
	got := kubectlArgs("k8s-manifests/gitea", res, "gitea", kubectlOpts{Diff: true}, []string{"--validate=strict"})
	want := []string{
		"diff", "-f", "k8s-manifests/gitea",
		"--context", "my-cluster",
		"--kubeconfig", "/tmp/kc",
		"--namespace", "gitea",
		"--validate=strict",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("kubectlArgs diff mismatch\n got: %v\nwant: %v", got, want)
	}
}

func TestKubectlArgs_ServerSide(t *testing.T) {
	res := Resolution{KubeContext: "my-cluster"}
	got := kubectlArgs("k8s/app", res, "", kubectlOpts{ServerSide: true, ForceConflicts: true, JSON: true}, nil)
	want := []string{"apply", "-f", "k8s/app", "--context", "my-cluster", "--server-side", "--field-manager=kest", "--force-conflicts", "-o", "json"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("apply mismatch\n got: %v\nwant: %v", got, want)
	}

	// diff takes no -o; it must still diff as kest's field manager.
	got = kubectlArgs("k8s/app", res, "", kubectlOpts{Diff: true, ServerSide: true, JSON: true}, nil)
	want = []string{"diff", "-f", "k8s/app", "--context", "my-cluster", "--server-side", "--field-manager=kest"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("diff mismatch\n got: %v\nwant: %v", got, want)
	}
}

func TestResolve_NamedContextFallback(t *testing.T) {
	// A target whose cluster has no kubernetes.contexts entry falls back
	// to using the cluster name as the context name directly.
	cfg := &config.Config{
		Targets: map[string]config.TargetConfig{
			"homelab": {Cluster: "my-cluster"},
		},
	}
	res, err := Resolve(cfg, "homelab")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.KubeContext != "my-cluster" {
		t.Errorf("KubeContext = %q, want my-cluster", res.KubeContext)
	}
	if res.AwsProfile != "" {
		t.Errorf("AwsProfile = %q, want empty for non-AWS cluster", res.AwsProfile)
	}
}

func TestResolve_MappedContextAndProfile(t *testing.T) {
	cfg := &config.Config{
		Targets: map[string]config.TargetConfig{
			"dev": {Cluster: "eks-dev", AWSAccount: "111122223333"},
		},
		Kubernetes: config.KubernetesConfig{
			Contexts: map[string]string{"eks-dev": "arn:aws:eks:us-east-1:111122223333:cluster/eks-dev"},
		},
		AWS: config.AWSConfig{
			Accounts: map[string]config.AWSAccountConfig{"111122223333": {AwsProfile: "dev-sso"}},
		},
	}
	res, err := Resolve(cfg, "dev")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.KubeContext != "arn:aws:eks:us-east-1:111122223333:cluster/eks-dev" {
		t.Errorf("KubeContext = %q", res.KubeContext)
	}
	if res.AwsProfile != "dev-sso" {
		t.Errorf("AwsProfile = %q, want dev-sso", res.AwsProfile)
	}
	if res.AccountID != "111122223333" {
		t.Errorf("AccountID = %q", res.AccountID)
	}
}

func TestResolve_ExplicitKubeconfigRelativeToProject(t *testing.T) {
	cfg := &config.Config{
		Targets: map[string]config.TargetConfig{
			"homelab": {Cluster: "my-cluster", Kubeconfig: "iac-live/cluster/kubeconfig"},
		},
		Sources: config.Sources{Project: "/repo/.kestconfig"},
	}
	res, err := Resolve(cfg, "homelab")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := filepath.Join("/repo", "iac-live/cluster/kubeconfig")
	if res.Kubeconfig != want {
		t.Errorf("Kubeconfig = %q, want %q", res.Kubeconfig, want)
	}
}

func TestResolve_UnknownTarget(t *testing.T) {
	cfg := &config.Config{}
	if _, err := Resolve(cfg, "nope"); err == nil {
		t.Fatal("expected error for unknown target")
	}
}

func TestDeployKindAndValidate(t *testing.T) {
	tests := []struct {
		name    string
		d       config.Deploy
		kind    string
		wantErr bool
	}{
		{"helm", config.Deploy{Chart: "charts/app", Target: "t"}, config.DeployHelm, false},
		{"manifest", config.Deploy{Manifests: "k8s/app", Target: "t"}, config.DeployManifest, false},
		{"neither", config.Deploy{Target: "t"}, "", true},
		{"both", config.Deploy{Chart: "c", Manifests: "m", Target: "t"}, "", true},
		{"no-target", config.Deploy{Chart: "c"}, config.DeployHelm, true},
		{"server-side-manifest", config.Deploy{Manifests: "k8s/app", Target: "t", ServerSide: &on}, config.DeployManifest, false},
		{"server-side-helm", config.Deploy{Chart: "c", Target: "t", ServerSide: &on}, config.DeployHelm, true},
		{"server-side-off-helm", config.Deploy{Chart: "c", Target: "t", ServerSide: &off}, config.DeployHelm, false},
		{"snapshot-skip", config.Deploy{Chart: "c", Target: "t", SnapshotSkip: []string{"nfs-*", "ns/data"}}, config.DeployHelm, false},
		{"snapshot-skip-bad-glob", config.Deploy{Chart: "c", Target: "t", SnapshotSkip: []string{"data["}}, config.DeployHelm, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.d.Kind(); got != tt.kind {
				t.Errorf("Kind() = %q, want %q", got, tt.kind)
			}
			err := tt.d.Validate(tt.name)
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

var on, off = true, false

func TestUsesServerSide(t *testing.T) {
	tests := []struct {
		name string
		d    config.Deploy
		want bool
	}{
		{"manifest default", config.Deploy{Manifests: "k8s/app"}, true},
		{"manifest opt-out", config.Deploy{Manifests: "k8s/app", ServerSide: &off}, false},
		{"manifest explicit", config.Deploy{Manifests: "k8s/app", ServerSide: &on}, true},
		{"helm", config.Deploy{Chart: "c"}, false},
	}
	for _, tt := range tests {
		if got := tt.d.UsesServerSide(); got != tt.want {
			t.Errorf("%s: UsesServerSide() = %v, want %v", tt.name, got, tt.want)
		}
	}
}
