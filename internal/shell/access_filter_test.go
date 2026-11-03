package shell

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedirectCapture(t *testing.T) {
	for _, test := range []struct {
		name    string
		command string
		output  string
		file    string
	}{
		{name: "overwrite", command: "printf first > output.log", output: "first", file: "first"},
		{name: "append", command: "printf first > output.log; printf second >> output.log", output: "firstsecond", file: "firstsecond"},
		{name: "stderr", command: "{ printf error >&2; } 2> output.log", output: "error", file: "error"},
		{name: "combined", command: "{ printf out; printf err >&2; } > output.log 2>&1", output: "outerr", file: "outerr"},
		{name: "subshell", command: "(printf nested > output.log)", output: "nested", file: "nested"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			var stdout, stderr bytes.Buffer
			err := Run(WithRedirectCapture(t.Context()), RunOptions{Command: test.command, Cwd: root, Env: os.Environ(), Stdout: &stdout, Stderr: &stderr})
			if err != nil {
				t.Fatal(err)
			}
			if stdout.String() != test.output {
				t.Fatalf("captured output = %q, want %q", stdout.String(), test.output)
			}
			data, err := os.ReadFile(filepath.Join(root, "output.log"))
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != test.file {
				t.Fatalf("redirected output = %q, want %q", data, test.file)
			}
		})
	}
}

func TestRedirectCaptureIsOptIn(t *testing.T) {
	root := t.TempDir()
	var stdout bytes.Buffer
	if err := Run(t.Context(), RunOptions{Command: "printf hidden > output.log", Cwd: root, Stdout: &stdout}); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("ordinary redirection leaked output: %q", stdout.String())
	}
}

func TestRedirectCaptureRejectsNestedShellRedirects(t *testing.T) {
	for _, command := range []string{
		"sh -c 'printf hidden > output.log'",
		"/bin/sh -c 'printf hidden >> output.log'",
		"env sh -c 'printf hidden 2> output.log'",
		"bash -lc 'printf hidden &> output.log'",
		"bash -c 'printf hidden >& output.log'",
		"sh -c '(printf hidden > output.log)'",
	} {
		t.Run(command, func(t *testing.T) {
			root := t.TempDir()
			err := Run(WithRedirectCapture(t.Context()), RunOptions{Command: command, Cwd: root, Env: os.Environ()})
			if err == nil || !strings.Contains(err.Error(), "move the redirection outside") {
				t.Fatalf("expected actionable redirection error, got %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, "output.log")); !os.IsNotExist(err) {
				t.Fatalf("nested shell created output file: %v", err)
			}
		})
	}
}

func TestNestedShellRedirectInspectionAllowsOtherArguments(t *testing.T) {
	for _, args := range [][]string{
		{"sh", "-c", "printf '%s' 'text > example'"},
		{"sh", "-c", "cat < input.txt"},
		{"sh", "-c", "printf error >&2"},
		{"sh", "-c", "printf out 2>&1"},
		{"program", "--log-file", "output.log"},
	} {
		if err := rejectNestedOutputRedirects(args); err != nil {
			t.Fatalf("rejected %q: %v", args, err)
		}
	}
}

func TestNestedShellOuterRedirectCapture(t *testing.T) {
	for _, capture := range []bool{false, true} {
		root := t.TempDir()
		ctx := t.Context()
		command := "sh -c 'printf retained > output.log'"
		if capture {
			ctx = WithRedirectCapture(ctx)
			command = "sh -c 'printf retained' > output.log"
		}
		var stdout bytes.Buffer
		if err := Run(ctx, RunOptions{Command: command, Cwd: root, Env: os.Environ(), Stdout: &stdout}); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(root, "output.log"))
		if err != nil || string(data) != "retained" {
			t.Fatalf("destination = %q, error = %v", data, err)
		}
		if capture && stdout.String() != "retained" || !capture && stdout.Len() != 0 {
			t.Fatalf("capture=%v stdout=%q", capture, stdout.String())
		}
	}
}

func TestRedirectCaptureStatefulShell(t *testing.T) {
	commandShell := NewShell(&Options{WorkingDir: t.TempDir()})
	stdout, _, err := commandShell.Exec(WithRedirectCapture(context.Background()), "printf retained > output.log")
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "retained" {
		t.Fatalf("captured output = %q", stdout)
	}
}
