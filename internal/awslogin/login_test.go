package awslogin

import (
	"os"
	"strings"
	"testing"
)

func stub(t *testing.T, valid, interactive bool) {
	t.Helper()
	origValid, origInteractive := sessionValid, isInteractive
	sessionValid = func(string) bool { return valid }
	isInteractive = func() bool { return interactive }
	t.Cleanup(func() { sessionValid, isInteractive = origValid, origInteractive })
}

func TestEnsureSession_EmptyProfile(t *testing.T) {
	stub(t, false, false)
	if err := EnsureSession(""); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestEnsureSession_ValidSession(t *testing.T) {
	stub(t, true, false)
	if err := EnsureSession("my-profile"); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestEnsureSession_ExpiredNoTTY(t *testing.T) {
	stub(t, false, false)
	err := EnsureSession("my-profile")
	if err == nil {
		t.Fatal("expected error for expired session without tty")
	}
	if !strings.Contains(err.Error(), "aws sso login --profile my-profile") {
		t.Errorf("error should include the login command, got %q", err)
	}
}

// /dev/null is a character device, so a ModeCharDevice check mistakes it for a terminal.
func TestStdinIsTerminal_DevNull(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = devNull.Close() }()
	orig := os.Stdin
	os.Stdin = devNull
	defer func() { os.Stdin = orig }()

	if stdinIsTerminal() {
		t.Error("stdin from /dev/null reported as a terminal")
	}
}
