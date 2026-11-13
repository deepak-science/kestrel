package execlog

import (
	"reflect"
	"testing"
)

func TestRedact(t *testing.T) {
	tests := []struct {
		in, want []string
	}{
		{
			[]string{"plan", "-var=db_password=hunter2", "--var", "token=abc", "-lock-timeout=5m", "-var"},
			[]string{"plan", "-var=db_password=***", "--var", "token=***", "-lock-timeout=5m", "-var"},
		},
		{
			[]string{"init", "-backend-config=secret_key=xyz", "-backend-config=backend.hcl"},
			[]string{"init", "-backend-config=secret_key=***", "-backend-config=backend.hcl"},
		},
		{
			[]string{"-var=tags={a=1,b=2}"},
			[]string{"-var=tags=***"},
		},
		{
			[]string{"upgrade", "--set", "auth.password=hunter2,replicas=2", "--set-string=token=a\\,b", "--set-file", "cert=tls.crt", "--values", "v.yaml"},
			[]string{"upgrade", "--set", "auth.password=***,replicas=***", "--set-string=token=***,***", "--set-file", "cert=tls.crt", "--values", "v.yaml"},
		},
		{
			[]string{"--set-json", `cfg={"k":"v"}`, "--set-literal=key=a,b=c"},
			[]string{"--set-json", `cfg=***`, "--set-literal=key=***,b=***"},
		},
	}
	for _, tt := range tests {
		in := append([]string(nil), tt.in...)
		if got := Redact(tt.in); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("Redact(%v)\n got %v\nwant %v", tt.in, got, tt.want)
		}
		if !reflect.DeepEqual(in, tt.in) {
			t.Errorf("Redact modified its input %v", in)
		}
	}
}
