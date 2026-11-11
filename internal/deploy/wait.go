package deploy

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/deepak-science/kestrel/internal/runner"
)

// Workload is a rollout-tracked object from a manifest deploy.
type Workload struct {
	Kind      string
	Name      string
	Namespace string
	Images    []string
}

func (w Workload) ref() string { return strings.ToLower(w.Kind) + "/" + w.Name }

var rolloutKinds = map[string]bool{"Deployment": true, "StatefulSet": true, "DaemonSet": true}

type kubeObject struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name            string `json:"name"`
		Namespace       string `json:"namespace"`
		ResourceVersion string `json:"resourceVersion"`
	} `json:"metadata"`
	Spec struct {
		Template struct {
			Spec struct {
				Containers []struct {
					Image string `json:"image"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	Items []kubeObject `json:"items"`
}

// parseWorkloads extracts rollout-tracked workloads from `kubectl get -o json`
// output, which is a List for multiple objects and a bare object for one.
func parseWorkloads(data []byte) ([]Workload, error) {
	objs, err := parseObjects(data)
	if err != nil {
		return nil, err
	}

	var out []Workload
	for _, o := range objs {
		if !rolloutKinds[o.Kind] {
			continue
		}
		w := Workload{Kind: o.Kind, Name: o.Metadata.Name, Namespace: o.Metadata.Namespace}
		for _, c := range o.Spec.Template.Spec.Containers {
			w.Images = append(w.Images, c.Image)
		}
		out = append(out, w)
	}
	return out, nil
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

func rolloutStatusArgs(w Workload, res Resolution, timeout time.Duration) []string {
	args := []string{"rollout", "status", w.ref(), "--timeout", timeout.String()}
	if w.Namespace != "" {
		args = append(args, "--namespace", w.Namespace)
	}
	return append(args, clusterArgs(res)...)
}

// waitForRollouts runs `kubectl rollout status` for every workload in the
// applied manifests, then prints each workload's images. All workloads are
// waited on even if one fails, so a single bad rollout doesn't hide the rest.
func waitForRollouts(path string, res Resolution, namespace string, timeout time.Duration, opts runner.Options, out io.Writer) error {
	getOpts := opts
	getOpts.CaptureStdout = true
	getOpts.Silent = true
	got, err := runner.RunWithOpts("kubectl", getWorkloadsArgs(path, res, namespace), getOpts)
	if err != nil {
		return fmt.Errorf("list applied workloads: %w", err)
	}
	workloads, err := parseWorkloads([]byte(got.Stdout))
	if err != nil {
		return err
	}
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(out, format, a...) }

	if len(workloads) == 0 {
		p("info: no Deployments/StatefulSets/DaemonSets to wait on\n")
		return nil
	}

	var failed []string
	ok := map[string]bool{}
	for _, w := range workloads {
		if _, err := runner.RunWithOpts("kubectl", rolloutStatusArgs(w, res, timeout), opts); err != nil {
			failed = append(failed, w.Namespace+"/"+w.ref())
			continue
		}
		ok[w.Namespace+"/"+w.ref()] = true
	}

	p("\nrollout summary:\n")
	for _, w := range workloads {
		status := "FAILED"
		if ok[w.Namespace+"/"+w.ref()] {
			status = "ready"
		}
		p("  %-6s %s/%s  %s\n", status, w.Namespace, w.ref(), strings.Join(w.Images, ", "))
	}

	if len(failed) > 0 {
		return fmt.Errorf("rollout not ready within %s: %s", timeout, strings.Join(failed, ", "))
	}
	return nil
}
