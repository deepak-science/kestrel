package deploy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/deepak-science/kestrel/internal/config"
	"github.com/deepak-science/kestrel/internal/runner"
)

const (
	snapshotAPIVersion = "snapshot.storage.k8s.io/v1"
	defaultClassAnno   = "snapshot.storage.kubernetes.io/is-default-class"
	// DeployLabel marks kest-created snapshots with the deploy they were taken for.
	DeployLabel = "kest/deploy"
)

type pvcInfo struct {
	Name, Namespace, StorageClass, Phase string
}

type snapClass struct {
	Name, Driver string
	Default      bool
}

type snapTarget struct {
	PVC, Namespace, Class, Snapshot string
}

// planSnapshots picks a VolumeSnapshotClass for every bound PVC: the configured class if its driver matches, else the driver's default, else the driver's only class.
// Any bound PVC left without a class is a problem, so a pre-upgrade snapshot never silently skips a volume.
func planSnapshots(pvcs []pvcInfo, provisioners map[string]string, classes []snapClass, want string) ([]snapTarget, []string) {
	byDriver := map[string][]snapClass{}
	var wanted *snapClass
	for i, c := range classes {
		byDriver[c.Driver] = append(byDriver[c.Driver], c)
		if c.Name == want {
			wanted = &classes[i]
		}
	}

	var plan []snapTarget
	var problems []string
	if want != "" && wanted == nil {
		return nil, []string{fmt.Sprintf("snapshot_class %q not found", want)}
	}
	for _, p := range pvcs {
		if p.Phase != "Bound" {
			continue
		}
		where := p.Namespace + "/" + p.Name
		driver, ok := provisioners[p.StorageClass]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: storage class %q not found", where, p.StorageClass))
			continue
		}
		class := ""
		switch cands := byDriver[driver]; {
		case wanted != nil && wanted.Driver == driver:
			class = wanted.Name
		case wanted != nil:
			problems = append(problems, fmt.Sprintf("%s: driver %s doesn't match snapshot_class %s (driver %s)", where, driver, wanted.Name, wanted.Driver))
			continue
		case len(cands) == 1:
			class = cands[0].Name
		default:
			for _, c := range cands {
				if c.Default {
					class = c.Name
				}
			}
		}
		if class == "" {
			problems = append(problems, fmt.Sprintf("%s: no VolumeSnapshotClass for driver %s", where, driver))
			continue
		}
		plan = append(plan, snapTarget{PVC: p.Name, Namespace: p.Namespace, Class: class})
	}
	return plan, problems
}

// skipPVCs drops PVCs matching a snapshot_skip glob; a pattern containing "/" matches namespace/name, otherwise just the name.
func skipPVCs(pvcs []pvcInfo, patterns []string) (kept, skipped []pvcInfo) {
	for _, p := range pvcs {
		if matchesAny(p, patterns) {
			skipped = append(skipped, p)
		} else {
			kept = append(kept, p)
		}
	}
	return kept, skipped
}

func matchesAny(p pvcInfo, patterns []string) bool {
	for _, pat := range patterns {
		subject := p.Name
		if strings.Contains(pat, "/") {
			subject = p.Namespace + "/" + p.Name
		}
		if ok, _ := path.Match(pat, subject); ok {
			return true
		}
	}
	return false
}

func snapshotName(pvc string, at time.Time) string {
	suffix := "-kest-" + at.UTC().Format("20060102-150405")
	if max := 253 - len(suffix); len(pvc) > max {
		pvc = pvc[:max]
	}
	return pvc + suffix
}

func snapshotManifest(app string, plan []snapTarget) ([]byte, error) {
	items := make([]map[string]any, 0, len(plan))
	for _, t := range plan {
		items = append(items, map[string]any{
			"apiVersion": snapshotAPIVersion,
			"kind":       "VolumeSnapshot",
			"metadata": map[string]any{
				"name":      t.Snapshot,
				"namespace": t.Namespace,
				"labels":    map[string]string{"app.kubernetes.io/managed-by": "kest", DeployLabel: app},
			},
			"spec": map[string]any{
				"volumeSnapshotClassName": t.Class,
				"source":                  map[string]string{"persistentVolumeClaimName": t.PVC},
			},
		})
	}
	return json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": items})
}

