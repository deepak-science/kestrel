package runner

import (
	"errors"
	"testing"
)

func TestErrorExcerpt(t *testing.T) {
	fallback := errors.New("exit status 1")
	for name, tc := range map[string]struct{ stderr, want string }{
		"colored tofu box": {
			"\x1b[31m╷\x1b[0m\x1b[0m\n\x1b[31m│\x1b[0m \x1b[0m\x1b[1m\x1b[31mError: \x1b[0m\x1b[0m\x1b[1mNo valid credential sources found\x1b[0m\n\x1b[31m╵\x1b[0m\x1b[0m\n",
			"Error: No valid credential sources found",
		},
		"kubectl":    {"error: the server doesn't have a resource type \"x\"\n", "error: the server doesn't have a resource type \"x\""},
		"last line":  {"warning: a\nsomething broke\n\n", "something broke"},
		"empty":      {"", "exit status 1"},
		"only boxes": {"\x1b[31m╷\x1b[0m\n\x1b[31m╵\x1b[0m\n", "exit status 1"},
	} {
		if got := ErrorExcerpt(tc.stderr, fallback); got != tc.want {
			t.Errorf("%s: ErrorExcerpt = %q, want %q", name, got, tc.want)
		}
	}
}
