//go:build !windows

package terminalhttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/example-git/crux/internal/backend"
	"github.com/example-git/crux/internal/tmuxsession"
	"github.com/gorilla/websocket"
)

const (
	maxTerminals          = 8
	terminalInputLimit    = 1 << 20
	terminalWriteTimeout  = 10 * time.Second
	terminalHangupTimeout = 3 * time.Second
	terminalDrainTimeout  = time.Second
)

type terminalControl struct {
	Type string `json:"type"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

type Exit struct {
	Type  string `json:"type"`
	Code  int    `json:"code"`
	Error string `json:"error,omitempty"`
}

type tmuxSessionView struct {
	Socket   string    `json:"socket"`
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Windows  int       `json:"windows"`
	Attached int       `json:"attached"`
	Activity time.Time `json:"activity"`
}

func Size(query url.Values) (*pty.Winsize, error) {
	parse := func(name string, fallback int) (uint16, error) {
		value := query.Get(name)
		if value == "" {
			return uint16(fallback), nil
		}
		number, err := strconv.Atoi(value)
		if err != nil || number < 2 || number > 1000 {
			return 0, errors.New(name + " must be a whole number from 2 to 1000")
		}
		return uint16(number), nil
	}
	cols, err := parse("cols", 80)
	if err != nil {
		return nil, err
	}
	rows, err := parse("rows", 24)
	if err != nil {
		return nil, err
	}
	return &pty.Winsize{Cols: cols, Rows: rows}, nil
}

func terminalShell() string {
	if shell := os.Getenv("SHELL"); shell != "" {
		return shell
	}
	return "/bin/sh"
}

func Environment(environment []string) []string {
	result := make([]string, 0, len(environment)+2)
	for _, value := range environment {
		if strings.HasPrefix(value, "TERM=") || strings.HasPrefix(value, "COLORTERM=") {
			continue
		}
		result = append(result, value)
	}
	return append(result, "TERM=xterm-256color", "COLORTERM=truecolor")
}

func findTmuxSession(ctx context.Context, socket, id string) (tmuxsession.Session, error) {
	sessions, err := tmuxsession.Discover(ctx)
	if err != nil {
		return tmuxsession.Session{}, err
	}
	for _, session := range sessions {
		if session.Socket == socket && session.ID == id {
			return session, nil
		}
	}
	return tmuxsession.Session{}, errors.New("the tmux session is no longer available")
}

func Sessions() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		sessions, err := tmuxsession.Discover(ctx)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		views := make([]tmuxSessionView, len(sessions))
		for index, session := range sessions {
			views[index] = tmuxSessionView(session)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(views)
	}
}

func Handler(native *backend.Backend) http.HandlerFunc {
	slots := make(chan struct{}, maxTerminals)
	upgrader := websocket.Upgrader{ReadBufferSize: 32 << 10, WriteBufferSize: 32 << 10}
	return func(w http.ResponseWriter, r *http.Request) {
		ws, err := native.GetWorkspace(r.PathValue("id"))
		if err != nil {
			http.Error(w, "workspace not found", http.StatusNotFound)
			return
		}
		query := r.URL.Query()
		size, err := Size(query)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		tmuxID, tmuxSocket := query.Get("tmux_session"), query.Get("tmux_socket")
		if tmuxID == "" && query.Has("tmux_socket") {
			http.Error(w, "tmux_socket requires tmux_session", http.StatusBadRequest)
			return
		}
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		connection.SetReadLimit(terminalInputLimit)
		fail := func(message string) {
			finishTerminal(connection, nil, Exit{Type: "exit", Code: -1, Error: message})
		}
		select {
		case slots <- struct{}{}:
		default:
			fail("Too many terminals are open. Close one and retry.")
			return
		}
		defer func() { <-slots }()
		var command *exec.Cmd
		if tmuxID != "" {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			session, findErr := findTmuxSession(ctx, tmuxSocket, tmuxID)
			var lease *tmuxsession.Lease
			if findErr == nil {
				lease, findErr = tmuxsession.Prepare(ctx, session)
			}
			cancel()
			if findErr != nil {
				fail(findErr.Error())
				return
			}
			defer restoreTerminalLease(lease)
			if command, err = lease.AttachCommand(); err != nil {
				fail(err.Error())
				return
			}
			command.Env = Environment(command.Env)
		} else {
			command = exec.Command(terminalShell(), "-l")
			command.Env = Environment(os.Environ())
		}
		command.Dir = ws.Path
		Run(connection, command, size)
	}
}

func restoreTerminalLease(lease *tmuxsession.Lease) {
	if lease == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = lease.Restore(ctx)
}

func finishTerminal(connection *websocket.Conn, writeMutex *sync.Mutex, exit Exit) {
	if writeMutex != nil {
		writeMutex.Lock()
		defer writeMutex.Unlock()
	}
	data, _ := json.Marshal(exit)
	_ = connection.SetWriteDeadline(time.Now().Add(terminalWriteTimeout))
	_ = connection.WriteMessage(websocket.TextMessage, data)
	_ = connection.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(terminalWriteTimeout))
}

func Run(connection *websocket.Conn, command *exec.Cmd, size *pty.Winsize) {
	var writeMutex sync.Mutex
	write := func(kind int, data []byte) error {
		writeMutex.Lock()
		defer writeMutex.Unlock()
		_ = connection.SetWriteDeadline(time.Now().Add(terminalWriteTimeout))
		return connection.WriteMessage(kind, data)
	}
	finish := func(exit Exit) { finishTerminal(connection, &writeMutex, exit) }
	terminal, err := pty.StartWithSize(command, size)
	if err != nil {
		finish(Exit{Type: "exit", Code: -1, Error: err.Error()})
		return
	}
	defer terminal.Close()
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	output := make(chan struct{})
	go func() {
		defer close(output)
		buffer := make([]byte, 32<<10)
		for {
			count, readErr := terminal.Read(buffer)
			if count > 0 && write(websocket.BinaryMessage, buffer[:count]) != nil {
				return
			}
			if readErr != nil {
				return
			}
		}
	}()
	input := make(chan struct{})
	go func() {
		defer close(input)
		for {
			kind, data, readErr := connection.ReadMessage()
			if readErr != nil {
				return
			}
			if kind == websocket.BinaryMessage {
				if _, writeErr := terminal.Write(data); writeErr != nil {
					return
				}
				continue
			}
			var control terminalControl
			if json.Unmarshal(data, &control) != nil || control.Type != "resize" || control.Cols < 2 || control.Cols > 1000 || control.Rows < 2 || control.Rows > 1000 {
				return
			}
			_ = pty.Setsize(terminal, &pty.Winsize{Cols: uint16(control.Cols), Rows: uint16(control.Rows)})
		}
	}()
	var exitErr error
	select {
	case exitErr = <-waited:
	case <-input:
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGHUP)
		select {
		case exitErr = <-waited:
		case <-time.After(terminalHangupTimeout):
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			exitErr = <-waited
		}
	}
	select {
	case <-output:
	case <-time.After(terminalDrainTimeout):
	}
	exit := Exit{Type: "exit"}
	var status *exec.ExitError
	switch {
	case errors.As(exitErr, &status):
		exit.Code = status.ExitCode()
	case exitErr != nil:
		exit.Code, exit.Error = -1, exitErr.Error()
	}
	finish(exit)
}
