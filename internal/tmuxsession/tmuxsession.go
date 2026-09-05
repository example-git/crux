package tmuxsession

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const CaptureSocket = "crux-capture"

const maxSessions = 100

var sessionIDPattern = regexp.MustCompile(`^\$[0-9]+$`)

type Session struct {
	Socket   string
	ID       string
	Name     string
	Windows  int
	Attached int
	Activity time.Time
}

type runner interface {
	Run(context.Context, []string, []string) (string, string, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, argv []string, env []string) (string, string, error) {
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if env != nil {
		command.Env = env
	}
	var stdout strings.Builder
	var stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return stdout.String(), stderr.String(), err
}

func Discover(ctx context.Context) ([]Session, error) {
	return discover(ctx, execRunner{})
}

func discover(ctx context.Context, commandRunner runner) ([]Session, error) {
	if _, err := exec.LookPath("tmux"); err != nil {
		return nil, fmt.Errorf("tmux is not installed or not available in PATH")
	}
	sources := []string{"", CaptureSocket}
	result := make([]Session, 0)
	for _, socket := range sources {
		sessions, err := discoverSocket(ctx, commandRunner, socket)
		if err != nil {
			return nil, err
		}
		result = append(result, sessions...)
		if len(result) >= maxSessions {
			result = result[:maxSessions]
			break
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Activity.Equal(result[j].Activity) {
			return result[i].Name < result[j].Name
		}
		return result[i].Activity.After(result[j].Activity)
	})
	return result, nil
}

func discoverSocket(ctx context.Context, commandRunner runner, socket string) ([]Session, error) {
	format := "#{session_id}\t#{session_name}\t#{session_windows}\t#{session_attached}\t#{session_activity}"
	stdout, stderr, err := commandRunner.Run(ctx, tmuxArgs(socket, "list-sessions", "-F", format), nil)
	if err != nil {
		if isMissingServer(stderr) {
			return nil, nil
		}
		return nil, fmt.Errorf("list tmux sessions on %s: %w: %s", socketLabel(socket), err, strings.TrimSpace(stderr))
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	candidates := make([]Session, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 5 || !sessionIDPattern.MatchString(parts[0]) {
			continue
		}
		if socket == "" && !strings.HasPrefix(parts[1], "crux-") {
			continue
		}
		windows, windowsErr := strconv.Atoi(parts[2])
		attached, attachedErr := strconv.Atoi(parts[3])
		activity, activityErr := strconv.ParseInt(parts[4], 10, 64)
		if windowsErr != nil || attachedErr != nil || activityErr != nil {
			continue
		}
		candidates = append(candidates, Session{Socket: socket, ID: parts[0], Name: parts[1], Windows: windows, Attached: attached, Activity: time.Unix(activity, 0)})
	}
	paneStates, err := sessionPaneStates(ctx, commandRunner, socket)
	if err != nil {
		return nil, err
	}
	result := make([]Session, 0, len(candidates))
	for _, session := range candidates {
		if paneStates[session.ID] {
			if len(result) < maxSessions {
				result = append(result, session)
			}
			continue
		}
		_, stderr, killErr := commandRunner.Run(ctx, tmuxArgs(socket, "kill-session", "-t", session.ID), nil)
		if killErr != nil && !isMissingServer(stderr) && !isMissingSession(stderr) {
			return nil, fmt.Errorf("remove dead tmux session %s on %s: %w: %s", session.ID, socketLabel(socket), killErr, strings.TrimSpace(stderr))
		}
	}
	return result, nil
}

func isMissingServer(stderr string) bool {
	value := strings.ToLower(stderr)
	return strings.Contains(value, "no server running") || strings.Contains(value, "failed to connect to server") || strings.Contains(value, "no sessions")
}

func isMissingSession(stderr string) bool {
	value := strings.ToLower(stderr)
	return strings.Contains(value, "can't find session") || strings.Contains(value, "no such session")
}

func sessionPaneStates(ctx context.Context, commandRunner runner, socket string) (map[string]bool, error) {
	format := "#{session_id}\t#{pane_dead}"
	stdout, stderr, err := commandRunner.Run(ctx, tmuxArgs(socket, "list-panes", "-a", "-F", format), nil)
	if err != nil {
		if isMissingServer(stderr) {
			return map[string]bool{}, nil
		}
		return nil, fmt.Errorf("list tmux panes on %s: %w: %s", socketLabel(socket), err, strings.TrimSpace(stderr))
	}
	active := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) != 2 || !sessionIDPattern.MatchString(parts[0]) {
			continue
		}
		if parts[1] == "0" {
			active[parts[0]] = true
		}
	}
	return active, nil
}

func socketLabel(socket string) string {
	if socket == "" {
		return "default socket"
	}
	return socket
}

func tmuxArgs(socket string, arguments ...string) []string {
	result := []string{"tmux"}
	if socket != "" {
		result = append(result, "-L", socket)
	}
	return append(result, arguments...)
}

