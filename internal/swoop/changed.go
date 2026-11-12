package swoop

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// ChangedRoots returns the subset of roots whose config files changed
// relative to the given git ref. If ref is empty, it defaults to the merge-base
// with main (or master).
func ChangedRoots(roots []Root, baseDir, ref string) ([]Root, error) {
	if ref == "" {
		var err error
		ref, err = mergeBase(baseDir)
		if err != nil {
			return nil, err
		}
	}

	changedFiles, err := gitDiffFiles(baseDir, ref)
	if err != nil {
		return nil, err
	}

	return matchChangedFiles(roots, baseDir, changedFiles), nil
}

// matchChangedFiles is the pure logic: given a list of changed file paths
// (relative to baseDir), return the roots that contain changed .tf files.
func matchChangedFiles(roots []Root, baseDir string, changedFiles []string) []Root {
	changedRootPaths := make(map[string]bool)
	for _, f := range changedFiles {
		// Same inputs the saved-plan fingerprint covers, plus the version pin.
		if name := filepath.Base(f); !fingerprintFiles(name) && name != ".opentofu-version" && name != ".terraform-version" {
			continue
		}
		dir := filepath.Dir(f)
		for _, r := range roots {
			if pathContains(r.AbsPath, filepath.Join(baseDir, dir)) {
				changedRootPaths[r.Path] = true
			}
		}
	}

	var result []Root
	for _, r := range roots {
		if changedRootPaths[r.Path] {
			result = append(result, r)
		}
	}
	return result
}

// mergeBase finds the merge-base between HEAD and main (or master).
func mergeBase(dir string) (string, error) {
	// CI checkouts often have only the remote-tracking branch.
	for _, branch := range []string{"main", "master", "origin/main", "origin/master"} {
		cmd := exec.Command("git", "merge-base", "HEAD", branch)
		cmd.Dir = dir
		out, err := cmd.Output()
		if err == nil {
			return strings.TrimSpace(string(out)), nil
		}
	}
	// Falling back to HEAD would report "nothing changed" for every committed change.
	return "", fmt.Errorf("--changed: no merge-base with main or master (a shallow checkout needs fetch-depth: 0); pass a ref, e.g. --changed=HEAD~1")
}

// gitDiffFiles returns the files changed between ref and the working tree, relative to dir (the discovery base, which may be below the repo root).
func gitDiffFiles(dir, ref string) ([]string, error) {
	cmd := exec.Command("git", "diff", "--name-only", "--relative", ref)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

// DirtyRoots returns the set of root paths that have uncommitted changes
// (staged or unstaged). Uses a single git status call for the whole base dir.
func DirtyRoots(roots []Root, baseDir string) map[string]bool {
	files, err := gitStatusFiles(baseDir)
	if err != nil {
		return nil
	}
	return matchDirtyFiles(roots, baseDir, files)
}

// matchDirtyFiles is the pure logic: given dirty file paths (relative to repo root),
// return the set of root paths that contain at least one dirty file.
func matchDirtyFiles(roots []Root, baseDir string, dirtyFiles []string) map[string]bool {
	result := make(map[string]bool)
	for _, f := range dirtyFiles {
		dir := filepath.Dir(f)
		for _, r := range roots {
			if pathContains(r.AbsPath, filepath.Join(baseDir, dir)) {
				result[r.Path] = true
			}
		}
	}
	return result
}

// gitStatusFiles returns file paths with uncommitted changes (relative to repo root).
func gitStatusFiles(dir string) ([]string, error) {
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if len(line) < 4 {
			continue
		}
		// Porcelain format: XY <space> <path> (first 3 chars are status + space).
		file := strings.TrimSpace(line[3:])
		if file != "" {
			files = append(files, file)
		}
	}
	return files, nil
}

// EnrichGitStatus sets GitDirty on each root that has uncommitted changes.
func EnrichGitStatus(roots []Root, baseDir string) {
	dirty := DirtyRoots(roots, baseDir)
	for i := range roots {
		if dirty[roots[i].Path] {
			roots[i].GitDirty = true
		}
	}
}

// pathContains returns true if child is inside or equal to parent.
func pathContains(parent, child string) bool {
	parent = filepath.Clean(parent)
	child = filepath.Clean(child)
	if child == parent {
		return true
	}
	return strings.HasPrefix(child, parent+string(filepath.Separator))
}
