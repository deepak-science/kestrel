package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/deepak-science/kestrel/internal/runner"
)

var rolloutKinds = map[string]bool{"Deployment": true, "StatefulSet": true, "DaemonSet": true}

// Spec and Status stay raw so an unrelated object with an unexpected shape can't fail the whole list; rollout() decodes them for workloads only.
type kubeObject struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name            string `json:"name"`
		Namespace       string `json:"namespace"`
		ResourceVersion string `json:"resourceVersion"`
		Generation      int64  `json:"generation"`
	} `json:"metadata"`
	Spec   json.RawMessage `json:"spec"`
	Status json.RawMessage `json:"status"`
	Items  []kubeObject    `json:"items"`
}

type workloadSpec struct {
	Replicas       *int64 `json:"replicas"`
	UpdateStrategy struct {
		Type          string `json:"type"`
		RollingUpdate struct {
			Partition int64 `json:"partition"`
		} `json:"rollingUpdate"`
	} `json:"updateStrategy"`
	Template struct {
		Spec struct {
			Containers []struct {
				Image string `json:"image"`
			} `json:"containers"`
		} `json:"spec"`
	} `json:"template"`
}

type workloadStatus struct {
	ObservedGeneration     int64  `json:"observedGeneration"`
	Replicas               int64  `json:"replicas"`
	UpdatedReplicas        int64  `json:"updatedReplicas"`
	AvailableReplicas      int64  `json:"availableReplicas"`
	ReadyReplicas          int64  `json:"readyReplicas"`
	CurrentRevision        string `json:"currentRevision"`
	UpdateRevision         string `json:"updateRevision"`
	DesiredNumberScheduled int64  `json:"desiredNumberScheduled"`
	UpdatedNumberScheduled int64  `json:"updatedNumberScheduled"`
	NumberAvailable        int64  `json:"numberAvailable"`
}

func (o kubeObject) workload() (workloadSpec, workloadStatus, error) {
	var spec workloadSpec
	var status workloadStatus
	if len(o.Spec) > 0 {
		if err := json.Unmarshal(o.Spec, &spec); err != nil {
			return spec, status, fmt.Errorf("parse %s spec: %w", o.ref(), err)
		}
	}
	if len(o.Status) > 0 {
		if err := json.Unmarshal(o.Status, &status); err != nil {
			return spec, status, fmt.Errorf("parse %s status: %w", o.ref(), err)
		}
	}
	return spec, status, nil
}

type rolloutState int

const (
	rolloutReady rolloutState = iota
	// rolloutRolling: the controller hasn't finished moving pods to the current spec.
	rolloutRolling
	// rolloutDegraded: every pod is on the current spec but not all are available.
	rolloutDegraded
)

// rolloutStatus applies the rules of `kubectl rollout status`, and splits not-ready into rolling and degraded.
// Stale status reads as ready right after an apply, hence the observedGeneration check.
func rolloutStatus(o kubeObject) (rolloutState, string, error) {
	spec, st, err := o.workload()
	if err != nil {
		return 0, "", err
	}
	if st.ObservedGeneration < o.Metadata.Generation {
		return rolloutRolling, "spec change not yet observed", nil
	}
	want := int64(1)
	if spec.Replicas != nil {
		want = *spec.Replicas
	}
	onDelete := spec.UpdateStrategy.Type == "OnDelete"

	switch o.Kind {
	case "Deployment":
		detail := fmt.Sprintf("%d/%d updated, %d available", st.UpdatedReplicas, want, st.AvailableReplicas)
		switch {
		case st.UpdatedReplicas < want, st.Replicas > st.UpdatedReplicas:
			return rolloutRolling, detail, nil
		case st.AvailableReplicas < st.UpdatedReplicas:
			return rolloutDegraded, detail, nil
		}
	case "StatefulSet":
		detail := fmt.Sprintf("%d/%d ready", st.ReadyReplicas, want)
		partition := spec.UpdateStrategy.RollingUpdate.Partition
		// Pods are created in order, so a set still short of its replica count has matching revisions but an unfinished rollout.
		if st.Replicas < want {
			return rolloutRolling, fmt.Sprintf("%s, %d/%d created", detail, st.Replicas, want), nil
		}
		switch {
		case onDelete:
		case partition > 0 && st.UpdatedReplicas < want-partition:
			return rolloutRolling, detail + ", update in progress", nil
		case partition == 0 && st.CurrentRevision != st.UpdateRevision:
			return rolloutRolling, detail + ", update in progress", nil
		}
		if st.ReadyReplicas < want {
			return rolloutDegraded, detail, nil
		}
	case "DaemonSet":
		detail := fmt.Sprintf("%d/%d updated, %d available", st.UpdatedNumberScheduled, st.DesiredNumberScheduled, st.NumberAvailable)
		switch {
		case !onDelete && st.UpdatedNumberScheduled < st.DesiredNumberScheduled:
			return rolloutRolling, detail, nil
		case st.NumberAvailable < st.DesiredNumberScheduled:
			return rolloutDegraded, detail, nil
		}
	}
	return rolloutReady, "", nil
}

