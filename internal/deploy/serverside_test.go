package deploy

import (
	"bytes"
	"reflect"
	"testing"
)

func obj(apiVersion, kind, ns, name, rv string) kubeObject {
	var o kubeObject
	o.APIVersion, o.Kind = apiVersion, kind
	o.Metadata.Namespace, o.Metadata.Name, o.Metadata.ResourceVersion = ns, name, rv
	return o
}

func TestClassifyApply(t *testing.T) {
	before := []kubeObject{
		obj("v1", "ConfigMap", "apps", "cfg", "595"),
		obj("apps/v1", "Deployment", "apps", "web", "608"),
		// Same name in another namespace must not be confused with the applied one.
		obj("v1", "Service", "other", "web", "10"),
	}
	after := []kubeObject{
		obj("v1", "ConfigMap", "apps", "cfg", "595"),
		obj("apps/v1", "Deployment", "apps", "web", "731"),
		obj("v1", "Service", "apps", "web", "732"),
		obj("rbac.authorization.k8s.io/v1", "ClusterRole", "", "reader", "733"),
	}
	got := classifyApply(before, after)
	want := []applyChange{
		{"configmap/cfg", "unchanged"},
		{"deployment.apps/web", "changed"},
		{"service/web", "created"},
		{"clusterrole.rbac.authorization.k8s.io/reader", "created"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("classifyApply mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

func TestPrintApplyChanges(t *testing.T) {
	var buf bytes.Buffer
	printApplyChanges(&buf, []applyChange{{"configmap/cfg", "unchanged"}, {"deployment.apps/web", "changed"}})
	want := "configmap/cfg unchanged\ndeployment.apps/web changed\n\n0 created, 1 changed, 1 unchanged\n"
	if buf.String() != want {
		t.Errorf("got %q, want %q", buf.String(), want)
	}
}

// `kubectl get --ignore-not-found` prints nothing when no object exists yet.
func TestParseObjects_Empty(t *testing.T) {
	got, err := parseObjects([]byte("\n"))
	if err != nil || got != nil {
		t.Errorf("got %v, %v; want nil, nil", got, err)
	}
}
