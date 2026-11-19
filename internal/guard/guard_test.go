package guard

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func clearCI(t *testing.T) {
	for _, v := range []string{"CI", "GITHUB_ACTIONS", "GITLAB_CI", "JENKINS_URL", "BUILDKITE"} {
		t.Setenv(v, "")
	}
}

// dirtyRepo makes a git repo on branch feature with an untracked file and chdirs into it.
func dirtyRepo(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	for _, args := range [][]string{
		{"init", "-q", "-b", "feature"},
		{"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile("untracked", nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyCheck(t *testing.T) {
	clearCI(t)
	dirtyRepo(t)

	all := Policy{CI: true, CleanWorktree: true, MainForProd: true}
	cases := []struct {
		name    string
		policy  Policy
		target  string
		wantErr string
	}{
		{"all on, fails on CI first", all, "prod", "not in CI"},
		{"CI off, fails on worktree", Policy{CleanWorktree: true, MainForProd: true}, "prod", "dirty worktree"},
		{"only branch, prod", Policy{MainForProd: true}, "prod", "must be on main"},
		{"only branch, non-prod", Policy{MainForProd: true}, "dev", ""},
		{"only branch, no target", Policy{MainForProd: true}, "", ""},
		{"all off", Policy{}, "prod", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.policy.Check(tc.target)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("Check(%q) = %v, want nil", tc.target, err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("Check(%q) = %v, want error containing %q", tc.target, err, tc.wantErr)
			}
		})
	}
}
