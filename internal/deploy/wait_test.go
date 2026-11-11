package deploy

import (
	"reflect"
	"testing"
	"time"
)

func TestParseWorkloads_List(t *testing.T) {
	data := `{"kind":"List","items":[
		{"kind":"Deployment","metadata":{"name":"web","namespace":"apps"},
		 "spec":{"template":{"spec":{"containers":[{"image":"web:1.2"},{"image":"sidecar:3"}]}}}},
		{"kind":"Service","metadata":{"name":"web","namespace":"apps"},"spec":{}},
		{"kind":"StatefulSet","metadata":{"name":"db","namespace":"apps"},
		 "spec":{"template":{"spec":{"containers":[{"image":"postgres:17"}]}}}},
		{"kind":"PersistentVolume","metadata":{"name":"data"},"spec":{}}
	]}`
	got, err := parseWorkloads([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	want := []Workload{
		{Kind: "Deployment", Name: "web", Namespace: "apps", Images: []string{"web:1.2", "sidecar:3"}},
		{Kind: "StatefulSet", Name: "db", Namespace: "apps", Images: []string{"postgres:17"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseWorkloads mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

// kubectl get -f on a single object returns the object itself, not a List.
func TestParseWorkloads_SingleObject(t *testing.T) {
	data := `{"kind":"DaemonSet","metadata":{"name":"agent","namespace":"kube-system"},
		"spec":{"template":{"spec":{"containers":[{"image":"agent:9"}]}}}}`
	got, err := parseWorkloads([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	want := []Workload{{Kind: "DaemonSet", Name: "agent", Namespace: "kube-system", Images: []string{"agent:9"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseWorkloads mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

func TestParseWorkloads_NoWorkloads(t *testing.T) {
	got, err := parseWorkloads([]byte(`{"kind":"ConfigMap","metadata":{"name":"cfg"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected no workloads, got %+v", got)
	}
}

func TestParseWorkloads_BadJSON(t *testing.T) {
	if _, err := parseWorkloads([]byte("not json")); err == nil {
		t.Fatal("expected error")
	}
}

func TestGetWorkloadsArgs(t *testing.T) {
	res := Resolution{KubeContext: "my-cluster", Kubeconfig: "/p/kubeconfig"}
	got := getWorkloadsArgs("k8s/app", res, "apps")
	want := []string{"get", "-f", "k8s/app", "-o", "json", "--context", "my-cluster", "--kubeconfig", "/p/kubeconfig", "--namespace", "apps"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("getWorkloadsArgs mismatch\n got: %v\nwant: %v", got, want)
	}
}

func TestRolloutStatusArgs(t *testing.T) {
	w := Workload{Kind: "StatefulSet", Name: "db", Namespace: "apps"}
	got := rolloutStatusArgs(w, Resolution{KubeContext: "my-cluster"}, 5*time.Minute)
	want := []string{"rollout", "status", "statefulset/db", "--timeout", "5m0s", "--namespace", "apps", "--context", "my-cluster"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rolloutStatusArgs mismatch\n got: %v\nwant: %v", got, want)
	}
}
