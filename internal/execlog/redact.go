package execlog

import "strings"

// secretFlags take name=value pairs whose values may be secrets.
var secretFlags = map[string]bool{"var": true, "backend-config": true}

// Redact returns a copy of args with the values of -var and -backend-config pairs masked, for logging.
// A -backend-config file path has no "=" and is kept.
func Redact(args []string) []string {
	out := make([]string, len(args))
	pending := false // previous arg was a secret flag whose pair is the next arg
	for i, a := range args {
		out[i] = a
		if pending {
			pending = false
			out[i] = maskPair(a)
			continue
		}
		if !strings.HasPrefix(a, "-") {
			continue
		}
		name, val, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if !secretFlags[name] {
			continue
		}
		if !hasVal {
			pending = true
			continue
		}
		out[i] = a[:len(a)-len(val)] + maskPair(val)
	}
	return out
}

func maskPair(pair string) string {
	key, _, ok := strings.Cut(pair, "=")
	if !ok {
		return pair
	}
	return key + "=***"
}
