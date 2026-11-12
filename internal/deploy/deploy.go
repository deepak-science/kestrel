// Package deploy is the cluster-agnostic app-deploy spine shared by
// `kest deploy` and `kestci deploy`: resolve a target, then execute via a
// pluggable executor (helm or kubectl) under an interactive or CI Policy.
package deploy

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/deepak-science/kestrel/internal/config"
	"github.com/deepak-science/kestrel/internal/runner"
)

// Deploy actions.
const (
	ActionApply = "apply" // helm upgrade --install / kubectl apply
	ActionDiff  = "diff"  // helm upgrade --dry-run / kubectl diff (read-only)
)

// Resolution is the resolved cluster access for a deploy target.
type Resolution struct {
	// KubeContext is the context to act in. Empty uses the current-context.
	KubeContext string
	// Kubeconfig is an explicit kubeconfig path (resolved against the project
	// root). Empty uses the ambient kubeconfig.
	Kubeconfig string
	// AwsProfile is injected as AWS_PROFILE under the non-ambient policy. Empty
	// for non-AWS clusters.
	AwsProfile string
	// AccountID is the expected AWS account, echoed for legibility.
	AccountID string
}

// Policy parameterizes execution. The zero value is the `kest deploy` posture
// (inject the resolved AWS profile); Ambient is the `kestci` posture
// (credentials and kubeconfig from the environment).
type Policy struct {
	// Ambient: don't set AWS_PROFILE; credentials come from the environment.
	Ambient bool
	// PrintContext: print the app/target/aws block before running.
	PrintContext bool
	// Out is where the context block is written. Defaults to os.Stderr.
	Out io.Writer
	// Wait, for manifest applies, runs rollout status on each workload with this per-workload timeout.
	// Zero skips it. Helm applies already wait via --atomic.
	Wait time.Duration
	// ForceConflicts takes ownership of fields another field manager holds (server_side deploys only).
	ForceConflicts bool
	// Stdout receives the server-side apply change summary. Defaults to os.Stdout.
	Stdout io.Writer
}

func (p Policy) out() io.Writer {
	if p.Out != nil {
		return p.Out
	}
	return os.Stderr
}

func (p Policy) stdout() io.Writer {
	if p.Stdout != nil {
		return p.Stdout
	}
	return os.Stdout
}

// ExecResult captures the outcome of a deploy execution.
type ExecResult struct {
	ExitCode int
}

func Resolve(cfg *config.Config, targetName string) (Resolution, error) {
	if cfg == nil {
		return Resolution{}, fmt.Errorf("no config loaded")
	}
	t, ok := cfg.Targets[targetName]
	if !ok {
		return Resolution{}, fmt.Errorf("target %q not configured (check your .kestconfig targets)", targetName)
	}

	var res Resolution
	if t.Cluster != "" {
		ctx := cfg.ResolveClusterContext(t.Cluster)
		if ctx == "" {
			// Unmapped cluster name is used as the context name directly.
			ctx = t.Cluster
		}
		res.KubeContext = ctx
	}

	if t.Kubeconfig != "" {
		res.Kubeconfig = projectPath(cfg, t.Kubeconfig)
	}

	// AWS profile: explicit account wins, else extract from an EKS ARN context.
	switch {
	case t.AWSAccount != "":
		res.AccountID = t.AWSAccount
		res.AwsProfile = cfg.ResolveAccountProfile(t.AWSAccount)
	case res.KubeContext != "":
		if acct := config.ExtractAccountIDFromARN(res.KubeContext); acct != "" {
			res.AccountID = acct
			res.AwsProfile = cfg.ResolveAccountProfile(acct)
		}
	}

	return res, nil
}

