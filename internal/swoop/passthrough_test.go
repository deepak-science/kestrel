package swoop

import (
	"reflect"
	"strings"
	"testing"
)

func TestSplitPassthrough(t *testing.T) {
	args := []string{"live/dev", "-detailed-exitcode", "-lock-timeout=5m"}
	pos, extra := SplitPassthrough(args, 1)
	if !reflect.DeepEqual(pos, []string{"live/dev"}) || !reflect.DeepEqual(extra, args[1:]) {
		t.Errorf("dash=1: got %v / %v", pos, extra)
	}
	pos, extra = SplitPassthrough(args[:1], -1)
	if len(pos) != 1 || extra != nil {
		t.Errorf("no dash: got %v / %v", pos, extra)
	}
	pos, extra = SplitPassthrough(args[1:], 0)
	if len(pos) != 0 || len(extra) != 2 {
		t.Errorf("dash=0: got %v / %v", pos, extra)
	}
}

func TestCheckPassthrough(t *testing.T) {
	tests := []struct {
		args    []string
		wantErr string
	}{
		{args: nil},
		{args: []string{"-detailed-exitcode", "-lock-timeout=5m", "-var=region=us-east-1", "--target=aws_s3_bucket.b"}},
		{args: []string{"-var", "region=us-east-1"}, wantErr: `"region=us-east-1"`},
		{args: []string{"plan.tfplan"}, wantErr: "-name=value"},
		{args: []string{"-"}, wantErr: "-name=value"},
		{args: []string{"--", "-unchecked.tfplan"}, wantErr: `"--"`},
		{args: []string{"-auto-approve"}, wantErr: "-auto-approve"},
		{args: []string{"--auto-approve=true"}, wantErr: "-auto-approve"},
		{args: []string{"-out=x.tfplan"}, wantErr: "use --out"},
		{args: []string{"-chdir=../other"}, wantErr: "-chdir"},
	}
	for _, tt := range tests {
		err := CheckPassthrough(tt.args)
		switch {
		case tt.wantErr == "" && err != nil:
			t.Errorf("%v: unexpected error %v", tt.args, err)
		case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
			t.Errorf("%v: error %v, want it to contain %q", tt.args, err, tt.wantErr)
		}
	}
}

func TestDetailedExitCode(t *testing.T) {
	tests := []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{"-lock-timeout=5m"}, false},
		{[]string{"-detailed-exitcode"}, true},
		{[]string{"--detailed-exitcode=true"}, true},
		{[]string{"-detailed-exitcode=false"}, false},
		{[]string{"-detailed-exitcode", "-detailed-exitcode=false"}, false},
	}
	for _, tt := range tests {
		if got := DetailedExitCode(tt.args); got != tt.want {
			t.Errorf("%v: got %v, want %v", tt.args, got, tt.want)
		}
	}
}
