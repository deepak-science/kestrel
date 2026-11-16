package swoop

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// moduleRepo lays out a root under iac-live/ that calls a module outside it, which calls a nested module and a registry module.
func moduleRepo(t *testing.T) (repo, rootDir string) {
	t.Helper()
	repo = t.TempDir()
	rootDir = filepath.Join(repo, "iac-live", "dev", "vpc")
	writeFile(t, filepath.Join(rootDir, "main.tf"), `terraform {
  backend "s3" {}
  required_providers {
    aws = { source = "hashicorp/aws" }
  }
}

module "net" {
  source = "../../../modules/net"
}

module "missing" {
  source = "./gone"
}
`)
	writeFile(t, filepath.Join(repo, "modules", "net", "main.tf"), `module "subnet" {
  source = "../subnet"
}

module "registry" {
  source  = "terraform-aws-modules/vpc/aws"
}
`)
	writeFile(t, filepath.Join(repo, "modules", "subnet", "main.tf.json"), `{
  "module": {
    "back": {
      "source": "../net"
    }
  }
}
`)
	writeFile(t, filepath.Join(repo, "modules", "unused", "main.tf"), "")
	return repo, rootDir
}

func TestLocalModuleDirs(t *testing.T) {
	repo, rootDir := moduleRepo(t)
	got := LocalModuleDirs(rootDir)
	// The missing ./gone is kept so its files' deletion can still be matched.
	want := []string{filepath.Join(rootDir, "gone"), filepath.Join(repo, "modules", "net"), filepath.Join(repo, "modules", "subnet")}
	if !slices.Equal(got, want) {
		t.Errorf("LocalModuleDirs = %v, want %v", got, want)
	}
}

func TestRootFingerprint_ModuleDeleted(t *testing.T) {
	repo, rootDir := moduleRepo(t)
	before, err := rootFingerprint(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(repo, "modules", "subnet")); err != nil {
		t.Fatal(err)
	}
	after, err := rootFingerprint(rootDir)
	if err != nil {
		t.Fatalf("fingerprint with a deleted module: %v", err)
	}
	if after == before {
		t.Error("deleting a called module left the fingerprint unchanged")
	}
}

func TestLocalSources_MidLine(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "main.tf"), `module "a" { source = "../a" }
module "r" { source = "hashicorp/consul/aws" }
locals { note = "resource_source = ./not-a-module" }
`)
	writeFile(t, filepath.Join(dir, "gen.tf.json"), `{"module":{"b":{"source":"./b"},"c":{"version":"1","source":"../c"}}}`)
	got := localSources(dir)
	slices.Sort(got)
	want := []string{"../a", "../c", "./b"}
	if !slices.Equal(got, want) {
		t.Errorf("localSources = %v, want %v", got, want)
	}
}

func TestRootFingerprint_CoversLocalModules(t *testing.T) {
	repo, rootDir := moduleRepo(t)
	before, err := rootFingerprint(rootDir)
	if err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(repo, "modules", "unused", "main.tf"), "# edit\n")
	if fp, _ := rootFingerprint(rootDir); fp != before {
		t.Error("an uncalled module changed the fingerprint")
	}

	writeFile(t, filepath.Join(repo, "modules", "subnet", "vars.tf"), `variable "x" {}`)
	if fp, _ := rootFingerprint(rootDir); fp == before {
		t.Error("an edit in a nested local module left the fingerprint unchanged")
	}
}

func TestMatchChangedFiles_LocalModule(t *testing.T) {
	base := "/repo"
	roots := []Root{
		{Path: "dev/vpc", AbsPath: "/repo/iac-live/dev/vpc"},
		{Path: "dev/rds", AbsPath: "/repo/iac-live/dev/rds"},
	}
	modules := map[string][]string{"dev/vpc": {"/repo/modules/net"}}
	changed := matchChangedFiles(roots, modules, base, []string{
		"modules/net/main.tf",
		"modules/net/README.md",
		"modules/net/sub/main.tf",
	})
	if len(changed) != 1 || changed[0].Path != "dev/vpc" {
		t.Errorf("changed = %v, want [dev/vpc]", rootPaths(changed))
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// The discovery base sits below the repo root and the module sits outside it.
func TestChangedRoots_ModuleOutsideBase(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo, rootDir := moduleRepo(t)
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "init")

	base := filepath.Join(repo, "iac-live")
	roots := []Root{{Path: "dev/vpc", AbsPath: rootDir}}

	changed, err := ChangedRoots(roots, base, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 0 {
		t.Fatalf("clean tree reported %v", rootPaths(changed))
	}

	writeFile(t, filepath.Join(repo, "modules", "subnet", "main.tf.json"), "{}\n")
	changed, err = ChangedRoots(roots, base, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 {
		t.Errorf("module edit outside the base selected %v, want [dev/vpc]", rootPaths(changed))
	}

	// diff.relative would otherwise limit the diff to the base and drop the module.
	git(t, repo, "config", "diff.relative", "true")
	if changed, err = ChangedRoots(roots, base, "HEAD"); err != nil || len(changed) != 1 {
		t.Errorf("with diff.relative=true: selected %v, %v, want [dev/vpc]", rootPaths(changed), err)
	}

	// Deleting a module's last file removes its dir; the root that calls it is still selected.
	git(t, repo, "checkout", "-q", "--", ".")
	if err := os.RemoveAll(filepath.Join(repo, "modules", "subnet")); err != nil {
		t.Fatal(err)
	}
	if changed, err = ChangedRoots(roots, base, "HEAD"); err != nil || len(changed) != 1 {
		t.Errorf("module deletion selected %v, %v, want [dev/vpc]", rootPaths(changed), err)
	}
}
