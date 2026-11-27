package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func workload(t *testing.T, kind, name string, gen int64, spec, status string) kubeObject {
	t.Helper()
	data := fmt.Sprintf(`{"apiVersion":"apps/v1","kind":%q,"metadata":{"name":%q,"namespace":"apps","generation":%d},"spec":%s,"status":%s}`, kind, name, gen, spec, status)
	objs, err := parseObjects([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	return objs[0]
}

const (
	deployReady    = `{"observedGeneration":1,"replicas":2,"updatedReplicas":2,"availableReplicas":2}`
	deployDegraded = `{"observedGeneration":1,"replicas":2,"updatedReplicas":2,"availableReplicas":0}`
	deployRolling  = `{"observedGeneration":1,"replicas":3,"updatedReplicas":1,"availableReplicas":2}`
	twoReplicas    = `{"replicas":2,"template":{"spec":{"containers":[{"image":"web:1.2"}]}}}`
)

func TestRolloutStatus(t *testing.T) {
	for name, tc := range map[string]struct {
		kind         string
		gen          int64
		spec, status string
		want         rolloutState
	}{
		"deployment ready":            {"Deployment", 1, twoReplicas, deployReady, rolloutReady},
		"deployment stale status":     {"Deployment", 2, twoReplicas, deployReady, rolloutRolling},
		"deployment old pods remain":  {"Deployment", 1, twoReplicas, deployRolling, rolloutRolling},
		"deployment updated, down":    {"Deployment", 1, twoReplicas, deployDegraded, rolloutDegraded},
		"deployment scaled to zero":   {"Deployment", 1, `{"replicas":0}`, `{"observedGeneration":1}`, rolloutReady},
		"deployment default replicas": {"Deployment", 1, `{}`, `{"observedGeneration":1}`, rolloutRolling},
		"statefulset ready":           {"StatefulSet", 1, `{"replicas":1}`, `{"observedGeneration":1,"replicas":1,"readyReplicas":1,"currentRevision":"a","updateRevision":"a"}`, rolloutReady},
		"statefulset mid-update":      {"StatefulSet", 1, `{"replicas":1}`, `{"observedGeneration":1,"replicas":1,"readyReplicas":1,"currentRevision":"a","updateRevision":"b"}`, rolloutRolling},
		"statefulset down":            {"StatefulSet", 1, `{"replicas":1}`, `{"observedGeneration":1,"replicas":1,"readyReplicas":0,"currentRevision":"a","updateRevision":"a"}`, rolloutDegraded},
		"statefulset still creating":  {"StatefulSet", 1, `{"replicas":3}`, `{"observedGeneration":1,"replicas":1,"readyReplicas":0,"currentRevision":"a","updateRevision":"a"}`, rolloutRolling},
		"statefulset partition held":  {"StatefulSet", 1, `{"replicas":3,"updateStrategy":{"rollingUpdate":{"partition":2}}}`, `{"observedGeneration":1,"replicas":3,"readyReplicas":3,"updatedReplicas":1,"currentRevision":"a","updateRevision":"b"}`, rolloutReady},
		"statefulset OnDelete":        {"StatefulSet", 1, `{"replicas":1,"updateStrategy":{"type":"OnDelete"}}`, `{"observedGeneration":1,"replicas":1,"readyReplicas":1,"currentRevision":"a","updateRevision":"b"}`, rolloutReady},
		"daemonset ready":             {"DaemonSet", 1, `{}`, `{"observedGeneration":1,"desiredNumberScheduled":3,"updatedNumberScheduled":3,"numberAvailable":3}`, rolloutReady},
		"daemonset mid-update":        {"DaemonSet", 1, `{}`, `{"observedGeneration":1,"desiredNumberScheduled":3,"updatedNumberScheduled":1,"numberAvailable":3}`, rolloutRolling},
		"daemonset down":              {"DaemonSet", 1, `{}`, `{"observedGeneration":1,"desiredNumberScheduled":3,"updatedNumberScheduled":3,"numberAvailable":2}`, rolloutDegraded},
	} {
		got, _, err := rolloutStatus(workload(t, tc.kind, "w", tc.gen, tc.spec, tc.status))
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if got != tc.want {
			t.Errorf("%s: state = %d, want %d", name, got, tc.want)
		}
	}
}

// An object kest doesn't track may put anything under spec; it must not fail the parse.
func TestParseObjects_ForeignSpecShape(t *testing.T) {
	data := `{"kind":"List","items":[
		{"apiVersion":"example.com/v1","kind":"Widget","metadata":{"name":"w"},"spec":{"replicas":"three","template":"inline"}},
		{"apiVersion":"apps/v1","kind":"DaemonSet","metadata":{"name":"agent","namespace":"kube-system"},
		 "spec":{"template":{"spec":{"containers":[{"image":"agent:9"}]}}}}
	]}`
	objs, err := parseObjects([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planWait(nil, false, objs)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 1 || plan[0].name != "kube-system/daemonset/agent" || !reflect.DeepEqual(plan[0].images, []string{"agent:9"}) {
		t.Errorf("unexpected plan: %+v", plan)
	}
}

func TestParseObjects_BadJSON(t *testing.T) {
	if _, err := parseObjects([]byte("not json")); err == nil {
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

func TestPlanWait(t *testing.T) {
	before := []kubeObject{
		workload(t, "Deployment", "changed", 1, twoReplicas, deployReady),
		workload(t, "Deployment", "steady", 1, twoReplicas, deployReady),
		workload(t, "Deployment", "degraded", 1, twoReplicas, deployDegraded),
		workload(t, "Deployment", "rolling", 1, twoReplicas, deployRolling),
		workload(t, "Deployment", "changed-degraded", 1, twoReplicas, deployDegraded),
	}
	now := []kubeObject{
		workload(t, "Deployment", "changed", 2, twoReplicas, deployReady),
		workload(t, "Deployment", "steady", 1, twoReplicas, deployReady),
		workload(t, "Deployment", "degraded", 1, twoReplicas, deployDegraded),
		workload(t, "Deployment", "rolling", 1, twoReplicas, deployRolling),
		workload(t, "Deployment", "changed-degraded", 2, twoReplicas, deployDegraded),
		workload(t, "Deployment", "created", 1, twoReplicas, `{}`),
	}

	waited := func(base *baseline, all bool) map[string]bool {
		t.Helper()
		plan, err := planWait(base, all, now)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, w := range plan {
			out[strings.TrimPrefix(w.name, "apps/deployment/")] = w.wait
		}
		return out
	}

	got := waited(&baseline{objects: before}, false)
	want := map[string]bool{"changed": true, "steady": false, "degraded": false, "rolling": true, "changed-degraded": true, "created": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("default plan\n got: %v\nwant: %v", got, want)
	}
	for name, got := range map[string]map[string]bool{"--wait-all": waited(&baseline{objects: before}, true), "no baseline": waited(nil, false)} {
		for w, wait := range got {
			if !wait {
				t.Errorf("%s: %s not waited on", name, w)
			}
		}
	}
}

func testWait(t *testing.T, base *baseline, all bool, polls ...[]kubeObject) (string, error) {
	t.Helper()
	var out bytes.Buffer
	i := 0
	fetch := func(context.Context) ([]kubeObject, error) {
		objs := polls[min(i, len(polls)-1)]
		i++
		return objs, nil
	}
	err := waitForRollouts(fetch, base, waitSpec{Timeout: 30 * time.Millisecond, All: all, Interval: time.Millisecond}, &out)
	return out.String(), err
}

func TestWaitForRollouts_ChangedBecomesReady(t *testing.T) {
	base := &baseline{objects: []kubeObject{
		workload(t, "Deployment", "web", 1, twoReplicas, deployReady),
		workload(t, "Deployment", "stuck", 1, twoReplicas, deployDegraded),
	}}
	stuck := workload(t, "Deployment", "stuck", 1, twoReplicas, deployDegraded)
	out, err := testWait(t, base, false,
		[]kubeObject{workload(t, "Deployment", "web", 2, twoReplicas, deployReady), stuck},
		[]kubeObject{workload(t, "Deployment", "web", 2, twoReplicas, `{"observedGeneration":2,"replicas":2,"updatedReplicas":2,"availableReplicas":2}`), stuck},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v\n%s", err, out)
	}
	for _, want := range []string{
		"waiting on 1 of 2 workload(s)",
		"ready     apps/deployment/web  web:1.2",
		"NOT READY apps/deployment/stuck  web:1.2  (not ready before apply, not waited on)",
		"warning: not ready before the apply and unchanged by it (--wait-all fails on these): apps/deployment/stuck",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestWaitForRollouts_ChangedTimesOut(t *testing.T) {
	base := &baseline{objects: []kubeObject{workload(t, "Deployment", "web", 1, twoReplicas, deployReady)}}
	out, err := testWait(t, base, false, []kubeObject{workload(t, "Deployment", "web", 2, twoReplicas, `{"observedGeneration":2,"replicas":3,"updatedReplicas":1,"availableReplicas":2}`)})
	if err == nil || !strings.Contains(err.Error(), "rollout not ready within 30ms: apps/deployment/web") {
		t.Fatalf("expected a timeout error, got %v", err)
	}
	if !strings.Contains(out, "FAILED    apps/deployment/web  web:1.2  (1/2 updated, 2 available)") {
		t.Errorf("summary missing the failed workload:\n%s", out)
	}
	if n := strings.Count(out, "waiting: apps/deployment/web"); n != 1 {
		t.Errorf("progress line printed %d times, want once per change:\n%s", n, out)
	}
}

// Retrying a deploy whose StatefulSet never finished creating its pods must wait on it again, though the re-apply changes nothing.
func TestWaitForRollouts_RetryWaitsOnHalfCreatedStatefulSet(t *testing.T) {
	objs := []kubeObject{workload(t, "StatefulSet", "db", 1, `{"replicas":3}`, `{"observedGeneration":1,"replicas":1,"readyReplicas":0,"currentRevision":"a","updateRevision":"a"}`)}
	out, err := testWait(t, &baseline{objects: objs}, false, objs)
	if err == nil || !strings.Contains(err.Error(), "apps/statefulset/db") {
		t.Fatalf("expected the retry to fail on the half-created StatefulSet, got %v\n%s", err, out)
	}
	if !strings.Contains(out, "unfinished rollout before apply") {
		t.Errorf("summary missing the reason:\n%s", out)
	}
}

// The deadline bounds the sleep between polls, and no poll starts after it.
func TestWaitForRollouts_DeadlineBoundsSleepAndPolls(t *testing.T) {
	base := &baseline{objects: []kubeObject{workload(t, "Deployment", "web", 1, twoReplicas, deployReady)}}
	calls := 0
	fetch := func(ctx context.Context) ([]kubeObject, error) {
		calls++
		if _, ok := ctx.Deadline(); !ok {
			t.Error("fetch got a context without the wait deadline")
		}
		if calls > 1 {
			return []kubeObject{workload(t, "Deployment", "web", 2, twoReplicas, `{"observedGeneration":2,"replicas":2,"updatedReplicas":2,"availableReplicas":2}`)}, nil
		}
		return []kubeObject{workload(t, "Deployment", "web", 2, twoReplicas, deployReady)}, nil
	}
	var out bytes.Buffer
	start := time.Now()
	err := waitForRollouts(fetch, base, waitSpec{Timeout: 20 * time.Millisecond, Interval: time.Hour}, &out)
	if err == nil {
		t.Fatalf("expected a timeout, got success:\n%s", out.String())
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("wait ran %s past a 20ms deadline", elapsed)
	}
	if calls != 1 {
		t.Errorf("fetch called %d times, want no poll after the deadline", calls)
	}
}

func TestWaitForRollouts_NothingChanged(t *testing.T) {
	objs := []kubeObject{workload(t, "Deployment", "web", 1, twoReplicas, deployReady)}
	out, err := testWait(t, &baseline{objects: objs}, false, objs)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "the apply changed no workloads") || !strings.Contains(out, "unchanged apps/deployment/web  web:1.2") {
		t.Errorf("unexpected output:\n%s", out)
	}
}

func TestWaitForRollouts_WaitAllFailsOnPreexisting(t *testing.T) {
	objs := []kubeObject{workload(t, "Deployment", "stuck", 1, twoReplicas, deployDegraded)}
	_, err := testWait(t, &baseline{objects: objs}, true, objs)
	if err == nil || !strings.Contains(err.Error(), "apps/deployment/stuck") {
		t.Fatalf("expected --wait-all to fail on the degraded workload, got %v", err)
	}
}

// A transient poll failure mid-wait must not fail a rollout that then completes.
func TestWaitForRollouts_ToleratesPollError(t *testing.T) {
	base := &baseline{objects: []kubeObject{workload(t, "Deployment", "web", 1, twoReplicas, deployReady)}}
	calls := 0
	fetch := func(context.Context) ([]kubeObject, error) {
		calls++
		switch calls {
		case 1:
			return []kubeObject{workload(t, "Deployment", "web", 2, twoReplicas, deployReady)}, nil
		case 2:
			return nil, errors.New("connection refused")
		}
		return []kubeObject{workload(t, "Deployment", "web", 2, twoReplicas, `{"observedGeneration":2,"replicas":2,"updatedReplicas":2,"availableReplicas":2}`)}, nil
	}
	var out bytes.Buffer
	if err := waitForRollouts(fetch, base, waitSpec{Timeout: time.Second, Interval: time.Millisecond}, &out); err != nil {
		t.Fatalf("unexpected error: %v\n%s", err, out.String())
	}
}
