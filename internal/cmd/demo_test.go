package cmd

import "testing"

func TestDemoInvocation(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want bool
	}{{[]string{"demo"}, true}, {[]string{"demo", "--port", "8770"}, true}, {[]string{"--cwd", "/tmp", "demo"}, true}, {[]string{"models"}, false}, {nil, false}} {
		if got := IsDemoInvocation(tt.args); got != tt.want {
			t.Errorf("IsDemoInvocation(%v)=%v", tt.args, got)
		}
	}
}
