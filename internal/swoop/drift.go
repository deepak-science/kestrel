package swoop

import (
	"errors"
	"fmt"
	"strings"

	"github.com/deepak-science/kestrel/internal/config"
	"github.com/deepak-science/kestrel/internal/runner"
)

// DriftResult is the outcome of a read-only drift plan.
type DriftResult struct {
	Changes bool
	// Summary is the plan's summary line, e.g. "1 to add, 0 to change, 0 to destroy".
	Summary string
	// ExitCode is terraform's, kept so the plan can be recorded to state.
	ExitCode int
}

// DriftInit runs a quiet `init -input=false`.
// Callers run inits one at a time: terraform's plugin cache isn't safe for concurrent init.
func DriftInit(cfg *config.Config, root Root, res Resolution, pol Policy) error {
	_, err := quietTerraform(cfg, root, res, pol, "init", "-input=false", "-no-color")
	return err
}

// DriftPlan runs `plan -detailed-exitcode` without taking the state lock, so a sweep never blocks a real apply.
// Output is captured; on failure the error carries terraform's first "Error:" line.
func DriftPlan(cfg *config.Config, root Root, res Resolution, pol Policy) (DriftResult, error) {
	if !root.Initialized {
		return DriftResult{}, errors.New("not initialized (run kest swoop init, or drift --init)")
	}
	args := append([]string{"plan", "-detailed-exitcode", "-lock=false", "-input=false", "-no-color"}, pol.ExtraArgs...)
	r, err := quietTerraform(cfg, root, res, pol, args...)
	out := DriftResult{ExitCode: r.ExitCode, Summary: parsePlanSummary(r.Stdout)}
	if r.ExitCode == 2 {
		out.Changes = true
		return out, nil
	}
	return out, err
}

func quietTerraform(cfg *config.Config, root Root, res Resolution, pol Policy, args ...string) (*runner.Result, error) {
	command := "terraform"
	if cfg != nil {
		command = cfg.TerraformCommand()
	}
	profile := res.Effective
	if pol.Ambient {
		profile = ""
	}
	r, err := runner.RunWithOpts(command, args, runner.Options{
		Dir:           root.AbsPath,
		Env:           profileEnv(profile),
		CaptureStdout: true,
		Silent:        true,
		SilentStderr:  true,
		Stdin:         strings.NewReader(""),
	})
	if err != nil && r.ExitCode != 2 {
		err = fmt.Errorf("%s %s: %s", command, args[0], runner.ErrorExcerpt(r.Stderr, err))
	}
	return r, err
}
