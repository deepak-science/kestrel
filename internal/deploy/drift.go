package deploy

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/deepak-science/kestrel/internal/config"
	"github.com/deepak-science/kestrel/internal/runner"
)

// DriftReport is the read-only drift check for one deploy.
type DriftReport struct {
	Drift bool
	// Details lists what drifted, one line each.
	Details []string
	// Note qualifies a clean result, e.g. what the check couldn't compare.
	Note string
}

// Drift checks a deploy against the live cluster without changing anything.
// Manifest deploys run `kubectl diff` and look for fields set by live kubectl edits, which neither diff nor a server-side apply reverts.
// Helm deploys check release status and chart version, plus a values diff when the helm-diff plugin is installed.
func Drift(cfg *config.Config, name string, d config.Deploy, res Resolution, pol Policy) (DriftReport, error) {
	if err := d.Validate(name); err != nil {
		return DriftReport{}, err
	}
	opts := runner.Options{
		Dir:           projectRoot(cfg),
		Env:           deployEnv(res, pol),
		CaptureStdout: true,
		Silent:        true,
		SilentStderr:  true,
		Stdin:         strings.NewReader(""),
	}
	if d.Kind() == config.DeployHelm {
		release := d.ReleaseName
		if release == "" {
			release = name
		}
		return helmDrift(release, d, res, opts)
	}
	return manifestDrift(d, res, opts)
}

func manifestDrift(d config.Deploy, res Resolution, opts runner.Options) (DriftReport, error) {
	var rep DriftReport
	// Without --force-conflicts, a live edit to a field git sets fails a server-side diff with an ownership conflict instead of showing as a difference.
	diffArgs := kubectlArgs(d.Manifests, res, d.Namespace, kubectlOpts{Diff: true, ServerSide: d.UsesServerSide(), ForceConflicts: d.UsesServerSide()}, nil)
	r, err := runner.RunWithOpts("kubectl", diffArgs, opts)
	switch {
	// kubectl diff exits 1 when differences exist; higher codes are real errors.
	case r.ExitCode == 1:
		rep.Drift = true
		rep.Details = append(rep.Details, "differs from git: "+capList(diffedObjects(r.Stdout), 3))
	case err != nil:
		return rep, fmt.Errorf("kubectl diff: %s", runner.ErrorExcerpt(r.Stderr, err))
	}

	getArgs := append(getWorkloadsArgs(d.Manifests, res, d.Namespace), "--show-managed-fields", "--ignore-not-found")
	g, err := runner.RunWithOpts("kubectl", getArgs, opts)
	if err != nil {
		return rep, fmt.Errorf("kubectl get: %s", runner.ErrorExcerpt(g.Stderr, err))
	}
	// A server-side dry-run as kest shows which fields git sets, whatever the deploy's apply mode.
	// --force-conflicts keeps a conflicting live edit from failing it; kubectl diff already reports those.
	dryArgs := append(kubectlArgs(d.Manifests, res, d.Namespace, kubectlOpts{ServerSide: true, ForceConflicts: true, JSON: true}, nil), "--dry-run=server", "--show-managed-fields")
	dry, err := runner.RunWithOpts("kubectl", dryArgs, opts)
	if err != nil {
		return rep, fmt.Errorf("kubectl apply --dry-run=server: %s", runner.ErrorExcerpt(dry.Stderr, err))
	}
	edits, err := liveEdits([]byte(g.Stdout), []byte(dry.Stdout))
	if err != nil {
		return rep, err
	}
	if len(edits) > 0 {
		rep.Drift = true
		rep.Details = append(rep.Details, edits...)
	}
	return rep, nil
}

// diffedObjects names the objects in `kubectl diff` output from its per-object header, e.g. apps.v1.Deployment.media.web.
func diffedObjects(out string) []string {
	var objs []string
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "diff ") {
			continue
		}
		fields := strings.Fields(line)
		objs = append(objs, filepath.Base(fields[len(fields)-1]))
	}
	return objs
}

func capList(items []string, n int) string {
	if len(items) == 0 {
		return "(no object names in diff output)"
	}
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return fmt.Sprintf("%s, +%d more", strings.Join(items[:n], ", "), len(items)-n)
}

type managedObject struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name          string `json:"name"`
		Namespace     string `json:"namespace"`
		ManagedFields []struct {
			Manager  string         `json:"manager"`
			FieldsV1 map[string]any `json:"fieldsV1"`
		} `json:"managedFields"`
	} `json:"metadata"`
	Items []managedObject `json:"items"`
}

