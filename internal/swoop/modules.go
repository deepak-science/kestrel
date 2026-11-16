package swoop

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// localSourceRE matches a local module source in HCL (`source = "../x"`) or JSON (`"source": "./x"`), anywhere in a line: single-line blocks and minified JSON put it mid-line.
// Terraform treats a source as local only when it starts with ./ or ../, and provider sources never do, so no block parsing is needed.
var localSourceRE = regexp.MustCompile(`(?:^|[\s{,])"?source"?\s*[=:]\s*"(\.\.?/[^"]*)"`)

// LocalModuleDirs returns the absolute dirs of every local module the root calls, following module-to-module calls.
// A missing source dir is skipped; terraform init reports it.
func LocalModuleDirs(rootDir string) []string {
	rootDir = filepath.Clean(rootDir)
	seen := map[string]bool{rootDir: true}
	queue := []string{rootDir}
	var dirs []string
	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]
		for _, src := range localSources(dir) {
			mod := filepath.Join(dir, src)
			if seen[mod] {
				continue
			}
			seen[mod] = true
			if fi, err := os.Stat(mod); err != nil || !fi.IsDir() {
				continue
			}
			dirs = append(dirs, mod)
			queue = append(queue, mod)
		}
	}
	sort.Strings(dirs)
	return dirs
}

func localSources(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var srcs []string
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasSuffix(name, ".tf") && !strings.HasSuffix(name, ".tf.json") {
			continue
		}
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for scanner.Scan() {
			for _, m := range localSourceRE.FindAllStringSubmatch(scanner.Text(), -1) {
				srcs = append(srcs, m[1])
			}
		}
		_ = f.Close()
	}
	return srcs
}
