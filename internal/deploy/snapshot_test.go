package deploy

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

var (
	testProvisioners = map[string]string{"fast": "csi.example.com", "local": "rancher.io/local-path", "other": "csi.other.io"}
	testClasses      = []snapClass{
		{Name: "csi-default", Driver: "csi.example.com", Default: true},
		{Name: "csi-alt", Driver: "csi.example.com"},
		{Name: "other-only", Driver: "csi.other.io"},
	}
)

func TestPlanSnapshots_PicksClassPerDriver(t *testing.T) {
	pvcs := []pvcInfo{
		{Name: "data", Namespace: "apps", StorageClass: "fast", Phase: "Bound"},
		{Name: "cache", Namespace: "apps", StorageClass: "other", Phase: "Bound"},
		{Name: "pending", Namespace: "apps", StorageClass: "local", Phase: "Pending"},
	}
	plan, problems := planSnapshots(pvcs, testProvisioners, testClasses, "")
	if len(problems) != 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
	want := []snapTarget{
		{PVC: "data", Namespace: "apps", Class: "csi-default"},
		{PVC: "cache", Namespace: "apps", Class: "other-only"},
	}
	if !reflect.DeepEqual(plan, want) {
		t.Errorf("plan mismatch\n got: %+v\nwant: %+v", plan, want)
	}
}

// A bound volume with no snapshot-capable driver must fail the plan, not be skipped.
func TestPlanSnapshots_UnsnapshotableVolume(t *testing.T) {
	pvcs := []pvcInfo{
		{Name: "data", Namespace: "apps", StorageClass: "fast", Phase: "Bound"},
		{Name: "scratch", Namespace: "apps", StorageClass: "local", Phase: "Bound"},
	}
	_, problems := planSnapshots(pvcs, testProvisioners, testClasses, "")
	if len(problems) != 1 || !strings.Contains(problems[0], "apps/scratch: no VolumeSnapshotClass for driver rancher.io/local-path") {
		t.Errorf("got problems %v", problems)
	}
}

func TestPlanSnapshots_ConfiguredClass(t *testing.T) {
	pvcs := []pvcInfo{{Name: "data", Namespace: "apps", StorageClass: "fast", Phase: "Bound"}}
	plan, problems := planSnapshots(pvcs, testProvisioners, testClasses, "csi-alt")
	if len(problems) != 0 || len(plan) != 1 || plan[0].Class != "csi-alt" {
		t.Errorf("got plan %+v problems %v", plan, problems)
	}

	_, problems = planSnapshots(pvcs, testProvisioners, testClasses, "other-only")
	if len(problems) != 1 || !strings.Contains(problems[0], "doesn't match snapshot_class other-only") {
		t.Errorf("driver mismatch: got %v", problems)
	}

	_, problems = planSnapshots(pvcs, testProvisioners, testClasses, "nope")
	if len(problems) != 1 || !strings.Contains(problems[0], `snapshot_class "nope" not found`) {
		t.Errorf("missing class: got %v", problems)
	}
}

// Several classes for one driver and none marked default is ambiguous.
func TestPlanSnapshots_AmbiguousWithoutDefault(t *testing.T) {
	classes := []snapClass{{Name: "a", Driver: "csi.example.com"}, {Name: "b", Driver: "csi.example.com"}}
	pvcs := []pvcInfo{{Name: "data", Namespace: "apps", StorageClass: "fast", Phase: "Bound"}}
	if _, problems := planSnapshots(pvcs, testProvisioners, classes, ""); len(problems) != 1 {
		t.Errorf("got %v", problems)
	}
}

// `kubectl get kind/name` returns the bare object, not a List; its status.error must still be read.
func TestParseKubeList_SingleObject(t *testing.T) {
	l, err := parseKubeList([]byte(`{"kind":"VolumeSnapshot","metadata":{"name":"s1"},"status":{"error":{"message":"volume busy"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Items) != 1 || l.Items[0].Metadata.Name != "s1" || l.Items[0].Status.Error.Message != "volume busy" {
		t.Errorf("got %+v", l)
	}
	l, err = parseKubeList([]byte(`{"kind":"VolumeSnapshotList","items":[{"metadata":{"name":"a"}},{"metadata":{"name":"b"}}]}`))
	if err != nil || len(l.Items) != 2 {
		t.Errorf("list: got %+v, %v", l, err)
	}
}

func TestSnapshotName(t *testing.T) {
	at := time.Date(2026, 11, 11, 14, 25, 30, 0, time.UTC)
	if got := snapshotName("data-postgres-0", at); got != "data-postgres-0-kest-20261111-142530" {
		t.Errorf("got %q", got)
	}
	if got := snapshotName(strings.Repeat("x", 300), at); len(got) != 253 || !strings.HasSuffix(got, "-kest-20261111-142530") {
		t.Errorf("long name: len %d, %q", len(got), got[len(got)-25:])
	}
}

func TestSnapshotManifest(t *testing.T) {
	data, err := snapshotManifest("gitea", []snapTarget{{PVC: "data", Namespace: "apps", Class: "csi-default", Snapshot: "data-kest-x"}})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Items []struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Metadata   struct {
				Name, Namespace string
				Labels          map[string]string
			} `json:"metadata"`
			Spec struct {
				VolumeSnapshotClassName string `json:"volumeSnapshotClassName"`
				Source                  struct {
					PersistentVolumeClaimName string `json:"persistentVolumeClaimName"`
				} `json:"source"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	it := got.Items[0]
	if it.APIVersion != "snapshot.storage.k8s.io/v1" || it.Kind != "VolumeSnapshot" || it.Metadata.Name != "data-kest-x" ||
		it.Metadata.Namespace != "apps" || it.Metadata.Labels[DeployLabel] != "gitea" ||
		it.Spec.VolumeSnapshotClassName != "csi-default" || it.Spec.Source.PersistentVolumeClaimName != "data" {
		t.Errorf("unexpected manifest: %s", data)
	}
}

func TestSkipPVCs(t *testing.T) {
	pvcs := []pvcInfo{
		{Name: "data-gitea-0", Namespace: "gitea"},
		{Name: "media", Namespace: "gitea"},
		{Name: "media", Namespace: "photos"},
		{Name: "nfs-backups", Namespace: "photos"},
	}
	kept, skipped := skipPVCs(pvcs, []string{"nfs-*", "gitea/media"})
	names := func(ps []pvcInfo) (out []string) {
		for _, p := range ps {
			out = append(out, p.Namespace+"/"+p.Name)
		}
		return out
	}
	if got, want := names(kept), []string{"gitea/data-gitea-0", "photos/media"}; !reflect.DeepEqual(got, want) {
		t.Errorf("kept %v, want %v", got, want)
	}
	if got, want := names(skipped), []string{"gitea/media", "photos/nfs-backups"}; !reflect.DeepEqual(got, want) {
		t.Errorf("skipped %v, want %v", got, want)
	}
}
