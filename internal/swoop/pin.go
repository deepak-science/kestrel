package swoop

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// PinFileFor returns the root's existing version-pin file (as readTFVersion picks it), else the one the manager uses.
func PinFileFor(root Root, manager string) string {
	for _, name := range []string{".opentofu-version", ".terraform-version"} {
		if _, err := os.Stat(filepath.Join(root.AbsPath, name)); err == nil {
			return name
		}
	}
	return VersionFileFor(manager)
}

// PinTFVersion writes version into the root's pin file, replacing any existing pin.
func PinTFVersion(root Root, manager, version string) (string, error) {
	name := PinFileFor(root, manager)
	if err := os.WriteFile(filepath.Join(root.AbsPath, name), []byte(version+"\n"), 0644); err != nil {
		return "", fmt.Errorf("could not write %s: %w", name, err)
	}
	return name, nil
}

// ActiveTFVersion runs `<command> version` in dir and returns the version it reports.
func ActiveTFVersion(command, dir string) (string, error) {
	cmd := exec.Command(command, "version")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("could not detect %s version: %w", command, err)
	}
	v := ParseTFVersion(string(out))
	if v == "" {
		return "", fmt.Errorf("could not parse %s version from output", command)
	}
	return v, nil
}

// VersionNewer reports whether a is a later x.y.z release than b.
func VersionNewer(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		na, errA := strconv.Atoi(pa[i])
		nb, errB := strconv.Atoi(pb[i])
		if errA != nil || errB != nil {
			return false
		}
		if na != nb {
			return na > nb
		}
	}
	return len(pa) > len(pb)
}