// parseObjects flattens `kubectl ... -o json` output: a List, a bare object, or nothing (get --ignore-not-found with no matches).
func parseObjects(data []byte) ([]kubeObject, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, nil
	}
	var obj kubeObject
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, fmt.Errorf("parse kubectl output: %w", err)
	}
	if obj.Kind == "List" {
		return obj.Items, nil
	}
	return []kubeObject{obj}, nil
}

func clusterArgs(res Resolution) []string {
	var args []string
	if res.KubeContext != "" {
		args = append(args, "--context", res.KubeContext)
	}
	if res.Kubeconfig != "" {
		args = append(args, "--kubeconfig", res.Kubeconfig)
	}
	return args
}

func getWorkloadsArgs(path string, res Resolution, namespace string) []string {
	args := append([]string{"get", "-f", path, "-o", "json"}, clusterArgs(res)...)
	if namespace != "" {
		args = append(args, "--namespace", namespace)
	}
	return args
}

// getObjects reads the manifests' live objects; ignoreMissing tolerates ones not created yet.
func getObjects(path string, res Resolution, namespace string, ignoreMissing bool, opts runner.Options) ([]kubeObject, error) {
	args := getWorkloadsArgs(path, res, namespace)
	if ignoreMissing {
		args = append(args, "--ignore-not-found")
	}
	opts.CaptureStdout = true
	opts.Silent = true
	got, err := runner.RunWithOpts("kubectl", args, opts)
	if err != nil {
		return nil, errors.New(runner.ErrorExcerpt(got.Stderr, err))
	}
	return parseObjects([]byte(got.Stdout))
}

// baseline is the manifests' objects as they were before the apply.
// A nil baseline means the snapshot failed, so nothing is known about what the apply changed.
type baseline struct {
	objects []kubeObject
}

type waitSpec struct {
	Timeout time.Duration
	// All waits on every workload and fails on any that isn't ready, whether or not the apply touched it.
	All      bool
	Interval time.Duration
}

// trackedWorkload is one workload's place in the wait: whether it is waited on, and why.
type trackedWorkload struct {
	key    string
	name   string
	images []string
	wait   bool
	// note says what was already wrong before the apply.
	note string
	// preexisting: unchanged and already degraded, so reported rather than waited on.
	preexisting bool
	state       rolloutState
	detail      string
}

// planWait decides which workloads the deploy answers for.
// It waits on those the apply created or changed (metadata.generation moved; resourceVersion also moves on status writes), and on unchanged ones a previous rollout left unfinished.
// An unchanged workload that was already degraded is reported, not waited on.
func planWait(base *baseline, all bool, now []kubeObject) ([]*trackedWorkload, error) {
	prior := map[string]kubeObject{}
	if base != nil {
		for _, o := range base.objects {
			prior[o.key()] = o
		}
	}

	var out []*trackedWorkload
	for _, o := range now {
		if !rolloutKinds[o.Kind] {
			continue
		}
		spec, _, err := o.workload()
		if err != nil {
			return nil, err
		}
		t := &trackedWorkload{key: o.key(), name: o.Metadata.Namespace + "/" + strings.ToLower(o.Kind) + "/" + o.Metadata.Name}
		for _, c := range spec.Template.Spec.Containers {
			t.images = append(t.images, c.Image)
		}
		out = append(out, t)

		was, existed := prior[o.key()]
		if base == nil || !existed {
			t.wait = true
			continue
		}
		state, _, err := rolloutStatus(was)
		if err != nil {
			return nil, err
		}
		changed := was.Metadata.Generation != o.Metadata.Generation
		switch {
		case changed:
			t.wait = true
			if state != rolloutReady {
				t.note = "not ready before apply"
			}
		case state == rolloutRolling:
			t.wait = true
			t.note = "unfinished rollout before apply"
		case state == rolloutDegraded:
			t.wait = all
			t.note = "not ready before apply"
			t.preexisting = !all
		default:
			t.wait = all
		}
	}
	return out, nil
}