// Execute runs one deploy action, picking the executor from d.Kind() and
// applying credentials per policy. It runs from the project root so
// chart/manifest/values paths resolve relative to it.
func Execute(cfg *config.Config, name string, d config.Deploy, res Resolution, action string, pol Policy, extra []string) (*ExecResult, error) {
	if err := d.Validate(name); err != nil {
		return nil, err
	}

	if pol.PrintContext {
		printContext(pol.out(), name, d, res, pol)
	}

	env := deployEnv(res, pol)

	var command string
	var args []string
	switch d.Kind() {
	case config.DeployHelm:
		command = "helm"
		release := d.ReleaseName
		if release == "" {
			release = name
		}
		args = helmArgs(release, d, res, action, extra)
	case config.DeployManifest:
		command = "kubectl"
		args = kubectlArgs(d.Manifests, res, d.Namespace, kubectlOpts{
			Diff:           action == ActionDiff,
			ServerSide:     d.UsesServerSide(),
			ForceConflicts: pol.ForceConflicts,
		}, extra)
	}

	opts := runner.Options{Dir: projectRoot(cfg), Env: env, CaptureStderr: d.UsesServerSide()}
	var result *runner.Result
	var err error
	if action == ActionApply && d.UsesServerSide() {
		ko := kubectlOpts{ServerSide: true, ForceConflicts: pol.ForceConflicts}
		result, err = serverSideApply(d.Manifests, res, d.Namespace, ko, extra, opts, pol.stdout(), pol.out())
	} else {
		result, err = runner.RunWithOpts(command, args, opts)
	}
	out := &ExecResult{ExitCode: result.ExitCode}

	// kubectl diff exits 1 when differences exist; higher codes are real errors.
	if d.Kind() == config.DeployManifest && action == ActionDiff && result.ExitCode == 1 {
		return out, nil
	}
	if err != nil {
		if strings.Contains(result.Stderr, csaConflict) {
			err = fmt.Errorf("%w\n%s", err, csaConflictHint)
		}
		return out, err
	}

	if d.Kind() == config.DeployManifest && action == ActionApply && pol.Wait > 0 {
		return out, waitForRollouts(d.Manifests, res, d.Namespace, pol.Wait, opts, pol.out())
	}
	return out, nil
}

// kubectl doesn't hand client-side ownership of an atomic field (e.g. a StatefulSet's volumeClaimTemplates) to the server-side manager when the live value has defaults filled in.
const (
	csaConflict     = `conflict with "kubectl-client-side-apply"`
	csaConflictHint = "the conflicting fields were last set by client-side kubectl apply (an earlier kest deploy, or kubectl apply by hand).\n" +
		"Apply once with --force-conflicts to hand them to kest (--diff --force-conflicts previews it), or set server_side: false."
)

func deployEnv(res Resolution, pol Policy) map[string]string {
	if pol.Ambient || res.AwsProfile == "" {
		return nil
	}
	return map[string]string{"AWS_PROFILE": res.AwsProfile}
}

// projectRoot returns the directory containing the loaded .kestconfig, or the
// current working directory if no project config was found.
func projectRoot(cfg *config.Config) string {
	if cfg != nil && cfg.Sources.Project != "" {
		return filepath.Dir(cfg.Sources.Project)
	}
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return wd
}

// projectPath resolves a config-relative path against the project root. Absolute
// paths are returned unchanged.
func projectPath(cfg *config.Config, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(projectRoot(cfg), p)
}

func printContext(out io.Writer, name string, d config.Deploy, res Resolution, pol Policy) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(out, format, a...) }

	p("app:     %s (%s)\n", name, d.Kind())
	p("target:  %s\n", d.Target)
	if res.KubeContext != "" {
		p("context: %s\n", res.KubeContext)
	}
	if res.Kubeconfig != "" {
		p("config:  %s\n", res.Kubeconfig)
	}
	switch {
	case pol.Ambient && res.AccountID != "":
		p("aws:     ambient (expect account %s)\n", res.AccountID)
	case pol.Ambient:
		p("aws:     ambient\n")
	case res.AwsProfile != "":
		p("aws:     %s\n", res.AwsProfile)
	}
	p("\n")
}

// sortedSetArgs flattens a --set map into deterministic key=value strings.
func sortedSetArgs(set map[string]string) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+set[k])
	}
	return out
}
