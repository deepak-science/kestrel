package deploy

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/deepak-science/kestrel/internal/config"
	"github.com/deepak-science/kestrel/internal/testutil"
)

// A Deployment last applied by kest, then patched, scaled and restarted by hand.
const managedJSON = `{
  "apiVersion": "v1",
  "kind": "List",
  "items": [{
    "apiVersion": "apps/v1",
    "kind": "Deployment",
    "metadata": {
      "name": "web",
      "managedFields": [
        {"manager": "kest", "fieldsV1": {"f:spec": {"f:replicas": {}, "f:template": {"f:spec": {"f:containers": {"k:{\"name\":\"web\"}": {".": {}, "f:image": {}}}}}}}},
        {"manager": "kubectl-patch", "fieldsV1": {"f:spec": {"f:template": {"f:spec": {"f:nodeSelector": {".": {}, "f:disktype": {}}}}}}},
        {"manager": "kubectl-edit", "fieldsV1": {"f:spec": {"f:template": {"f:spec": {"f:containers": {"k:{\"name\":\"web\"}": {"f:resources": {"f:limits": {"f:cpu": {}}}}}}}}}},
        {"manager": "kubectl-rollout", "fieldsV1": {"f:spec": {"f:template": {"f:metadata": {"f:annotations": {"f:kubectl.kubernetes.io/restartedAt": {}}}}}}},
        {"manager": "kubectl-client-side-apply", "fieldsV1": {"f:metadata": {"f:annotations": {"f:kubectl.kubernetes.io/last-applied-configuration": {}}}}},
        {"manager": "kube-controller-manager", "fieldsV1": {"f:status": {"f:replicas": {}}}}
      ]
    }
  }]
}`

func TestLiveEdits(t *testing.T) {
	got, err := liveEdits([]byte(managedJSON), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"kubectl-patch on deployment.apps/web: spec.template.spec.nodeSelector.disktype",
		"kubectl-edit on deployment.apps/web: spec.template.spec.containers[name=web].resources.limits.cpu",
	}
	if !slices.Equal(got, want) {
		t.Errorf("liveEdits =\n  %q\nwant\n  %q", got, want)
	}
}

// A field git sets but an old patch co-owns with the same value isn't a live edit.
func TestLiveEdits_GitCoOwned(t *testing.T) {
	live := `{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": {"name": "server", "namespace": "app", "managedFields": [
  {"manager": "kubectl-client-side-apply", "fieldsV1": {"f:spec": {"f:template": {"f:spec": {"f:nodeSelector": {}}}}}},
  {"manager": "kubectl-patch", "fieldsV1": {"f:spec": {"f:template": {"f:spec": {"f:nodeSelector": {}, "f:tolerations": {}}}}}}
]}}`
	dry := `{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": {"name": "server", "namespace": "app", "managedFields": [
  {"manager": "kest", "fieldsV1": {"f:spec": {"f:template": {"f:spec": {"f:nodeSelector": {}}}}}},
  {"manager": "kubectl-patch", "fieldsV1": {"f:spec": {"f:template": {"f:spec": {"f:nodeSelector": {}, "f:tolerations": {}}}}}}
]}}`
	got, err := liveEdits([]byte(live), []byte(dry))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"kubectl-patch on deployment.apps/server: spec.template.spec.tolerations"}
	if !slices.Equal(got, want) {
		t.Errorf("liveEdits = %q, want %q", got, want)
	}
}

