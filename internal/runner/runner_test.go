package runner

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRunWithOpts_CaptureStdout(t *testing.T) {
	res, err := RunWithOpts("sh", []string{"-c", "echo hello"}, Options{
		CaptureStdout: true,
	})
	if err != nil {
		t.Fatalf("RunWithOpts: %v", err)
	}
	if !strings.Contains(res.Stdout, "hello") {
		t.Errorf("Stdout = %q, want to contain %q", res.Stdout, "hello")
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
}

// A background child inherits the captured stdout pipe and outlives the killed shell.
// Its stderr is detached so the orphan doesn't hold the test binary's own output open.
func TestRunWithOpts_ContextBoundsPipeDrain(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := RunWithOpts("sh", []string{"-c", "sleep 20 2>/dev/null & wait"}, Options{Context: ctx, CaptureStdout: true, Silent: true})
	if err == nil {
		t.Fatal("expected the cancelled command to return an error")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("cancelled command took %s to return", elapsed)
	}
}

func TestRunWithOpts_NoCapture(t *testing.T) {
	res, err := RunWithOpts("sh", []string{"-c", "echo hello"}, Options{})
	if err != nil {
		t.Fatalf("RunWithOpts: %v", err)
	}
	if res.Stdout != "" {
		t.Errorf("Stdout = %q, want empty when CaptureStdout is false", res.Stdout)
	}
}

func TestRunWithOpts_Env(t *testing.T) {
	res, err := RunWithOpts("sh", []string{"-c", `printf '%s' "$KEST_TEST_VAR"`}, Options{
		Env:           map[string]string{"KEST_TEST_VAR": "from-kest"},
		CaptureStdout: true,
	})
	if err != nil {
		t.Fatalf("RunWithOpts: %v", err)
	}
	if res.Stdout != "from-kest" {
		t.Errorf("Stdout = %q, want %q (env var not propagated)", res.Stdout, "from-kest")
	}
}

func TestRunWithOpts_ExitCode(t *testing.T) {
	res, err := RunWithOpts("sh", []string{"-c", "exit 7"}, Options{})
	if err == nil {
		t.Fatal("RunWithOpts: expected error for nonzero exit")
	}
	if res.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", res.ExitCode)
	}
}

func TestRunWithOpts_Dir(t *testing.T) {
	dir := t.TempDir()
	res, err := RunWithOpts("pwd", nil, Options{
		Dir:           dir,
		CaptureStdout: true,
	})
	if err != nil {
		t.Fatalf("RunWithOpts: %v", err)
	}
	if !strings.Contains(res.Stdout, dir) {
		t.Errorf("Stdout = %q, want to contain dir %q", res.Stdout, dir)
	}
}

func TestOutput_Captures(t *testing.T) {
	out, err := Output("sh", "-c", "echo silently")
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if out != "silently" {
		t.Errorf("out = %q, want %q", out, "silently")
	}
}

func TestOutput_TrimsWhitespace(t *testing.T) {
	out, err := Output("sh", "-c", "echo '  padded  '")
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if out != "padded" {
		t.Errorf("out = %q, want %q (should trim)", out, "padded")
	}
}
