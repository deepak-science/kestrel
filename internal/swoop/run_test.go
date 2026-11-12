package swoop

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func selectRoots() []Root {
	base := "/repo"
	var roots []Root
	for _, p := range []string{"dev/vpc", "dev/rds", "prd/vpc"} {
		roots = append(roots, Root{Path: p, AbsPath: filepath.Join(base, p), Dir: strings.SplitN(p, "/", 2)[0]})
	}
	return roots
}

func paths(roots []Root) []string {
	var out []string
	for _, r := range roots {
		out = append(out, r.Path)
	}
	return out
}

func TestSelect(t *testing.T) {
	tests := []struct {
		name    string
		sel     Selection
		want    []string
		wantErr string
	}{
		{"exact", Selection{Target: "dev/vpc"}, []string{"dev/vpc"}, ""},
		{"unique substring", Selection{Target: "rds"}, []string{"dev/rds"}, ""},
		{"ambiguous substring", Selection{Target: "vpc"}, nil, `target "vpc" matches 2 roots`},
		{"glob", Selection{Target: "*/vpc"}, []string{"dev/vpc", "prd/vpc"}, ""},
		{"dir", Selection{Dir: "dev"}, []string{"dev/vpc", "dev/rds"}, ""},
		{"dir narrows substring", Selection{Target: "vpc", Dir: "prd"}, []string{"prd/vpc"}, ""},
		{"no target, no filters", Selection{}, nil, "matches 3 roots"},
		{"no match", Selection{Target: "zzz/nope"}, nil, ErrNoMatches.Error()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _, err := Select(selectRoots(), "/repo", tt.sel)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if g := paths(got); strings.Join(g, ",") != strings.Join(tt.want, ",") {
				t.Errorf("got %v, want %v", g, tt.want)
			}
		})
	}
}

func TestSelect_NoMatchIsSentinel(t *testing.T) {
	_, _, err := Select(selectRoots(), "/repo", Selection{Dir: "stg"})
	if !errors.Is(err, ErrNoMatches) {
		t.Errorf("err = %v, want ErrNoMatches", err)
	}
}
