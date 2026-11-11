package swoop

import (
	"strings"
	"testing"
)

// Trimmed from `tofu show -json` of a plan that updates one resource, replaces another, and creates/deletes two more.
const planJSONFixture = `{
  "resource_changes": [
    {"address": "terraform_data.pinned", "change": {
      "actions": ["delete", "create"],
      "before": {"id": "55fa", "input": "a", "output": "a", "triggers_replace": ["41.4.0"]},
      "after": {"input": "a", "triggers_replace": ["42.3.1"]},
      "after_unknown": {"id": true, "output": true, "triggers_replace": [false]},
      "before_sensitive": {"triggers_replace": [false]},
      "after_sensitive": {"triggers_replace": [false]},
      "replace_paths": [["triggers_replace"]]
    }},
    {"address": "helm_release.traefik", "change": {
      "actions": ["update"],
      "before": {"version": "41.4.0", "values": ["a: 1\nb: 2\n"], "set_sensitive": [{"name": "token", "value": "hunter2"}], "replicas": 2},
      "after": {"version": "42.3.1", "values": ["a: 1\nb: 3\n"], "set_sensitive": [{"name": "token", "value": "hunter3"}], "replicas": 2},
      "after_unknown": {},
      "before_sensitive": {"set_sensitive": [{"value": true}]},
      "after_sensitive": {"set_sensitive": [{"value": true}]}
    }},
    {"address": "aws_s3_bucket.logs", "change": {"actions": ["create"], "before": null, "after": {"bucket": "logs"}}},
    {"address": "aws_iam_role.old", "change": {"actions": ["delete"], "before": {"name": "old"}, "after": null}},
    {"address": "aws_vpc.main", "change": {"actions": ["no-op"], "before": {"cidr": "10.0.0.0/16"}, "after": {"cidr": "10.0.0.0/16"}}},
    {"address": "data.aws_caller_identity.me", "change": {"actions": ["read"], "before": null, "after": {}}}
  ],
  "output_changes": {
    "endpoint": {"actions": ["update"], "before": "a", "after": "b"},
    "unchanged": {"actions": ["no-op"], "before": "x", "after": "x"}
  }
}`

func TestFormatPlanSummary(t *testing.T) {
	got, err := FormatPlanSummary([]byte(planJSONFixture))
	if err != nil {
		t.Fatal(err)
	}
	want := `-/+ terraform_data.pinned
      id: "55fa" → (known after apply)
      output: "a" → (known after apply)
      triggers_replace[0]: "41.4.0" → "42.3.1" (forces replacement)
~   helm_release.traefik
      set_sensitive[0].value: (sensitive) → (sensitive)
      values[0]: (10-char string) → (10-char string)
      version: "41.4.0" → "42.3.1"
+   aws_s3_bucket.logs
-   aws_iam_role.old
<=  data.aws_caller_identity.me
~   output.endpoint

Plan: 2 to add, 1 to change, 2 to destroy.
`
	if got != want {
		t.Errorf("summary mismatch\n got:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(got, "hunter") {
		t.Error("summary leaked a sensitive value")
	}
}

func TestFormatPlanSummary_NoChanges(t *testing.T) {
	got, err := FormatPlanSummary([]byte(`{"resource_changes":[{"address":"a.b","change":{"actions":["no-op"]}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got != "No changes.\n" {
		t.Errorf("got %q", got)
	}
}

// A whole-attribute sensitive marker must mask nested values too, even when rendered as a container.
func TestRenderValue_SensitiveContainer(t *testing.T) {
	if got := renderValue(map[string]any{"k": "secret"}, map[string]any{"k": true}); got != "(sensitive)" {
		t.Errorf("got %q", got)
	}
	if got := renderValue("plain", false); got != `"plain"` {
		t.Errorf("got %q", got)
	}
}

func TestFormatPlanSummary_BadJSON(t *testing.T) {
	if _, err := FormatPlanSummary([]byte("nope")); err == nil {
		t.Fatal("expected error")
	}
}
