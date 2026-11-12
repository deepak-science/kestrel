package swoop

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/deepak-science/kestrel/internal/config"
)

// Selection narrows discovered roots, shared by `kest swoop` and `kestci`.
type Selection struct {
	// Target is an exact path, glob, substring or fuzzy match. Empty matches every root.
	Target string
	// Changed limits to roots with modified .tf files: "true" diffs against the merge-base, anything else is a git ref.
	Changed string
	// Dir limits to roots under one top-level directory.
	Dir string
}

// ErrNoMatches means the selection matched no roots. With --changed it just means nothing changed.
var ErrNoMatches = errors.New("no matching roots found")

// IsGlob reports whether target is a glob, i.e. a deliberate multi-root match.
func IsGlob(target string) bool {
	return strings.ContainsAny(target, "*?[")
}

// Select applies a Selection. Several matches are allowed only for a glob, --changed or --dir; a plain substring hitting several roots is ambiguous.
// The caller decides what a MatchFuzzy result needs (kest asks, kestci refuses).
func Select(roots []Root, baseDir string, sel Selection) ([]Root, MatchType, error) {
	matches := roots
	if sel.Dir != "" {
		matches = ResolveByDir(matches, sel.Dir)
	}
	if sel.Changed != "" {
		ref := ""
		if sel.Changed != "true" {
			ref = sel.Changed
		}
		changed, err := ChangedRoots(matches, baseDir, ref)
		if err != nil {
			return nil, MatchAll, fmt.Errorf("detecting changed roots: %w", err)
		}
		matches = changed
	}
	matchType := MatchAll
	if sel.Target != "" {
		matches, matchType = ResolveWithType(matches, sel.Target)
	}

	if len(matches) == 0 {
		return nil, matchType, ErrNoMatches
	}
	if len(matches) > 1 && !IsGlob(sel.Target) && sel.Changed == "" && sel.Dir == "" {
		return nil, matchType, AmbiguousError(matches, sel.Target)
	}
	return matches, matchType, nil
}

// AmbiguousError lists the roots a target matched.
func AmbiguousError(matches []Root, target string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "target %q matches %d roots — be more specific or use a glob pattern:\n", target, len(matches))
	for _, m := range matches {
		fmt.Fprintf(&b, "  %s\n", m.Path)
	}
	return fmt.Errorf("%s", b.String())
}

// Runner executes one action across selected roots: a single root directly, several as a batch with a summary.
type Runner struct {
	Cfg         *config.Config
	BaseDir     string
	Environment string
	// Policy is the exec posture. PrintContext applies to single-root runs; batches print a header per root instead, plus the context when BatchContext is set.
	Policy       Policy
	BatchContext bool
	// Preflight runs before each root's action, e.g. the version-pin check; an error skips that root.
	Preflight func(root *Root, batch bool) error
	// Out receives headers and the batch summary. Defaults to os.Stderr.
	Out io.Writer
}

func (r Runner) out() io.Writer {
	if r.Out != nil {
		return r.Out
	}
	return os.Stderr
}

// Run executes action on roots and returns an error if any root failed.
func (r Runner) Run(action string, roots []Root) error {
	if len(roots) == 1 {
		_, err := r.runOne(action, roots[0], false)
		return err
	}

	out := r.out()
	_, _ = fmt.Fprintf(out, "Running terraform %s on %d root(s)...\n\n", action, len(roots))
	results := make([]batchResult, len(roots))
	for i, root := range roots {
		_, _ = fmt.Fprintf(out, "━━━ [%d/%d] %s ━━━\n", i+1, len(roots), root.Path)
		summary, err := r.runOne(action, root, true)
		results[i] = batchResult{root: root, err: err, summary: summary}
		if err != nil {
			_, _ = fmt.Fprintf(out, "  FAILED: %v\n", err)
		}
		_, _ = fmt.Fprintln(out)
	}
	printBatchSummary(out, action, results)

	var failed int
	for _, res := range results {
		if res.err != nil {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d root(s) failed", failed, len(results))
	}
	return nil
}

func (r Runner) runOne(action string, root Root, batch bool) (string, error) {
	if r.Preflight != nil {
		if err := r.Preflight(&root, batch); err != nil {
			return "", err
		}
	}
	res := EffectiveProfiles(r.Cfg, root, r.Environment)
	pol := r.Policy
	if batch {
		pol.PrintContext = r.BatchContext
	}
	slog.Info("swoop action", "action", action, "root", root.Path, "dir", root.Dir, "aws_profile", res.Effective, "tf_version", root.TFVersion, "batch", batch)

	result, err := Execute(r.Cfg, root, r.BaseDir, action, res, pol)
	summary := ""
	if result != nil {
		summary = result.PlanSummary
	}
	if err != nil {
		slog.Warn("terraform action failed", "action", action, "root", root.Path, "err", err)
		return summary, fmt.Errorf("terraform %s failed: %w", action, err)
	}
	slog.Info("terraform action complete", "action", action, "root", root.Path, "exit_code", result.ExitCode, "plan_summary", summary)
	return summary, nil
}

type batchResult struct {
	root    Root
	err     error
	summary string
}

func printBatchSummary(out io.Writer, action string, results []batchResult) {
	_, _ = fmt.Fprintf(out, "━━━ Swoop %s summary ━━━\n", action)
	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	for _, r := range results {
		status := "OK"
		if r.err != nil {
			status = "FAILED"
		}
		if r.summary != "" {
			_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\n", r.root.Path, status, r.summary)
		} else {
			_, _ = fmt.Fprintf(w, "  %s\t%s\n", r.root.Path, status)
		}
	}
	_ = w.Flush()
}

// VersionMismatchWarning names the fix: repin when the active binary is newer than the pin, else install the pinned version.
func VersionMismatchWarning(binary string, root Root, check TFVersionCheck, indent string) string {
	pinFile := PinFileFor(root, "")
	var b strings.Builder
	if check.Installed == "" {
		fmt.Fprintf(&b, "%swarning: %s pins %s %s (%s), and %s version failed\n", indent, root.Path, binary, check.Required, pinFile, binary)
	} else {
		fmt.Fprintf(&b, "%swarning: %s pins %s %s (%s) but %s is active\n", indent, root.Path, binary, check.Required, pinFile, check.Installed)
	}
	switch {
	case check.Installed != "" && VersionNewer(check.Installed, check.Required):
		fmt.Fprintf(&b, "%s  to move the pin to %s: kest swoop pin %s\n", indent, check.Installed, root.Path)
	case !check.VersionManagerAvailable:
		fmt.Fprintf(&b, "%s  install %s %s, or set terraform.version_manager to one that's on PATH\n", indent, binary, check.Required)
	}
	return b.String()
}
