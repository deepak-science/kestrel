package swoop

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVersionNewer(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"1.12.3", "1.11.5", true},
		{"1.11.5", "1.12.3", false},
		{"1.10.0", "1.9.9", true},
		{"1.9.2", "1.9.2", false},
		{"2.0.0", "1.99.99", true},
		{"1.9.x", "1.9.2", false},
	}
	for _, tt := range tests {
		if got := VersionNewer(tt.a, tt.b); got != tt.want {
			t.Errorf("VersionNewer(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestPinTFVersion_KeepsExistingFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".terraform-version"), []byte("1.9.0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// tofuenv would choose .opentofu-version, but the root already pins via .terraform-version.
	file, err := PinTFVersion(Root{AbsPath: dir}, "tofuenv", "1.9.2")
	if err != nil {
		t.Fatal(err)
	}
	if file != ".terraform-version" {
		t.Errorf("wrote %s, want .terraform-version", file)
	}
	if got := readTFVersion(dir); got != "1.9.2" {
		t.Errorf("pin = %q, want 1.9.2", got)
	}
	if _, err := os.Stat(filepath.Join(dir, ".opentofu-version")); err == nil {
		t.Error("created a second pin file")
	}
}

func TestPinTFVersion_NewRootUsesManagerFile(t *testing.T) {
	dir := t.TempDir()
	file, err := PinTFVersion(Root{AbsPath: dir}, "tofuenv", "1.9.2")
	if err != nil || file != ".opentofu-version" {
		t.Fatalf("got %q, %v", file, err)
	}
}
