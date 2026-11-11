package awslogin

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"

	"golang.org/x/term"
)

// Overridable in tests.
var (
	sessionValid  = stsSessionValid
	isInteractive = stdinIsTerminal
)

// EnsureSession checks whether the given AWS profile has a valid session
// (via sts get-caller-identity) and, if not, runs aws sso login.
// Without a TTY it returns an error instead, since the browser flow would block forever.
func EnsureSession(profile string) error {
	if profile == "" {
		return nil
	}

	if sessionValid(profile) {
		slog.Debug("aws session valid", "profile", profile)
		return nil
	}

	if !isInteractive() {
		slog.Warn("aws session expired, no tty for sso login", "profile", profile)
		return fmt.Errorf("AWS session expired for profile %q — run: aws sso login --profile %s", profile, profile)
	}

	slog.Info("aws sso login starting", "profile", profile)
	fmt.Fprintf(os.Stderr, "AWS session expired for profile %q, running sso login...\n", profile)

	cmd := exec.Command("aws", "sso", "login", "--profile", profile)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		slog.Warn("aws sso login failed", "profile", profile, "err", err)
		return fmt.Errorf("aws sso login failed for profile %q: %w", profile, err)
	}

	slog.Info("aws sso login complete", "profile", profile)
	return nil
}

// stsSessionValid returns true if sts get-caller-identity succeeds for the profile.
func stsSessionValid(profile string) bool {
	cmd := exec.Command("aws", "sts", "get-caller-identity", "--profile", profile)
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run() == nil
}

// A ModeCharDevice check isn't enough: /dev/null, the usual stdin for agents and cron, is a character device too.
func stdinIsTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}
