package runner

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/deepak-science/kestrel/internal/execlog"
)

// Options configures a RunWithOpts call.
type Options struct {
	// Dir is the working directory. Empty means inherit.
	Dir string

	// Env merges onto os.Environ() for the child process. Empty means inherit only.
	Env map[string]string

	// CaptureStdout makes Result.Stdout contain the command's stdout in
	// addition to streaming it to os.Stdout. When false, stdout streams
	// directly and Result.Stdout is empty.
	CaptureStdout bool

	// Silent suppresses streaming stdout to os.Stdout; use with CaptureStdout.
	Silent bool

	// Stdin overrides the command's stdin. Default os.Stdin.
	Stdin io.Reader

	// CaptureStderr also keeps stderr in Result.Stderr; it still streams to os.Stderr.
	CaptureStderr bool
}

// Result returns details from a RunWithOpts call.
type Result struct {
	// Stdout is the captured stdout. Empty unless Options.CaptureStdout was set.
	Stdout string

	// Stderr is the captured stderr. Empty unless Options.CaptureStderr was set.
	Stderr string

	// ExitCode is the process exit code (-1 if the process did not start).
	ExitCode int
}

// RunWithOpts is the unified entry point for shelling out. It streams stdout
// to os.Stdout (optionally also capturing) and stderr to os.Stderr, and
// records the execution in the exec log + slog.
func RunWithOpts(name string, args []string, opts Options) (*Result, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = opts.Dir
	var stderrBuf bytes.Buffer
	cmd.Stderr = os.Stderr
	if opts.CaptureStderr {
		cmd.Stderr = io.MultiWriter(os.Stderr, &stderrBuf)
	}

	if opts.Stdin != nil {
		cmd.Stdin = opts.Stdin
	} else {
		cmd.Stdin = os.Stdin
	}

	if len(opts.Env) > 0 {
		cmd.Env = os.Environ()
		for k, v := range opts.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}

	var stdoutBuf bytes.Buffer
	switch {
	case opts.CaptureStdout && opts.Silent:
		cmd.Stdout = &stdoutBuf
	case opts.CaptureStdout:
		cmd.Stdout = io.MultiWriter(os.Stdout, &stdoutBuf)
	default:
		cmd.Stdout = os.Stdout
	}

	slog.Debug("exec",
		"command", name,
		"args", args,
		"dir", opts.Dir,
		"env", opts.Env,
	)

	start := time.Now()
	err := cmd.Run()

	result := &Result{Stdout: stdoutBuf.String(), Stderr: stderrBuf.String(), ExitCode: -1}
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}

	logCommand(name, args, opts.Dir, start, result.ExitCode)
	return result, err
}

// Run executes a command, streaming stdout/stderr to the terminal.
func Run(name string, args ...string) error {
	_, err := RunWithOpts(name, args, Options{})
	return err
}

// RunInDir executes a command in a specific directory.
func RunInDir(dir, name string, args ...string) error {
	_, err := RunWithOpts(name, args, Options{Dir: dir})
	return err
}

// RunWithEnv executes a command with additional environment variables.
func RunWithEnv(env map[string]string, name string, args ...string) error {
	_, err := RunWithOpts(name, args, Options{Env: env})
	return err
}

// RunInDirWithEnv executes a command in a specific directory with additional env vars.
func RunInDirWithEnv(dir string, env map[string]string, name string, args ...string) error {
	_, err := RunWithOpts(name, args, Options{Dir: dir, Env: env})
	return err
}

// Output executes a command and returns its stdout. Unlike RunWithOpts the
// child's stdout is NOT streamed to the parent — only captured. Use this
// for silent helper calls (git, version probes) where streaming would be noise.
func Output(name string, args ...string) (string, error) {
	slog.Debug("exec.output", "command", name, "args", args)
	cmd := exec.Command(name, args...)
	start := time.Now()
	out, err := cmd.Output()
	exitCode := exitCodeFromErr(err)
	logCommand(name, args, "", start, exitCode)
	return strings.TrimSpace(string(out)), err
}

func logCommand(name string, args []string, dir string, start time.Time, exitCode int) {
	execlog.Log(execlog.Entry{
		Timestamp:  start,
		Command:    name,
		Args:       args,
		Dir:        dir,
		ExitCode:   exitCode,
		DurationMs: time.Since(start).Milliseconds(),
	})
}

func exitCodeFromErr(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}