func TestCoveredBy(t *testing.T) {
	git := []fieldPath{{"f:spec", "f:nodeSelector"}, {"f:spec", "f:replicas"}, {"f:metadata", "f:labels", "f:app"}}
	for _, tc := range []struct {
		path fieldPath
		want bool
	}{
		{fieldPath{"f:spec", "f:nodeSelector"}, true},
		{fieldPath{"f:spec", "f:nodeSelector", "f:disktype"}, true},
		{fieldPath{"f:spec", "f:replicasX"}, false},
		{fieldPath{"f:spec"}, false},
		// A dotted label name is one segment, not a child of the label "app".
		{fieldPath{"f:metadata", "f:labels", "f:app.kubernetes.io/name"}, false},
	} {
		if got := coveredBy(tc.path, git); got != tc.want {
			t.Errorf("coveredBy(%v) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// Git sets label "app"; a live-added "app.kubernetes.io/name" flattens to a lookalike child and must still be reported.
// The same kind and name in another API group has its own field coverage.
func TestLiveEdits_DottedNamesAndGroups(t *testing.T) {
	obj := func(apiVersion, manager, fields string) string {
		return `{"apiVersion": "` + apiVersion + `", "kind": "Widget", "metadata": {"name": "w", "namespace": "app", "managedFields": [{"manager": "` + manager + `", "fieldsV1": ` + fields + `}]}}`
	}
	list := func(items ...string) []byte {
		return []byte(`{"kind": "List", "items": [` + strings.Join(items, ",") + `]}`)
	}
	live := list(
		obj("a.example.com/v1", "kubectl-label", `{"f:metadata": {"f:labels": {"f:app.kubernetes.io/name": {}}}}`),
		obj("b.example.com/v1", "kubectl-patch", `{"f:spec": {"f:size": {}}}`),
	)
	dry := list(
		obj("a.example.com/v1", "kest", `{"f:metadata": {"f:labels": {"f:app": {}}}, "f:spec": {"f:size": {}}}`),
		obj("b.example.com/v1", "kest", `{"f:metadata": {"f:labels": {"f:app": {}}}}`),
	)
	got, err := liveEdits(live, dry)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"kubectl-label on widget.a.example.com/w: metadata.labels.app.kubernetes.io/name",
		"kubectl-patch on widget.b.example.com/w: spec.size",
	}
	if !slices.Equal(got, want) {
		t.Errorf("liveEdits =\n  %q\nwant\n  %q", got, want)
	}
}

func TestLiveEdits_Empty(t *testing.T) {
	got, err := liveEdits([]byte("  \n"), nil)
	if err != nil || got != nil {
		t.Errorf("liveEdits(empty) = %v, %v", got, err)
	}
}

func TestFieldSegment(t *testing.T) {
	for in, want := range map[string]string{
		"f:replicas":                 "replicas",
		`k:{"name":"web","port":80}`: "[name=web,port=80]",
		`v:"foo"`:                    `["foo"]`,
		"i:2":                        "[2]",
	} {
		if got := fieldSegment(in); got != want {
			t.Errorf("fieldSegment(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDiffedObjects(t *testing.T) {
	out := "diff -u -N /tmp/LIVE-1/apps.v1.Deployment.media.web /tmp/MERGED-2/apps.v1.Deployment.media.web\n--- a\n+++ b\n" +
		"diff -u -N /tmp/LIVE-1/v1.Service.media.web /tmp/MERGED-2/v1.Service.media.web\n"
	got := diffedObjects(out)
	want := []string{"apps.v1.Deployment.media.web", "v1.Service.media.web"}
	if !slices.Equal(got, want) {
		t.Errorf("diffedObjects = %v, want %v", got, want)
	}
}

func TestCapList(t *testing.T) {
	if got := capList([]string{"a", "b", "c", "d", "e"}, 3); got != "a, b, c, +2 more" {
		t.Errorf("capList = %q", got)
	}
}

func TestHelmDiffObjects(t *testing.T) {
	out := "media, web, Deployment (apps) has changed:\n  spec...\nmedia, web-extra, ConfigMap (v1) has been added:\nmedia, old, Service (v1) has been removed:\n"
	got := helmDiffObjects(out)
	want := []string{"Deployment/web changed", "ConfigMap/web-extra added", "Service/old removed"}
	if !slices.Equal(got, want) {
		t.Errorf("helmDiffObjects = %v, want %v", got, want)
	}
}

func manifestDir(t *testing.T, diffCode int, diffOut, getJSON string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{
		"fake-diff.code": strconv.Itoa(diffCode),
		"fake-diff.out":  diffOut,
		"fake-get.json":  getJSON,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// fakeKubectl answers `diff -f DIR` and `get -f DIR` from files in DIR; the dry-run apply owns nothing.
const fakeKubectl = `p="$3"
case "$1" in
diff) cat "$p/fake-diff.out"; exit "$(cat "$p/fake-diff.code")" ;;
get) cat "$p/fake-get.json" ;;
apply) echo '{"kind":"List","items":[]}' ;;
esac
`

func TestDrift_Manifest(t *testing.T) {
	testutil.FakeBin(t, "kubectl", fakeKubectl)
	cfg := &config.Config{}

	clean := config.Deploy{Target: "dev", Manifests: manifestDir(t, 0, "", "")}
	rep, err := Drift(cfg, "clean", clean, Resolution{}, Policy{})
	if err != nil || rep.Drift {
		t.Fatalf("clean deploy: %+v, %v", rep, err)
	}

	diffOut := "diff -u -N /tmp/LIVE-1/apps.v1.Deployment.media.web /tmp/MERGED-2/apps.v1.Deployment.media.web\n"
	drifted := config.Deploy{Target: "dev", Manifests: manifestDir(t, 1, diffOut, managedJSON)}
	rep, err = Drift(cfg, "web", drifted, Resolution{}, Policy{})
	if err != nil || !rep.Drift || len(rep.Details) != 3 {
		t.Fatalf("drifted deploy: %+v, %v", rep, err)
	}
	if rep.Details[0] != "differs from git: apps.v1.Deployment.media.web" {
		t.Errorf("first detail = %q", rep.Details[0])
	}

	broken := config.Deploy{Target: "dev", Manifests: manifestDir(t, 2, "", "")}
	if _, err := Drift(cfg, "broken", broken, Resolution{}, Policy{}); err == nil {
		t.Error("kubectl diff exit 2 wasn't an error")
	}
}

// fakeHelm answers status from $FAKE_HELM_STATUS (or "not found" when unset) and diff from $FAKE_HELM_DIFF.
const fakeHelm = `case "$1 $2" in
"diff version") exit 0 ;;
"diff upgrade") printf 'media, web, Deployment (apps) has changed:\n'; exit "${FAKE_HELM_DIFF:-0}" ;;
esac
if [ "$1" = status ]; then
  if [ -z "$FAKE_HELM_STATUS" ]; then echo "Error: release: not found" >&2; exit 1; fi
  echo "$FAKE_HELM_STATUS"
fi
`

func TestDrift_Helm(t *testing.T) {
	testutil.FakeBin(t, "helm", fakeHelm)
	orig := helmDiffAvailable
	t.Cleanup(func() { helmDiffAvailable = orig })
	helmDiffAvailable = func() bool { return true }

	d := config.Deploy{Target: "dev", Chart: "oci://example.com/charts/web", Version: "1.2.0"}
	cfg := &config.Config{}

	rep, err := Drift(cfg, "web", d, Resolution{}, Policy{})
	if err != nil || !rep.Drift || rep.Details[0] != "release web not installed" {
		t.Fatalf("missing release: %+v, %v", rep, err)
	}

	t.Setenv("FAKE_HELM_STATUS", `{"info":{"status":"deployed"},"chart":{"metadata":{"version":"1.2.0"}}}`)
	if rep, err = Drift(cfg, "web", d, Resolution{}, Policy{}); err != nil || rep.Drift {
		t.Fatalf("clean release: %+v, %v", rep, err)
	}

	t.Setenv("FAKE_HELM_STATUS", `{"info":{"status":"failed"},"chart":{"metadata":{"version":"1.1.0"}}}`)
	t.Setenv("FAKE_HELM_DIFF", "2")
	rep, err = Drift(cfg, "web", d, Resolution{}, Policy{})
	want := []string{"release status failed", "chart 1.1.0 deployed, 1.2.0 configured", "differs from config: Deployment/web changed"}
	if err != nil || !rep.Drift || !slices.Equal(rep.Details, want) {
		t.Fatalf("drifted release: %+v, %v", rep, err)
	}

	helmDiffAvailable = func() bool { return false }
	t.Setenv("FAKE_HELM_STATUS", `{"info":{"status":"deployed"},"chart":{"metadata":{"version":"1.2.0"}}}`)
	if rep, err = Drift(cfg, "web", d, Resolution{}, Policy{}); err != nil || rep.Drift || rep.Note == "" {
		t.Fatalf("without helm-diff: %+v, %v", rep, err)
	}
}