// waitForRollouts polls the applied manifests' workloads under one deadline, then prints each workload's images.
// Only a workload that was waited on and isn't ready by the deadline fails the deploy.
// fetch gets the deadline's context so a stalled poll can't outlast it.
func waitForRollouts(fetch func(context.Context) ([]kubeObject, error), base *baseline, spec waitSpec, out io.Writer) error {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(out, format, a...) }
	ctx, cancel := context.WithTimeout(context.Background(), spec.Timeout)
	defer cancel()

	objs, err := fetch(ctx)
	if err != nil {
		return fmt.Errorf("list applied workloads: %w", err)
	}
	workloads, err := planWait(base, spec.All, objs)
	if err != nil {
		return err
	}
	if len(workloads) == 0 {
		p("info: no Deployments/StatefulSets/DaemonSets to wait on\n")
		return nil
	}

	waiting := 0
	for _, w := range workloads {
		if w.wait {
			waiting++
		}
	}
	if waiting == 0 {
		p("info: the apply changed no workloads; nothing to wait on\n")
	} else {
		p("waiting on %d of %d workload(s), up to %s\n", waiting, len(workloads), spec.Timeout)
	}

	var pollErr error
	for waiting > 0 {
		live := map[string]kubeObject{}
		for _, o := range objs {
			live[o.key()] = o
		}
		pending := 0
		for _, w := range workloads {
			o, ok := live[w.key]
			if !w.wait || !ok {
				continue
			}
			state, detail, err := rolloutStatus(o)
			if err != nil {
				return err
			}
			if state != rolloutReady {
				pending++
				if detail != w.detail {
					p("  waiting: %s: %s\n", w.name, detail)
				}
			}
			w.state, w.detail = state, detail
		}
		if pending == 0 {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(spec.Interval):
		}
		if ctx.Err() != nil {
			break
		}
		// A failed poll keeps the last good view; it only matters if the deadline passes first.
		next, err := fetch(ctx)
		switch {
		case err == nil:
			objs, pollErr = next, nil
		case ctx.Err() == nil:
			pollErr = err
		}
	}

	var failed, preexisting []string
	p("\nrollout summary:\n")
	for _, w := range workloads {
		status := "unchanged"
		var notes []string
		switch {
		case w.preexisting:
			status = "NOT READY"
			notes = append(notes, w.note+", not waited on")
			preexisting = append(preexisting, w.name)
		case w.wait && w.state != rolloutReady:
			status = "FAILED"
			notes = append(notes, w.detail)
			failed = append(failed, w.name)
		case w.wait:
			status = "ready"
		}
		if w.note != "" && !w.preexisting {
			notes = append(notes, w.note)
		}
		line := fmt.Sprintf("  %-9s %s  %s", status, w.name, strings.Join(w.images, ", "))
		if len(notes) > 0 {
			line += "  (" + strings.Join(notes, "; ") + ")"
		}
		p("%s\n", line)
	}

	if len(preexisting) > 0 {
		p("warning: not ready before the apply and unchanged by it (--wait-all fails on these): %s\n", strings.Join(preexisting, ", "))
	}
	if len(failed) > 0 {
		err := fmt.Errorf("rollout not ready within %s: %s", spec.Timeout, strings.Join(failed, ", "))
		if pollErr != nil {
			err = fmt.Errorf("%w (last status poll failed: %v)", err, pollErr)
		}
		return err
	}
	return nil
}