// liveEdit reports whether a field manager is a hand-run kubectl command (edit, patch, scale, set, label, ...).
// kest's own managers are not, nor is client-side apply, which is kest's manager when server_side is off.
// rollout restart only stamps restartedAt, which is expected operational churn.
func liveEdit(manager string) bool {
	switch manager {
	case "kubectl-client-side-apply", "kubectl-rollout":
		return false
	}
	return manager == "kubectl" || strings.HasPrefix(manager, "kubectl-")
}

// liveEdits lists, per object and manager, the fields owned by live kubectl edits that git doesn't set.
// live is `kubectl get --show-managed-fields`; dryRun is a server-side dry-run apply as kest, whose kest entry is exactly the fields git sets.
// A field git sets can be co-owned by an old patch with the same value; kubectl diff covers a differing one.
func liveEdits(live, dryRun []byte) ([]string, error) {
	liveObjs, err := parseManaged(live)
	if err != nil {
		return nil, err
	}
	dryObjs, err := parseManaged(dryRun)
	if err != nil {
		return nil, err
	}
	gitPaths := map[string][]fieldPath{}
	for _, o := range dryObjs {
		for _, mf := range o.Metadata.ManagedFields {
			if mf.Manager == FieldManager {
				gitPaths[o.key()] = append(gitPaths[o.key()], fieldPaths(mf.FieldsV1, nil)...)
			}
		}
	}

	var out []string
	for _, o := range liveObjs {
		ref := kubeObject{APIVersion: o.APIVersion, Kind: o.Kind}
		ref.Metadata.Name = o.Metadata.Name
		for _, mf := range o.Metadata.ManagedFields {
			if !liveEdit(mf.Manager) {
				continue
			}
			var paths []string
			for _, p := range fieldPaths(mf.FieldsV1, nil) {
				if !coveredBy(p, gitPaths[o.key()]) {
					paths = append(paths, p.String())
				}
			}
			if len(paths) == 0 {
				continue
			}
			out = append(out, fmt.Sprintf("%s on %s: %s", mf.Manager, ref.ref(), capList(paths, 3)))
		}
	}
	return out, nil
}

// key identifies an object by API group and kind, not version: two CRDs can share a kind.
func (o managedObject) key() string {
	group := ""
	if g, _, ok := strings.Cut(o.APIVersion, "/"); ok {
		group = g
	}
	return group + "|" + o.Kind + "|" + o.Metadata.Namespace + "|" + o.Metadata.Name
}

// fieldPath is a managedFields path as raw fieldsV1 keys ("f:labels", "f:app.kubernetes.io/name", `k:{"name":"web"}`).
// Paths are compared by segment: a field name can contain dots, so the flattened form is for display only.
type fieldPath []string

// String flattens the path for display, e.g. spec.template.spec.containers[name=web].image.
func (p fieldPath) String() string {
	var b strings.Builder
	for _, k := range p {
		seg := fieldSegment(k)
		if b.Len() > 0 && !strings.HasPrefix(seg, "[") {
			b.WriteByte('.')
		}
		b.WriteString(seg)
	}
	return b.String()
}

// coveredBy reports whether git sets path itself or an ancestor of it (an atomic map or list owned whole).
func coveredBy(path fieldPath, gitPaths []fieldPath) bool {
	for _, g := range gitPaths {
		if len(g) <= len(path) && slices.Equal(g, path[:len(g)]) {
			return true
		}
	}
	return false
}

func parseManaged(data []byte) ([]managedObject, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, nil
	}
	var top managedObject
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("parse kubectl output: %w", err)
	}
	if top.Kind == "List" {
		return top.Items, nil
	}
	return []managedObject{top}, nil
}

