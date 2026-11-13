package swoop

import (
	"errors"
	"fmt"
	"strings"
)

// ErrChanges means -detailed-exitcode was passed and a plan has changes; kest and kestci exit 2 for it, as terraform does.
var ErrChanges = errors.New("plan has changes")

// SplitPassthrough splits cobra args at "--" (dash is cmd.ArgsLenAtDash()) into positional args and args for terraform.
func SplitPassthrough(args []string, dash int) (positional, extra []string) {
	if dash < 0 {
		return args, nil
	}
	return args[:dash], args[dash:]
}

// managedFlags are set by kest itself, or would skip the review a saved plan gives.
var managedFlags = map[string]string{
	"out":          "use --out",
	"auto-approve": "review with plan --out, then apply --plan",
	"chdir":        "kest already runs terraform in the root's directory",
}

// CheckPassthrough rejects args kest can't forward safely.
// Positional args are refused because `apply <planfile>` would skip the saved-plan checks; requiring -name=value keeps flag values from looking positional.
// A bare "--" ends terraform's flag parsing, so a later "-x.tfplan" would be positional too.
func CheckPassthrough(args []string) error {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") || a == "-" || a == "--" {
			return fmt.Errorf("terraform arg %q isn't forwarded: pass flags as -name=value", a)
		}
		name, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if why, ok := managedFlags[name]; ok {
			return fmt.Errorf("terraform flag -%s isn't forwarded: %s", name, why)
		}
	}
	return nil
}

// DetailedExitCode reports whether args ask plan to exit 2 when there are changes.
func DetailedExitCode(args []string) bool {
	on := false
	for _, a := range args {
		name, val, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if name == "detailed-exitcode" {
			on = !hasVal || val != "false"
		}
	}
	return on
}