type optionLease struct {
	name      string
	explicit  bool
	original  string
	temporary string
}

type Lease struct {
	runner  runner
	session Session
	options []optionLease
}

func Prepare(ctx context.Context, session Session) (*Lease, error) {
	return prepare(ctx, execRunner{}, session)
}

func prepare(ctx context.Context, commandRunner runner, session Session) (*Lease, error) {
	if !sessionIDPattern.MatchString(session.ID) {
		return nil, fmt.Errorf("invalid tmux session ID %q", session.ID)
	}
	prefix, _, err := runValue(ctx, commandRunner, session.Socket, "show-options", "-gqv", "prefix")
	if err != nil || strings.TrimSpace(prefix) == "" {
		prefix = "C-b"
	}
	statusRight, err := snapshotOption(ctx, commandRunner, session, "status-right")
	if err != nil {
		return nil, err
	}
	status, err := snapshotOption(ctx, commandRunner, session, "status")
	if err != nil {
		return nil, err
	}
	hint := fmt.Sprintf("#[reverse] %s d: return to Crux #[default]", strings.TrimSpace(prefix))
	statusRight.temporary = hint
	if statusRight.original != "" {
		statusRight.temporary = statusRight.original + " " + hint
	}
	status.temporary = "on"
	lease := &Lease{runner: commandRunner, session: session, options: []optionLease{status, statusRight}}
	for index := range lease.options {
		option := lease.options[index]
		if _, stderr, setErr := commandRunner.Run(ctx, tmuxArgs(session.Socket, "set-option", "-q", "-t", session.ID, option.name, option.temporary), nil); setErr != nil {
			_ = lease.restorePrefix(ctx, index)
			return nil, fmt.Errorf("set temporary tmux %s: %w: %s", option.name, setErr, strings.TrimSpace(stderr))
		}
	}
	return lease, nil
}

func snapshotOption(ctx context.Context, commandRunner runner, session Session, name string) (optionLease, error) {
	raw, _, rawErr := runValue(ctx, commandRunner, session.Socket, "show-options", "-q", "-t", session.ID, name)
	if rawErr != nil {
		return optionLease{}, fmt.Errorf("read tmux %s: %w", name, rawErr)
	}
	effective, stderr, effectiveErr := runValue(ctx, commandRunner, session.Socket, "show-options", "-qv", "-t", session.ID, name)
	if effectiveErr != nil {
		return optionLease{}, fmt.Errorf("read effective tmux %s: %w: %s", name, effectiveErr, strings.TrimSpace(stderr))
	}
	value := strings.TrimSuffix(effective, "\n")
	explicit := strings.TrimSpace(raw) != ""
	if explicit {
		line := strings.TrimSuffix(raw, "\n")
		if _, parsed, found := strings.Cut(line, " "); found {
			value = parsed
		}
	}
	return optionLease{name: name, explicit: explicit, original: value}, nil
}

func runValue(ctx context.Context, commandRunner runner, socket string, arguments ...string) (string, string, error) {
	return commandRunner.Run(ctx, tmuxArgs(socket, arguments...), nil)
}

func (lease *Lease) AttachCommand() (*exec.Cmd, error) {
	if lease == nil || !sessionIDPattern.MatchString(lease.session.ID) {
		return nil, errors.New("invalid tmux attachment lease")
	}
	argv := tmuxArgs(lease.session.Socket, "attach-session", "-t", lease.session.ID)
	command := exec.CommandContext(context.Background(), argv[0], argv[1:]...)
	command.Env = withoutTmuxEnvironment(os.Environ())
	return command, nil
}

func withoutTmuxEnvironment(environment []string) []string {
	result := make([]string, 0, len(environment))
	for _, value := range environment {
		name, _, _ := strings.Cut(value, "=")
		if name == "TMUX" || name == "TMUX_PANE" {
			continue
		}
		result = append(result, value)
	}
	return result
}

func (lease *Lease) Restore(ctx context.Context) error {
	if lease == nil {
		return nil
	}
	return lease.restorePrefix(ctx, len(lease.options))
}

func (lease *Lease) restorePrefix(ctx context.Context, count int) error {
	var result error
	for index := count - 1; index >= 0; index-- {
		option := lease.options[index]
		current, _, err := runValue(ctx, lease.runner, lease.session.Socket, "show-options", "-qv", "-t", lease.session.ID, option.name)
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		if strings.TrimSuffix(current, "\n") != option.temporary {
			continue
		}
		arguments := []string{"set-option", "-q", "-t", lease.session.ID}
		if option.explicit {
			arguments = append(arguments, option.name, option.original)
		} else {
			arguments = append(arguments, "-u", option.name)
		}
		_, stderr, restoreErr := lease.runner.Run(ctx, tmuxArgs(lease.session.Socket, arguments...), nil)
		if restoreErr != nil {
			result = errors.Join(result, fmt.Errorf("restore tmux %s: %w: %s", option.name, restoreErr, strings.TrimSpace(stderr)))
		}
	}
	return result
}