// fieldPaths returns the leaf paths of a managedFields fieldsV1 tree, in sorted order.
func fieldPaths(tree map[string]any, prefix fieldPath) []fieldPath {
	keys := make([]string, 0, len(tree))
	for k := range tree {
		if k != "." {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var out []fieldPath
	for _, k := range keys {
		p := append(slices.Clone(prefix), k)
		child, _ := tree[k].(map[string]any)
		if hasFieldKeys(child) {
			out = append(out, fieldPaths(child, p)...)
		} else {
			out = append(out, p)
		}
	}
	return out
}

func hasFieldKeys(m map[string]any) bool {
	for k := range m {
		if k != "." {
			return true
		}
	}
	return false
}

// fieldSegment renders one fieldsV1 key: f:name is a field, k:{...} a keyed list item, v:... a set value, i:N an index.
func fieldSegment(k string) string {
	prefix, rest, _ := strings.Cut(k, ":")
	switch prefix {
	case "f":
		return rest
	case "k":
		var kv map[string]any
		if json.Unmarshal([]byte(rest), &kv) == nil {
			parts := make([]string, 0, len(kv))
			for key, v := range kv {
				parts = append(parts, fmt.Sprintf("%s=%v", key, v))
			}
			sort.Strings(parts)
			return "[" + strings.Join(parts, ",") + "]"
		}
	}
	return "[" + rest + "]"
}

// Helm release state, from `helm status -o json`.
type helmStatus struct {
	Info struct {
		Status string `json:"status"`
	} `json:"info"`
	Chart struct {
		Metadata struct {
			Version string `json:"version"`
		} `json:"metadata"`
	} `json:"chart"`
}

func helmDrift(release string, d config.Deploy, res Resolution, opts runner.Options) (DriftReport, error) {
	var rep DriftReport
	statusArgs := append([]string{"status", release, "-o", "json"}, helmClusterFlags(d, res)...)
	r, err := runner.RunWithOpts("helm", statusArgs, opts)
	if err != nil {
		if strings.Contains(r.Stderr, "release: not found") {
			rep.Drift = true
			rep.Details = []string{"release " + release + " not installed"}
			return rep, nil
		}
		return rep, fmt.Errorf("helm status: %s", runner.ErrorExcerpt(r.Stderr, err))
	}
	var st helmStatus
	if err := json.Unmarshal([]byte(r.Stdout), &st); err != nil {
		return rep, fmt.Errorf("parse helm status: %w", err)
	}
	if st.Info.Status != "deployed" {
		rep.Drift = true
		rep.Details = append(rep.Details, "release status "+st.Info.Status)
	}
	if d.Version != "" && st.Chart.Metadata.Version != d.Version {
		rep.Drift = true
		rep.Details = append(rep.Details, fmt.Sprintf("chart %s deployed, %s configured", st.Chart.Metadata.Version, d.Version))
	}

	if !helmDiffAvailable() {
		rep.Note = "values not compared (install the helm-diff plugin)"
		return rep, nil
	}
	diffArgs := append([]string{"diff", "upgrade", release, d.Chart}, helmChartFlags(d, res, false)...)
	// --three-way-merge compares against live objects; the default only compares the stored release manifest, which misses edits made outside helm.
	diffArgs = append(diffArgs, "--detailed-exitcode", "--suppress-secrets", "--three-way-merge")
	r, err = runner.RunWithOpts("helm", diffArgs, opts)
	switch {
	case r.ExitCode == 2:
		rep.Drift = true
		rep.Details = append(rep.Details, "differs from config: "+capList(helmDiffObjects(r.Stdout), 3))
	case err != nil:
		return rep, fmt.Errorf("helm diff: %s", runner.ErrorExcerpt(r.Stderr, err))
	}
	return rep, nil
}

func helmClusterFlags(d config.Deploy, res Resolution) []string {
	var args []string
	if d.Namespace != "" {
		args = append(args, "--namespace", d.Namespace)
	}
	if res.KubeContext != "" {
		args = append(args, "--kube-context", res.KubeContext)
	}
	if res.Kubeconfig != "" {
		args = append(args, "--kubeconfig", res.Kubeconfig)
	}
	return args
}

// helm-diff prints one header per object, e.g. "media, web, Deployment (apps) has changed:".
var helmDiffHeaderRE = regexp.MustCompile(`^\S+, (\S+), (\S+)(?: \([^)]*\))? has (changed|been added|been removed):`)

func helmDiffObjects(out string) []string {
	var objs []string
	for _, line := range strings.Split(out, "\n") {
		if m := helmDiffHeaderRE.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			verb := strings.TrimPrefix(m[3], "been ")
			objs = append(objs, fmt.Sprintf("%s/%s %s", m[2], m[1], verb))
		}
	}
	return objs
}

var (
	helmDiffOnce sync.Once
	helmDiffOK   bool
)

// helmDiffAvailable reports whether the helm-diff plugin is installed; checked once per run.
var helmDiffAvailable = func() bool {
	helmDiffOnce.Do(func() {
		_, err := runner.RunWithOpts("helm", []string{"diff", "version"}, runner.Options{CaptureStdout: true, Silent: true, SilentStderr: true})
		helmDiffOK = err == nil
	})
	return helmDiffOK
}
