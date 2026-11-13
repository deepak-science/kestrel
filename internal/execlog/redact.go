package execlog

import "strings"

// secretFlags take name=value pairs whose values may be secrets.
// The bool marks helm flags, which take several comma-separated pairs.
var secretFlags = map[string]bool{
	"var":            false,
	"backend-config": false,
	"set":            true,
	"set-string":     true,
	"set-json":       true,
	"set-literal":    true,
}

// Redact returns a copy of args with the values of terraform -var/-backend-config and helm --set pairs masked, for logging.
// A -backend-config file path has no "=" and is kept.
func Redact(args []string) []string {
	out := make([]string, len(args))
	pending, list := false, false // previous arg was a secret flag whose pairs are the next arg
	for i, a := range args {
		out[i] = a
		if pending {
			pending = false
			out[i] = mask(a, list)
			continue
		}
		if !strings.HasPrefix(a, "-") {
			continue
		}
		name, val, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		isList, ok := secretFlags[name]
		if !ok {
			continue
		}
		if !hasVal {
			pending, list = true, isList
			continue
		}
		out[i] = a[:len(a)-len(val)] + mask(val, isList)
	}
	return out
}

func mask(val string, list bool) string {
	if !strings.Contains(val, "=") {
		return val
	}
	if !list {
		key, _, _ := strings.Cut(val, "=")
		return key + "=***"
	}
	// A fragment without "=" is the tail of a value that contained a comma.
	parts := strings.Split(val, ",")
	for i, p := range parts {
		if key, _, ok := strings.Cut(p, "="); ok {
			parts[i] = key + "=***"
		} else {
			parts[i] = "***"
		}
	}
	return strings.Join(parts, ",")
}
