package tmuxsession

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type runnerResponse struct {
	stdout string
	stderr string
	err    error
}

type staticRunner struct {
	stdout    string
	stderr    string
	err       error
	responses []runnerResponse
	argv      [][]string
}

func (r *staticRunner) Run(_ context.Context, argv []string, _ []string) (string, string, error) {
	r.argv = append(r.argv, slices.Clone(argv))
	index := len(r.argv) - 1
	if index < len(r.responses) {
		response := r.responses[index]
		return response.stdout, response.stderr, response.err
	}
	return r.stdout, r.stderr, r.err
}

func TestDiscoverSocketFiltersDefaultSessionsAndInvalidRows(t *testing.T) {
	runner := &staticRunner{responses: []runnerResponse{
		{stdout: strings.Join([]string{
			"$1\tcrux-main\t2\t1\t100",
			"$2\tunrelated\t1\t0\t200",
			"bad\tcrux-invalid\t1\t0\t300",
			"$3\tcrux-malformed\tx\t0\t400",
		}, "\n")},
		{stdout: "$1\t0\n$2\t0\n"},
	}}

	sessions, err := discoverSocket(context.Background(), runner, "")
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.Equal(t, Session{ID: "$1", Name: "crux-main", Windows: 2, Attached: 1, Activity: sessions[0].Activity}, sessions[0])
	require.Equal(t, []string{"tmux", "list-sessions", "-F", "#{session_id}\t#{session_name}\t#{session_windows}\t#{session_attached}\t#{session_activity}"}, runner.argv[0])
	require.Equal(t, []string{"tmux", "list-panes", "-a", "-F", "#{session_id}\t#{pane_dead}"}, runner.argv[1])

	runner = &staticRunner{responses: []runnerResponse{
		{stdout: "$4\tcustom-name\t1\t0\t500\n"},
		{stdout: "$4\t0\n"},
	}}
	sessions, err = discoverSocket(context.Background(), runner, CaptureSocket)
	require.NoError(t, err)
	require.Equal(t, "custom-name", sessions[0].Name)
	require.Equal(t, CaptureSocket, sessions[0].Socket)
}

func TestDiscoverSocketRemovesDeadSessions(t *testing.T) {
	runner := &staticRunner{responses: []runnerResponse{
		{stdout: "$1\tdead-capture\t1\t0\t100\n$2\tactive-capture\t1\t0\t200\n"},
		{stdout: "$1\t1\n$2\t0\n"},
		{},
	}}

	sessions, err := discoverSocket(context.Background(), runner, CaptureSocket)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.Equal(t, "$2", sessions[0].ID)
	require.Equal(t, []string{"tmux", "-L", CaptureSocket, "kill-session", "-t", "$1"}, runner.argv[2])
}

func TestDiscoverSocketTreatsMissingServerAsEmpty(t *testing.T) {
	runner := &staticRunner{stderr: "no server running on /tmp/tmux", err: errors.New("exit status 1")}
	sessions, err := discoverSocket(context.Background(), runner, CaptureSocket)
	require.NoError(t, err)
	require.Empty(t, sessions)
}

func TestLeaseAttachCommandUsesFixedArgumentsAndClearsTmuxEnvironment(t *testing.T) {
	t.Setenv("TMUX", "nested")
	t.Setenv("TMUX_PANE", "%1")
	lease := &Lease{session: Session{Socket: CaptureSocket, ID: "$7"}}
	command, err := lease.AttachCommand()
	require.NoError(t, err)
	require.Equal(t, []string{"tmux", "-L", CaptureSocket, "attach-session", "-t", "$7"}, command.Args)
	for _, value := range command.Env {
		require.False(t, strings.HasPrefix(value, "TMUX="))
		require.False(t, strings.HasPrefix(value, "TMUX_PANE="))
	}
	require.NotEmpty(t, os.Getenv("TMUX"))
}

type optionRunner struct {
	global   map[string]string
	explicit map[string]string
	calls    [][]string
}

func (r *optionRunner) Run(_ context.Context, argv []string, _ []string) (string, string, error) {
	r.calls = append(r.calls, slices.Clone(argv))
	args := argv[1:]
	if len(args) >= 2 && args[0] == "-L" {
		args = args[2:]
	}
	if slices.Contains(args, "-gqv") {
		return r.global[args[len(args)-1]] + "\n", "", nil
	}
	if args[0] == "show-options" {
		name := args[len(args)-1]
		if slices.Contains(args, "-qv") {
			if value, ok := r.explicit[name]; ok {
				return value + "\n", "", nil
			}
			return r.global[name] + "\n", "", nil
		}
		if value, ok := r.explicit[name]; ok {
			return name + " " + value + "\n", "", nil
		}
		return "", "", nil
	}
	if args[0] == "set-option" {
		name := args[len(args)-2]
		if args[len(args)-2] == "-u" {
			delete(r.explicit, args[len(args)-1])
			return "", "", nil
		}
		r.explicit[name] = args[len(args)-1]
	}
	return "", "", nil
}

func TestPrepareShowsReturnHintAndRestoresUnchangedOptions(t *testing.T) {
	runner := &optionRunner{
		global:   map[string]string{"prefix": "C-a", "status": "off", "status-right": "clock"},
		explicit: map[string]string{"status-right": "custom"},
	}
	lease, err := prepare(context.Background(), runner, Session{ID: "$3"})
	require.NoError(t, err)
	require.Equal(t, "on", runner.explicit["status"])
	require.Contains(t, runner.explicit["status-right"], "C-a d: return to Crux")

	require.NoError(t, lease.Restore(context.Background()))
	_, statusExplicit := runner.explicit["status"]
	require.False(t, statusExplicit)
	require.Equal(t, "custom", runner.explicit["status-right"])
}

func TestRestorePreservesUserStatusChange(t *testing.T) {
	runner := &optionRunner{
		global:   map[string]string{"prefix": "C-b", "status": "on", "status-right": "clock"},
		explicit: map[string]string{},
	}
	lease, err := prepare(context.Background(), runner, Session{ID: "$4"})
	require.NoError(t, err)
	runner.explicit["status-right"] = "user changed this"
	require.NoError(t, lease.Restore(context.Background()))
	require.Equal(t, "user changed this", runner.explicit["status-right"])
}
