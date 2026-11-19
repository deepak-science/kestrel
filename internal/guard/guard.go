package guard

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// IsCI returns true if running in a CI environment.
func IsCI() bool {
	for _, v := range []string{"CI", "GITHUB_ACTIONS", "GITLAB_CI", "JENKINS_URL", "BUILDKITE"} {
		if os.Getenv(v) != "" {
			return true
		}
	}
	return false
}

// CheckCI returns an error if we're not in a CI environment.
func CheckCI() error {
	if IsCI() {
		return nil
	}
	return fmt.Errorf("not in CI environment, no deploys! Use --force, or set guards.require_ci: false in the global config")
}

// Policy selects which apply guards run.
type Policy struct {
	CI            bool
	CleanWorktree bool
	MainForProd   bool
}

// Check runs the enabled guards. An empty targetEnv skips the branch guard.
func (p Policy) Check(targetEnv string) error {
	if p.CI {
		if err := CheckCI(); err != nil {
			return err
		}
	}
	if p.CleanWorktree {
		if err := CheckCleanWorktree(); err != nil {
			return err
		}
	}
	if p.MainForProd {
		if err := CheckBranch(targetEnv); err != nil {
			return err
		}
	}
	return nil
}

// CheckCleanWorktree returns an error if the git working tree is dirty.
func CheckCleanWorktree() error {
	out, err := exec.Command("git", "status", "--porcelain").Output()
	if err != nil {
		return fmt.Errorf("checking git status: %w", err)
	}
	if strings.TrimSpace(string(out)) != "" {
		return fmt.Errorf("cannot deploy from dirty worktree — commit or stash your changes")
	}
	return nil
}

// CheckBranch returns an error if deploying to prod from a non-main branch.
func CheckBranch(targetEnv string) error {
	if targetEnv != "prod" {
		return nil
	}
	out, err := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return fmt.Errorf("checking git branch: %w", err)
	}
	branch := strings.TrimSpace(string(out))
	if branch != "main" && branch != "master" {
		return fmt.Errorf("cannot deploy to prod from branch %q — must be on main", branch)
	}
	return nil
}
