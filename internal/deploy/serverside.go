package deploy

import (
	"fmt"
	"io"
	"strings"

	"github.com/deepak-science/kestrel/internal/runner"
)

// ref renders an object the way kubectl apply does, e.g. deployment.apps/web.
func (o kubeObject) ref() string {
	kind := strings.ToLower(o.Kind)
	if group, _, ok := strings.Cut(o.APIVersion, "/"); ok {
		kind += "." + group
	}
	return kind + "/" + o.Metadata.Name
}

func (o kubeObject) key() string {
	return o.APIVersion + "|" + o.Kind + "|" + o.Metadata.Namespace + "|" + o.Metadata.Name
}

type applyChange struct {
	Ref    string
	Status string // created, changed, unchanged
}

// classifyApply compares objects before and after a server-side apply.
// kubectl reports every object as "serverside-applied"; a no-op apply leaves resourceVersion untouched, which is what tells changed from unchanged.
func classifyApply(before, after []kubeObject) []applyChange {
	prior := map[string]string{}
	for _, o := range before {
		prior[o.key()] = o.Metadata.ResourceVersion
	}
	out := make([]applyChange, 0, len(after))
	for _, o := range after {
		status := "changed"
		switch rv, ok := prior[o.key()]; {
		case !ok:
			status = "created"
		case rv == o.Metadata.ResourceVersion:
			status = "unchanged"
		}
		out = append(out, applyChange{Ref: o.ref(), Status: status})
	}
	return out
}

func printApplyChanges(out io.Writer, changes []applyChange) {
	counts := map[string]int{}
	for _, c := range changes {
		_, _ = fmt.Fprintf(out, "%s %s\n", c.Ref, c.Status)
		counts[c.Status]++
	}
	_, _ = fmt.Fprintf(out, "\n%d created, %d changed, %d unchanged\n", counts["created"], counts["changed"], counts["unchanged"])
}

// serverSideApply runs a server-side apply and prints a created/changed/unchanged line per object.
// If the pre-apply snapshot fails (e.g. a CRD the manifests install doesn't exist yet), it falls back to kubectl's own output.
func serverSideApply(path string, res Resolution, namespace string, ko kubectlOpts, extra []string, opts runner.Options, stdout, stderr io.Writer) (*runner.Result, error) {
	snapOpts := opts
	snapOpts.CaptureStdout = true
	snapOpts.Silent = true

	getArgs := append(getWorkloadsArgs(path, res, namespace), "--ignore-not-found")
	var before []kubeObject
	got, err := runner.RunWithOpts("kubectl", getArgs, snapOpts)
	if err == nil {
		before, err = parseObjects([]byte(got.Stdout))
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: couldn't snapshot objects before apply (%v); change summary unavailable\n", err)
		return runner.RunWithOpts("kubectl", kubectlArgs(path, res, namespace, ko, extra), opts)
	}

	ko.JSON = true
	result, err := runner.RunWithOpts("kubectl", kubectlArgs(path, res, namespace, ko, extra), snapOpts)
	after, perr := parseObjects([]byte(result.Stdout))
	if perr == nil && (err == nil || len(after) > 0) {
		printApplyChanges(stdout, classifyApply(before, after))
	}
	if err != nil {
		return result, err
	}
	if perr != nil {
		_, _ = fmt.Fprintf(stderr, "warning: applied, but couldn't read kubectl output for the change summary: %v\n", perr)
	}
	return result, nil
}
