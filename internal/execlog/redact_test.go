package execlog

import (
	"reflect"
	"testing"
)

func TestRedact(t *testing.T) {
	in := []string{"plan", "-var=db_password=hunter2", "--var", "token=abc", "-backend-config=secret_key=xyz", "-backend-config=backend.hcl", "-lock-timeout=5m", "-var"}
	want := []string{"plan", "-var=db_password=***", "--var", "token=***", "-backend-config=secret_key=***", "-backend-config=backend.hcl", "-lock-timeout=5m", "-var"}
	got := Redact(in)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
	if in[1] != "-var=db_password=hunter2" {
		t.Error("Redact modified its input")
	}
}
