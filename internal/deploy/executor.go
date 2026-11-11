package deploy

import "github.com/deepak-science/kestrel/internal/config"

// helmArgs builds the helm args for a deploy: `upgrade --install` for apply
// (with --atomic --cleanup-on-fail --timeout), or --dry-run for diff.
func helmArgs(release string, d config.Deploy, res Resolution, action string, extra []string) []string {
	args := []string{"upgrade", "--install", release, d.Chart}

	if d.Namespace != "" {
		args = append(args, "--namespace", d.Namespace, "--create-namespace")
	}
	if res.KubeContext != "" {
		args = append(args, "--kube-context", res.KubeContext)
	}
	if res.Kubeconfig != "" {
		args = append(args, "--kubeconfig", res.Kubeconfig)
	}
	if d.Repo != "" {
		args = append(args, "--repo", d.Repo)
	}
	if d.Version != "" {
		args = append(args, "--version", d.Version)
	}
	for _, v := range d.Values {
		args = append(args, "--values", v)
	}
	for _, kv := range sortedSetArgs(d.Set) {
		args = append(args, "--set", kv)
	}

	if action == ActionDiff {
		args = append(args, "--dry-run")
	} else {
		args = append(args, "--atomic", "--cleanup-on-fail", "--timeout", "5m0s")
	}

	return append(args, extra...)
}

// FieldManager is the server-side apply field manager kest applies as.
const FieldManager = "kest"

type kubectlOpts struct {
	Diff           bool
	ServerSide     bool
	ForceConflicts bool
	// JSON adds `-o json` to an apply, for the server-side change summary.
	JSON bool
}

// kubectlArgs builds the kubectl args for a manifest deploy: `apply -f` or
// `diff -f`.
func kubectlArgs(path string, res Resolution, namespace string, o kubectlOpts, extra []string) []string {
	verb := "apply"
	if o.Diff {
		verb = "diff"
	}
	args := append([]string{verb, "-f", path}, clusterArgs(res)...)
	if namespace != "" {
		args = append(args, "--namespace", namespace)
	}
	if o.ServerSide {
		args = append(args, "--server-side", "--field-manager="+FieldManager)
		if o.ForceConflicts {
			args = append(args, "--force-conflicts")
		}
	}
	if o.JSON && !o.Diff {
		args = append(args, "-o", "json")
	}

	return append(args, extra...)
}
