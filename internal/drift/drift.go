// Package drift is the read-only sweep behind `kest drift` and `kestci drift`: plan every terraform root and diff every deploy, then report one table.
package drift

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"text/tabwriter"

	"github.com/deepak-science/kestrel/internal/config"
	"github.com/deepak-science/kestrel/internal/deploy"
	"github.com/deepak-science/kestrel/internal/swoop"
)

// Statuses.
const (
	Clean = "clean"
	Drift = "drift"
	Error = "error"
)

// Item is one checked root or deploy.
type Item struct {
	Kind    string   `json:"kind"` // "root" or "deploy"
	Name    string   `json:"name"`
	Status  string   `json:"status"`
	Summary string   `json:"summary,omitempty"`
	Details []string `json:"details,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// Sweep checks roots and deploys. The zero value is the `kest drift` posture; Ambient is kestci's.
type Sweep struct {
	Cfg         *config.Config
	BaseDir     string
	Environment string
	Ambient     bool
	// Init runs `init` on every root first, one at a time.
	Init bool
	// Parallel is how many checks run at once. Values below 1 mean 1.
	Parallel int
	// Progress gets a line per finished check. Defaults to os.Stderr.
	Progress io.Writer
}

type job struct {
	root   *swoop.Root
	deploy string
}

// Run checks every root and deploy and returns the results in input order: roots, then deploys.
func (s Sweep) Run(roots []swoop.Root, deploys []string) []Item {
	progress := s.Progress
	if progress == nil {
		progress = os.Stderr
	}
	items := make([]Item, len(roots)+len(deploys))
	jobs := make([]job, 0, len(items))
	for i := range roots {
		jobs = append(jobs, job{root: &roots[i]})
	}
	for _, d := range deploys {
		jobs = append(jobs, job{deploy: d})
	}

	failedInit := map[int]error{}
	if s.Init {
		for i := range roots {
			_, _ = fmt.Fprintf(progress, "init %s\n", roots[i].Path)
			if err := swoop.DriftInit(s.Cfg, roots[i], s.rootResolution(roots[i]), s.swoopPolicy()); err != nil {
				failedInit[i] = err
				continue
			}
			roots[i].Initialized = true
		}
	}

	plans := make([]*swoop.DriftResult, len(jobs))
	var mu sync.Mutex
	done := 0
	sem := make(chan struct{}, max(s.Parallel, 1))
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			var it Item
			if err, ok := failedInit[i]; ok {
				it = Item{Kind: "root", Name: j.root.Path, Status: Error, Error: err.Error()}
			} else if j.root != nil {
				it, plans[i] = s.checkRoot(*j.root)
			} else {
				it = s.checkDeploy(j.deploy)
			}
			mu.Lock()
			items[i] = it
			done++
			_, _ = fmt.Fprintf(progress, "[%d/%d] %s %s: %s\n", done, len(jobs), it.Kind, it.Name, it.Status)
			mu.Unlock()
		}()
	}
	wg.Wait()

	// After the workers: state is one unlocked file per project.
	for i, p := range plans {
		if p != nil {
			swoop.RecordAction(s.BaseDir, jobs[i].root.Path, "plan", &swoop.ExecResult{ExitCode: p.ExitCode, PlanSummary: p.Summary, Changes: p.Changes})
		}
	}
	return items
}

func (s Sweep) swoopPolicy() swoop.Policy { return swoop.Policy{Ambient: s.Ambient} }

func (s Sweep) rootResolution(root swoop.Root) swoop.Resolution {
	return swoop.EffectiveProfiles(s.Cfg, root, s.Environment)
}

// checkRoot returns the plan result too, for recording when it completed.
func (s Sweep) checkRoot(root swoop.Root) (Item, *swoop.DriftResult) {
	it := Item{Kind: "root", Name: root.Path}
	res, err := swoop.DriftPlan(s.Cfg, root, s.rootResolution(root), s.swoopPolicy())
	switch {
	case err != nil:
		it.Status, it.Error = Error, err.Error()
		return it, nil
	case res.Changes:
		it.Status, it.Summary = Drift, res.Summary
	default:
		it.Status, it.Summary = Clean, res.Summary
	}
	return it, &res
}

func (s Sweep) checkDeploy(name string) Item {
	it := Item{Kind: "deploy", Name: name}
	d, ok := s.Cfg.Deploys[name]
	if !ok {
		it.Status, it.Error = Error, "not found in .kestconfig"
		return it
	}
	res, err := deploy.Resolve(s.Cfg, d.Target)
	if err != nil {
		it.Status, it.Error = Error, err.Error()
		return it
	}
	rep, err := deploy.Drift(s.Cfg, name, d, res, deploy.Policy{Ambient: s.Ambient})
	switch {
	case err != nil:
		it.Status, it.Error = Error, err.Error()
	case rep.Drift:
		it.Status, it.Details = Drift, rep.Details
	default:
		it.Status = Clean
	}
	return it
}

// Counts tallies items by status.
func Counts(items []Item) (clean, drifted, errored int) {
	for _, it := range items {
		switch it.Status {
		case Clean:
			clean++
		case Drift:
			drifted++
		case Error:
			errored++
		}
	}
	return
}

// Result maps a sweep to the exit contract: an error if any check failed, else swoop.ErrChanges if anything drifted.
func Result(items []Item) error {
	_, drifted, errored := Counts(items)
	switch {
	case errored > 0:
		return fmt.Errorf("%d of %d check(s) failed", errored, len(items))
	case drifted > 0:
		return swoop.ErrChanges
	}
	return nil
}

// PrintTable writes one row per item, with extra detail lines under it, then the tally.
func PrintTable(w io.Writer, items []Item) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "KIND\tNAME\tSTATUS\tDETAIL")
	for _, it := range items {
		lines := it.Details
		switch {
		case it.Error != "":
			lines = []string{it.Error}
		case len(lines) == 0:
			lines = []string{it.Summary}
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", it.Kind, it.Name, strings.ToUpper(it.Status), lines[0])
		for _, l := range lines[1:] {
			_, _ = fmt.Fprintf(tw, "\t\t\t%s\n", l)
		}
	}
	_ = tw.Flush()
	clean, drifted, errored := Counts(items)
	_, _ = fmt.Fprintf(w, "\n%d checked: %d clean, %d drifted, %d errored\n", len(items), clean, drifted, errored)
}

// PrintJSON writes the items as a JSON array.
func PrintJSON(w io.Writer, items []Item) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(items)
}

// Filter narrows a sweep: Only is "", "terraform" or "deploys"; Dir limits roots to a top-level dir; Target limits deploys to a target.
type Filter struct {
	Only   string
	Dir    string
	Target string
}

// Validate rejects an unknown Only.
func (f Filter) Validate() error {
	switch f.Only {
	case "", "terraform", "deploys":
		return nil
	}
	return fmt.Errorf("--only must be terraform or deploys, not %q", f.Only)
}

// WantRoots reports whether the caller needs to discover roots.
func (f Filter) WantRoots() bool { return f.Only != "deploys" }

// Select applies the filter to discovered roots and the configured deploys.
// A --dir or --target that matches nothing is an error: a sweep that quietly drops what was asked for reads as clean.
func Select(cfg *config.Config, roots []swoop.Root, f Filter) ([]swoop.Root, []string, error) {
	switch {
	case !f.WantRoots() && f.Dir != "":
		return nil, nil, errors.New("--dir selects terraform roots; it can't be combined with --only deploys")
	case !f.WantRoots():
		roots = nil
	case f.Dir != "":
		if roots = swoop.ResolveByDir(roots, f.Dir); len(roots) == 0 {
			return nil, nil, fmt.Errorf("--dir %q matched no terraform roots", f.Dir)
		}
	}
	var deploys []string
	switch {
	case f.Only == "terraform" && f.Target != "":
		return nil, nil, errors.New("--target selects deploys; it can't be combined with --only terraform")
	case f.Only != "terraform" && cfg != nil:
		deploys = cfg.DeployNames()
		if f.Target != "" {
			if deploys = cfg.DeploysForTarget(f.Target); len(deploys) == 0 {
				return nil, nil, fmt.Errorf("--target %q matched no deploys", f.Target)
			}
		}
	}
	if len(roots) == 0 && len(deploys) == 0 {
		return nil, nil, errors.New("nothing to check: no terraform roots or deploys matched")
	}
	return roots, deploys, nil
}

// Report prints items as a table (or JSON) to stdout and returns the exit contract from Result.
func Report(items []Item, asJSON bool) error {
	if asJSON {
		if err := PrintJSON(os.Stdout, items); err != nil {
			return err
		}
	} else {
		_, _ = fmt.Fprintln(os.Stderr)
		PrintTable(os.Stdout, items)
	}
	return Result(items)
}
