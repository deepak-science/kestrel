package swoop

import (
	"path/filepath"
	"testing"
)

func TestMatchChangedFiles_SingleRoot(t *testing.T) {
	base := "/repo"
	roots := []Root{
		{Path: filepath.Join("dev", "vpc"), AbsPath: filepath.Join(base, "dev", "vpc")},
		{Path: filepath.Join("dev", "rds"), AbsPath: filepath.Join(base, "dev", "rds")},
		{Path: filepath.Join("prd", "vpc"), AbsPath: filepath.Join(base, "prd", "vpc")},
	}

	changed := matchChangedFiles(roots, base, []string{
		filepath.Join("dev", "vpc", "extra.tf"),
	})

	if len(changed) != 1 {
		t.Fatalf("expected 1 changed root, got %d", len(changed))
	}
	if changed[0].Path != filepath.Join("dev", "vpc") {
		t.Errorf("expected dev/vpc, got %s", changed[0].Path)
	}
}

func TestMatchChangedFiles_MultipleRoots(t *testing.T) {
	base := "/repo"
	roots := []Root{
		{Path: filepath.Join("dev", "vpc"), AbsPath: filepath.Join(base, "dev", "vpc")},
		{Path: filepath.Join("dev", "rds"), AbsPath: filepath.Join(base, "dev", "rds")},
		{Path: filepath.Join("prd", "vpc"), AbsPath: filepath.Join(base, "prd", "vpc")},
	}

	changed := matchChangedFiles(roots, base, []string{
		filepath.Join("dev", "vpc", "root.tf"),
		filepath.Join("dev", "rds", "extra.tf"),
	})

	if len(changed) != 2 {
		t.Fatalf("expected 2 changed roots, got %d", len(changed))
	}
}

func TestMatchChangedFiles_IgnoresNonTF(t *testing.T) {
	base := "/repo"
	roots := []Root{
		{Path: filepath.Join("dev", "vpc"), AbsPath: filepath.Join(base, "dev", "vpc")},
	}

	changed := matchChangedFiles(roots, base, []string{
		filepath.Join("dev", "vpc", "README.md"),
		filepath.Join("dev", "vpc", "notes.txt"),
	})

	if len(changed) != 0 {
		t.Fatalf("expected 0 changed roots, got %d", len(changed))
	}
}

func TestMatchChangedFiles_NoMatches(t *testing.T) {
	base := "/repo"
	roots := []Root{
		{Path: filepath.Join("dev", "vpc"), AbsPath: filepath.Join(base, "dev", "vpc")},
	}

	changed := matchChangedFiles(roots, base, []string{
		filepath.Join("prd", "vpc", "root.tf"),
	})

	if len(changed) != 0 {
		t.Fatalf("expected 0 changed roots, got %d", len(changed))
	}
}

func TestMatchChangedFiles_EmptyFiles(t *testing.T) {
	base := "/repo"
	roots := []Root{
		{Path: filepath.Join("dev", "vpc"), AbsPath: filepath.Join(base, "dev", "vpc")},
	}

	changed := matchChangedFiles(roots, base, nil)
	if len(changed) != 0 {
		t.Fatalf("expected 0 changed roots, got %d", len(changed))
	}
}

func TestMatchChangedFiles_NoDuplicates(t *testing.T) {
	base := "/repo"
	roots := []Root{
		{Path: filepath.Join("dev", "vpc"), AbsPath: filepath.Join(base, "dev", "vpc")},
	}

	// Two files in the same root — should still return one root.
	changed := matchChangedFiles(roots, base, []string{
		filepath.Join("dev", "vpc", "root.tf"),
		filepath.Join("dev", "vpc", "extra.tf"),
	})

	if len(changed) != 1 {
		t.Fatalf("expected 1 changed root, got %d", len(changed))
	}
}

