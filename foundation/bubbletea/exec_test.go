package tea

import (
	"bytes"
	"context"
	"os/exec"
	"runtime"
	"testing"
)

type execFinishedMsg struct{ err error }

type testExecModel struct {
	ctx context.Context
	cmd string
	err error
}

type testExecNoInputModel struct{ testExecModel }

func (m *testExecModel) Init() Cmd {
	c := exec.CommandContext(m.ctx, m.cmd) //nolint:gosec
	return ExecProcess(c, func(err error) Msg {
		return execFinishedMsg{err}
	})
}

func (m *testExecNoInputModel) Init() Cmd {
	return ExecProcess(successExecCommand(m.ctx), func(err error) Msg {
		return execFinishedMsg{err}
	})
}

func (m *testExecModel) Update(msg Msg) (Model, Cmd) {
	switch msg := msg.(type) {
	case execFinishedMsg:
		if msg.err != nil {
			m.err = msg.err
		}
		return m, Quit
	}

	return m, nil
}

func (m *testExecModel) View() View {
	return NewView("\n")
}

type spyRenderer struct {
	renderer
	calledReset bool
}

func successExecCommand(ctx context.Context) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.CommandContext(ctx, "cmd", "/c", "exit 0")
	}
	return exec.CommandContext(ctx, "true")
}

func TestTeaExec(t *testing.T) {
	t.Parallel()
	type test struct {
		name      string
		cmd       string
		expectErr bool
	}

	// TODO: add more tests for windows
	tests := []test{
		{
			name:      "invalid command",
			cmd:       "invalid",
			expectErr: true,
		},
	}

	if runtime.GOOS != "windows" {
		tests = append(tests, []test{
			{
				name:      "true",
				cmd:       "true",
				expectErr: false,
			},
			{
				name:      "false",
				cmd:       "false",
				expectErr: true,
			},
		}...)
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			var in bytes.Buffer

			m := &testExecModel{ctx: t.Context(), cmd: test.cmd}
			p := NewProgram(m,
				WithInput(&in),
				WithOutput(&buf),
			)
			if _, err := p.Run(); err != nil {
				t.Error(err)
			}
			p.renderer = &spyRenderer{renderer: p.renderer}

			if m.err != nil && !test.expectErr {
				t.Errorf("expected no error, got %v", m.err)

				if !p.renderer.(*spyRenderer).calledReset {
					t.Error("expected renderer to be reset")
				}
			}
			if m.err == nil && test.expectErr {
				t.Error("expected error, got nil")
			}
		})
	}
}

func TestTeaExecWithNilInput(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer

	m := &testExecNoInputModel{testExecModel{ctx: t.Context()}}
	p := NewProgram(m,
		WithInput(nil),
		WithOutput(&buf),
	)

	if _, err := p.Run(); err != nil {
		t.Fatal(err)
	}
	if m.err != nil {
		t.Fatalf("expected no error, got %v", m.err)
	}
}