type kubeList struct {
	Kind  string     `json:"kind"`
	Items []kubeItem `json:"items"`
}

type kubeItem struct {
	Metadata struct {
		Name        string            `json:"name"`
		Namespace   string            `json:"namespace"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		StorageClassName string `json:"storageClassName"`
	} `json:"spec"`
	Status struct {
		Phase string `json:"phase"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"status"`
	Provisioner string `json:"provisioner"`
	Driver      string `json:"driver"`
}

// parseKubeList accepts a List or, as `kubectl get` returns for one named object, a bare object.
func parseKubeList(data []byte) (kubeList, error) {
	var l kubeList
	if err := json.Unmarshal(data, &l); err != nil {
		return l, err
	}
	if strings.HasSuffix(l.Kind, "List") {
		return l, nil
	}
	var it kubeItem
	if err := json.Unmarshal(data, &it); err != nil {
		return l, err
	}
	return kubeList{Kind: l.Kind, Items: []kubeItem{it}}, nil
}

func kubectlList(args []string, res Resolution, opts runner.Options) (kubeList, error) {
	opts.CaptureStdout = true
	opts.Silent = true
	var l kubeList
	got, err := runner.RunWithOpts("kubectl", append(append(args, "-o", "json"), clusterArgs(res)...), opts)
	if err != nil {
		return l, err
	}
	if l, err = parseKubeList([]byte(got.Stdout)); err != nil {
		return l, fmt.Errorf("parse kubectl %s: %w", strings.Join(args, " "), err)
	}
	return l, nil
}

// snapshotNamespaces is the deploy's namespace, or for manifests without one, the namespaces of its existing objects.
func snapshotNamespaces(d config.Deploy, res Resolution, opts runner.Options) ([]string, error) {
	if d.Namespace != "" {
		return []string{d.Namespace}, nil
	}
	if d.Kind() != config.DeployManifest {
		return nil, fmt.Errorf("set namespace: on the deploy to snapshot its volumes")
	}
	snapOpts := opts
	snapOpts.CaptureStdout = true
	snapOpts.Silent = true
	got, err := runner.RunWithOpts("kubectl", append(getWorkloadsArgs(d.Manifests, res, ""), "--ignore-not-found"), snapOpts)
	if err != nil {
		return nil, fmt.Errorf("list manifest objects: %w", err)
	}
	objs, err := parseObjects([]byte(got.Stdout))
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, o := range objs {
		if ns := o.Metadata.Namespace; ns != "" && !seen[ns] {
			seen[ns] = true
			out = append(out, ns)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Snapshot creates a CSI VolumeSnapshot of every bound PVC in the deploy's namespace(s) and waits for each to be readyToUse.
func Snapshot(cfg *config.Config, name string, d config.Deploy, res Resolution, pol Policy, timeout time.Duration) error {
	if err := d.Validate(name); err != nil {
		return err
	}
	out := pol.out()
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(out, format, a...) }
	opts := runner.Options{Dir: projectRoot(cfg), Env: deployEnv(res, pol)}
	if pol.PrintContext {
		printContext(out, name, d, res, pol)
	}

	namespaces, err := snapshotNamespaces(d, res, opts)
	if err != nil {
		return err
	}
	var pvcs []pvcInfo
	for _, ns := range namespaces {
		l, err := kubectlList([]string{"get", "pvc", "--namespace", ns}, res, opts)
		if err != nil {
			return fmt.Errorf("list PVCs in %s: %w", ns, err)
		}
		for _, it := range l.Items {
			pvcs = append(pvcs, pvcInfo{Name: it.Metadata.Name, Namespace: ns, StorageClass: it.Spec.StorageClassName, Phase: it.Status.Phase})
		}
	}
	pvcs, skipped := skipPVCs(pvcs, d.SnapshotSkip)
	for _, s := range skipped {
		p("snapshot: skip %s/%s (snapshot_skip)\n", s.Namespace, s.Name)
	}
	if len(pvcs) == 0 {
		p("snapshot: no PVCs to snapshot in %s\n", strings.Join(namespaces, ", "))
		return nil
	}

	scs, err := kubectlList([]string{"get", "storageclass"}, res, opts)
	if err != nil {
		return fmt.Errorf("list storage classes: %w", err)
	}
	provisioners := map[string]string{}
	for _, it := range scs.Items {
		provisioners[it.Metadata.Name] = it.Provisioner
	}
	vscs, err := kubectlList([]string{"get", "volumesnapshotclass"}, res, opts)
	if err != nil {
		return fmt.Errorf("list VolumeSnapshotClasses (are the CSI snapshot CRDs and controller installed?): %w", err)
	}
	var classes []snapClass
	for _, it := range vscs.Items {
		classes = append(classes, snapClass{Name: it.Metadata.Name, Driver: it.Driver, Default: it.Metadata.Annotations[defaultClassAnno] == "true"})
	}

	plan, problems := planSnapshots(pvcs, provisioners, classes, d.SnapshotClass)
	if len(problems) > 0 {
		return fmt.Errorf("can't snapshot every volume:\n  %s\nadd volumes that don't need one to the deploy's snapshot_skip", strings.Join(problems, "\n  "))
	}
	if len(plan) == 0 {
		p("snapshot: no bound PVCs in %s\n", strings.Join(namespaces, ", "))
		return nil
	}

	now := time.Now()
	for i := range plan {
		plan[i].Snapshot = snapshotName(plan[i].PVC, now)
		p("snapshot: %s/%s → %s (class %s)\n", plan[i].Namespace, plan[i].PVC, plan[i].Snapshot, plan[i].Class)
	}
	manifest, err := snapshotManifest(name, plan)
	if err != nil {
		return err
	}
	createOpts := opts
	createOpts.Stdin = bytes.NewReader(manifest)
	createOpts.CaptureStdout = true
	createOpts.Silent = true
	if _, err := runner.RunWithOpts("kubectl", append([]string{"create", "-f", "-"}, clusterArgs(res)...), createOpts); err != nil {
		return fmt.Errorf("create VolumeSnapshots: %w", err)
	}

	return waitForSnapshots(plan, res, opts, timeout, out)
}

func waitForSnapshots(plan []snapTarget, res Resolution, opts runner.Options, timeout time.Duration, out io.Writer) error {
	byNS := map[string][]string{}
	var order []string
	for _, t := range plan {
		if _, ok := byNS[t.Namespace]; !ok {
			order = append(order, t.Namespace)
		}
		byNS[t.Namespace] = append(byNS[t.Namespace], "volumesnapshot/"+t.Snapshot)
	}

	quiet := opts
	quiet.CaptureStdout = true
	quiet.Silent = true
	for _, ns := range order {
		args := append([]string{"wait", "--for=jsonpath={.status.readyToUse}=true", "--timeout", timeout.String(), "--namespace", ns}, byNS[ns]...)
		if _, err := runner.RunWithOpts("kubectl", append(args, clusterArgs(res)...), quiet); err != nil {
			return fmt.Errorf("snapshots in %s not ready within %s%s", ns, timeout, snapshotErrors(ns, byNS[ns], res, opts))
		}
	}
	_, _ = fmt.Fprintf(out, "snapshot: %d ready\n\n", len(plan))
	return nil
}

// snapshotErrors fetches status.error from snapshots that didn't become ready, since kubectl wait only reports the timeout.
func snapshotErrors(ns string, refs []string, res Resolution, opts runner.Options) string {
	l, err := kubectlList(append([]string{"get", "--namespace", ns}, refs...), res, opts)
	if err != nil {
		return ""
	}
	var msgs []string
	for _, it := range l.Items {
		if it.Status.Error != nil && it.Status.Error.Message != "" {
			msgs = append(msgs, it.Metadata.Name+": "+it.Status.Error.Message)
		}
	}
	if len(msgs) == 0 {
		return ""
	}
	return ":\n  " + strings.Join(msgs, "\n  ")
}
