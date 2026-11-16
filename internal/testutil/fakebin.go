// Package testutil holds test helpers shared across packages.
package testutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// FakeBin puts a shell script named name at the front of PATH for the rest of the test.
func FakeBin(t *testing.T, name, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake binaries are shell scripts")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