func TestMatchDirtyFiles_SingleRoot(t *testing.T) {
	base := "/repo"
	roots := []Root{
		{Path: filepath.Join("dev", "vpc"), AbsPath: filepath.Join(base, "dev", "vpc")},
		{Path: filepath.Join("dev", "rds"), AbsPath: filepath.Join(base, "dev", "rds")},
	}

	dirty := matchDirtyFiles(roots, base, []string{
		filepath.Join("dev", "vpc", "main.tf"),
	})

	if len(dirty) != 1 {
		t.Fatalf("expected 1 dirty root, got %d", len(dirty))
	}
	if !dirty[filepath.Join("dev", "vpc")] {
		t.Error("expected dev/vpc to be dirty")
	}
}

func TestMatchDirtyFiles_AnyFileType(t *testing.T) {
	base := "/repo"
	roots := []Root{
		{Path: filepath.Join("dev", "vpc"), AbsPath: filepath.Join(base, "dev", "vpc")},
	}

	// Unlike ChangedRoots which only matches .tf files, dirty detection matches any file.
	dirty := matchDirtyFiles(roots, base, []string{
		filepath.Join("dev", "vpc", "README.md"),
	})

	if len(dirty) != 1 {
		t.Fatalf("expected 1 dirty root, got %d", len(dirty))
	}
}

func TestMatchDirtyFiles_NoMatches(t *testing.T) {
	base := "/repo"
	roots := []Root{
		{Path: filepath.Join("dev", "vpc"), AbsPath: filepath.Join(base, "dev", "vpc")},
	}

	dirty := matchDirtyFiles(roots, base, []string{
		filepath.Join("prd", "vpc", "main.tf"),
	})

	if len(dirty) != 0 {
		t.Fatalf("expected 0 dirty roots, got %d", len(dirty))
	}
}

func TestMatchDirtyFiles_NoDuplicates(t *testing.T) {
	base := "/repo"
	roots := []Root{
		{Path: filepath.Join("dev", "vpc"), AbsPath: filepath.Join(base, "dev", "vpc")},
	}

	dirty := matchDirtyFiles(roots, base, []string{
		filepath.Join("dev", "vpc", "main.tf"),
		filepath.Join("dev", "vpc", "vars.tf"),
		filepath.Join("dev", "vpc", "notes.md"),
	})

	if len(dirty) != 1 {
		t.Fatalf("expected 1 dirty root, got %d", len(dirty))
	}
}

func TestPathContains(t *testing.T) {
	tests := []struct {
		parent, child string
		want          bool
	}{
		{"/a/b", "/a/b", true},
		{"/a/b", "/a/b/c", true},
		{"/a/b", "/a/b/c/d.tf", true},
		{"/a/b", "/a/bc", false},
		{"/a/b", "/a", false},
	}
	for _, tt := range tests {
		got := pathContains(tt.parent, tt.child)
		if got != tt.want {
			t.Errorf("pathContains(%q, %q) = %v, want %v", tt.parent, tt.child, got, tt.want)
		}
	}
}

func TestMatchChangedFiles_NonTFInputs(t *testing.T) {
	base := "/repo"
	roots := []Root{
		{Path: "vars", AbsPath: filepath.Join(base, "vars")},
		{Path: "lock", AbsPath: filepath.Join(base, "lock")},
		{Path: "pin", AbsPath: filepath.Join(base, "pin")},
		{Path: "docs", AbsPath: filepath.Join(base, "docs")},
	}
	changed := matchChangedFiles(roots, base, []string{
		filepath.Join("vars", "prod.tfvars"),
		filepath.Join("lock", ".terraform.lock.hcl"),
		filepath.Join("pin", ".opentofu-version"),
		filepath.Join("docs", "README.md"),
	})
	var got []string
	for _, r := range changed {
		got = append(got, r.Path)
	}
	if len(got) != 3 || got[0] != "vars" || got[1] != "lock" || got[2] != "pin" {
		t.Errorf("changed = %v, want [vars lock pin]", got)
	}
}
