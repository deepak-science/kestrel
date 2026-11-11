package swoop

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// planJSON is the subset of `terraform show -json <planfile>` the summary reads.
type planJSON struct {
	ResourceChanges []struct {
		Address string       `json:"address"`
		Change  changeObject `json:"change"`
	} `json:"resource_changes"`
	OutputChanges map[string]changeObject `json:"output_changes"`
}

type changeObject struct {
	Actions         []string `json:"actions"`
	Before          any      `json:"before"`
	After           any      `json:"after"`
	AfterUnknown    any      `json:"after_unknown"`
	BeforeSensitive any      `json:"before_sensitive"`
	AfterSensitive  any      `json:"after_sensitive"`
	ReplacePaths    [][]any  `json:"replace_paths"`
}

func actionSymbol(actions []string) string {
	switch strings.Join(actions, ",") {
	case "create":
		return "+"
	case "delete":
		return "-"
	case "update":
		return "~"
	case "delete,create":
		return "-/+"
	case "create,delete":
		return "+/-"
	case "read":
		return "<="
	case "no-op", "":
		return ""
	}
	return strings.Join(actions, ",")
}

// FormatPlanSummary renders `show -json` output as one line per changed resource, with changed attribute paths under updates and replacements.
// Sensitive values are masked; terraform includes them in plaintext in the JSON.
func FormatPlanSummary(data []byte) (string, error) {
	var plan planJSON
	if err := json.Unmarshal(data, &plan); err != nil {
		return "", fmt.Errorf("parse plan json: %w", err)
	}

	var b strings.Builder
	var add, change, destroy int
	for _, rc := range plan.ResourceChanges {
		sym := actionSymbol(rc.Change.Actions)
		if sym == "" {
			continue
		}
		switch sym {
		case "+":
			add++
		case "-":
			destroy++
		case "~":
			change++
		case "-/+", "+/-":
			add++
			destroy++
		}
		fmt.Fprintf(&b, "%-3s %s\n", sym, rc.Address)
		if sym == "~" || sym == "-/+" || sym == "+/-" {
			for _, line := range attrChanges(rc.Change) {
				fmt.Fprintf(&b, "      %s\n", line)
			}
		}
	}

	var outputs []string
	for name, oc := range plan.OutputChanges {
		if sym := actionSymbol(oc.Actions); sym != "" {
			outputs = append(outputs, fmt.Sprintf("%-3s output.%s", sym, name))
		}
	}
	sort.Strings(outputs)
	for _, o := range outputs {
		b.WriteString(o + "\n")
	}

	if add+change+destroy == 0 && len(outputs) == 0 {
		return "No changes.\n", nil
	}
	fmt.Fprintf(&b, "\nPlan: %d to add, %d to change, %d to destroy.\n", add, change, destroy)
	return b.String(), nil
}

func attrChanges(c changeObject) []string {
	var replace []string
	for _, p := range c.ReplacePaths {
		replace = append(replace, formatPath(p))
	}
	var lines []string
	walkChange("", c.Before, c.After, c.BeforeSensitive, c.AfterSensitive, c.AfterUnknown, func(path, from, to string) {
		line := fmt.Sprintf("%s: %s → %s", path, from, to)
		if forcesReplace(path, replace) {
			line += " (forces replacement)"
		}
		lines = append(lines, line)
	})
	return lines
}

// forcesReplace reports whether path is, or is under, one of terraform's replace_paths.
func forcesReplace(path string, replace []string) bool {
	for _, rp := range replace {
		if path == rp || strings.HasPrefix(path, rp+".") || strings.HasPrefix(path, rp+"[") {
			return true
		}
	}
	return false
}

func walkChange(path string, before, after, bSens, aSens, unknown any, emit func(path, from, to string)) {
	if isTrue(unknown) {
		emit(path, renderValue(before, bSens), "(known after apply)")
		return
	}
	bm, bok := before.(map[string]any)
	am, aok := after.(map[string]any)
	if bok && aok {
		keys := map[string]bool{}
		for k := range bm {
			keys[k] = true
		}
		for k := range am {
			keys[k] = true
		}
		if um, ok := unknown.(map[string]any); ok {
			for k := range um {
				keys[k] = true
			}
		}
		sorted := make([]string, 0, len(keys))
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		for _, k := range sorted {
			walkChange(joinPath(path, k), bm[k], am[k], subTree(bSens, k), subTree(aSens, k), subTree(unknown, k), emit)
		}
		return
	}
	ba, bok := before.([]any)
	aa, aok := after.([]any)
	if bok && aok && len(ba) == len(aa) {
		for i := range ba {
			walkChange(fmt.Sprintf("%s[%d]", path, i), ba[i], aa[i], subIndex(bSens, i), subIndex(aSens, i), subIndex(unknown, i), emit)
		}
		return
	}
	if !reflect.DeepEqual(before, after) {
		emit(path, renderValue(before, bSens), renderValue(after, aSens))
	}
}

const maxValueLen = 60

func renderValue(v, sens any) string {
	if containsTrue(sens) {
		return "(sensitive)"
	}
	switch t := v.(type) {
	case nil:
		return "null"
	case map[string]any:
		return fmt.Sprintf("{%d keys}", len(t))
	case []any:
		return fmt.Sprintf("[%d items]", len(t))
	case string:
		if strings.Contains(t, "\n") || len(t) > maxValueLen {
			return fmt.Sprintf("(%d-char string)", len(t))
		}
	}
	out, err := json.Marshal(v)
	if err != nil {
		return "?"
	}
	return string(out)
}

func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func formatPath(steps []any) string {
	var path string
	for _, s := range steps {
		switch t := s.(type) {
		case string:
			path = joinPath(path, t)
		case float64:
			path += "[" + strconv.Itoa(int(t)) + "]"
		}
	}
	return path
}

func isTrue(v any) bool {
	b, ok := v.(bool)
	return ok && b
}

func containsTrue(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case map[string]any:
		for _, x := range t {
			if containsTrue(x) {
				return true
			}
		}
	case []any:
		for _, x := range t {
			if containsTrue(x) {
				return true
			}
		}
	}
	return false
}

// subTree descends a sensitivity/unknown marker tree, where `true` covers everything below it.
func subTree(tree any, key string) any {
	if isTrue(tree) {
		return true
	}
	if m, ok := tree.(map[string]any); ok {
		return m[key]
	}
	return nil
}

func subIndex(tree any, i int) any {
	if isTrue(tree) {
		return true
	}
	if a, ok := tree.([]any); ok && i < len(a) {
		return a[i]
	}
	return nil
}
