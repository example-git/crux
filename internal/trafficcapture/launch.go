package trafficcapture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const workerStartupTimeout = 60 * time.Second

func Launch(ctx context.Context, request Request) (Metadata, error) {
	if !EmbeddedRuntimeAvailable() {
		return Metadata{}, embeddedRuntimeUnavailableError()
	}
	target, err := resolveTarget(ctx, request)
	if err != nil {
		return Metadata{}, err
	}
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		return Metadata{}, errors.New("tmux is required but is not available in PATH")
	}
	if request.ManagedCapture {
		if err := ensurePrivateDirectory(storageDirectory()); err != nil {
			return Metadata{}, err
		}
		if err := ensurePrivateDirectory(filepath.Dir(request.CapturePath)); err != nil {
			return Metadata{}, err
		}
	} else if err := os.MkdirAll(filepath.Dir(request.CapturePath), 0o700); err != nil {
		return Metadata{}, fmt.Errorf("create capture directory: %w", err)
	}
	timestamp := time.Now().UTC().Format("20060102T150405Z")
	session := fmt.Sprintf("crux-capture-%s-%d", timestamp, os.Getpid())
	exists, err := tmuxSessionExists(ctx, tmux, session)
	if err != nil {
		return Metadata{}, err
	}
	if exists {
		return Metadata{}, fmt.Errorf("tmux session already exists: %s", session)
	}
	runtimeRoot := runDirectory()
	if err := ensurePrivateDirectory(storageDirectory()); err != nil {
		return Metadata{}, err
	}
	if err := ensurePrivateDirectory(runtimeRoot); err != nil {
		return Metadata{}, err
	}
	runtimePath := filepath.Join(runtimeRoot, session)
	if err := os.Mkdir(runtimePath, 0o700); err != nil {
		return Metadata{}, fmt.Errorf("create capture runtime directory: %w", err)
	}
	statusPath := filepath.Join(runtimePath, "status.json")
	configPath := filepath.Join(runtimePath, "worker.json")
	readyPath := filepath.Join(runtimePath, "pane-ready")
	stopPath := filepath.Join(runtimePath, "stop")
	paneLogPath := filepath.Join(runtimePath, "pane.log")
	proxyPort, err := chooseLoopbackPort(ctx)
	if err != nil {
		return Metadata{}, err
	}
	viewerPort, err := chooseLoopbackPort(ctx)
	if err != nil {
		return Metadata{}, err
	}
	for viewerPort == proxyPort {
		viewerPort, err = chooseLoopbackPort(ctx)
		if err != nil {
			return Metadata{}, err
		}
	}
	config := workerConfig{
		Command:     target.Command,
		Environment: target.Environment,
		WorkingDir:  target.WorkingDir,
		Output:      request.CapturePath,
		Host:        "127.0.0.1",
		Port:        proxyPort,
		ViewerPort:  viewerPort,
		UnsetEnv:    append([]string{}, request.UnsetEnv...),
		RuntimePath: runtimePath,
		StatusPath:  statusPath,
		ReadyPath:   readyPath,
		StopPath:    stopPath,
		PaneLogPath: paneLogPath,
		Session:     session,
	}
	if err := writePrivateJSON(configPath, config); err != nil {
		return Metadata{}, err
	}
	executable, err := os.Executable()
	if err != nil {
		return Metadata{}, fmt.Errorf("locate Crux executable: %w", err)
	}
	workerCommand := shellJoin(executable, "__traffic-capture-worker", configPath)
	if err := startTmuxCapture(ctx, tmux, session, target.WorkingDir, workerCommand); err != nil {
		return Metadata{}, err
	}
	launched := false
	defer func() {
		if !launched {
			killTmuxSession(tmux, session)
		}
	}()
	if _, err := runTmux(ctx, tmux, "set-option", "-t", session, "remain-on-exit", "off"); err != nil {
		return Metadata{}, fmt.Errorf("disable tmux remain-on-exit: %w", err)
	}
	paneCommand := shellJoin(executable, "__traffic-capture-pane-log", paneLogPath)
	if _, err := runTmux(ctx, tmux, "pipe-pane", "-o", "-t", session+":0.0", paneCommand); err != nil {
		return Metadata{}, fmt.Errorf("activate tmux capture output: %w", err)
	}
	if err := os.WriteFile(readyPath, nil, 0o600); err != nil {
		return Metadata{}, fmt.Errorf("signal capture worker readiness: %w", err)
	}
	status, err := waitForWorkerStart(ctx, tmux, session, statusPath)
	if err != nil {
		return Metadata{}, err
	}
	metadata := Metadata{
		Session:     session,
		CapturePath: request.CapturePath,
		StatusPath:  statusPath,
		PaneLogPath: paneLogPath,
		Attach:      fmt.Sprintf("tmux -L %s attach -t %s", TmuxSocket, session),
		ViewerURL:   status.ViewerURL,
	}
	if status.State == "failed" {
		return Metadata{}, workerFailure(status)
	}
	launched = true
	if request.Wait {
		status, err = waitForCompletion(ctx, tmux, session, statusPath)
		if err != nil {
			return Metadata{}, err
		}
		if status.State == "failed" {
			return Metadata{}, workerFailure(status)
		}
		metadata.ViewerURL = status.ViewerURL
	}
	return metadata, nil
}

func chooseLoopbackPort(ctx context.Context) (int, error) {
	var config net.ListenConfig
	listener, err := config.Listen(ctx, "tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("select loopback port: %w", err)
	}
	defer listener.Close()
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0, errors.New("selected loopback listener has an unexpected address")
	}
	return address.Port, nil
}

func writePrivateJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode traffic capture worker config: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".worker-*")
	if err != nil {
		return fmt.Errorf("create traffic capture worker config: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("secure traffic capture worker config: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write traffic capture worker config: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close traffic capture worker config: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("install traffic capture worker config: %w", err)
	}
	return nil
}

func startTmuxCapture(ctx context.Context, tmux, session, workingDir, workerCommand string) error {
	output, err := runTmux(
		ctx,
		tmux,
		"new-session",
		"-d",
		"-s",
		session,
		"-n",
		"capture",
		"-c",
		workingDir,
		workerCommand,
	)
	if err != nil {
		return fmt.Errorf("create tmux capture session %s: %w: %s", session, err, output)
	}
	return nil
}

func runTmux(ctx context.Context, tmux string, arguments ...string) (string, error) {
	command := exec.CommandContext(ctx, tmux, append([]string{"-L", TmuxSocket}, arguments...)...)
	output, err := command.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

func tmuxSessionExists(ctx context.Context, tmux, session string) (bool, error) {
	output, err := runTmux(ctx, tmux, "has-session", "-t", session)
	if err == nil {
		return true, nil
	}
	lowered := strings.ToLower(output)
	if strings.Contains(lowered, "no server running") || strings.Contains(lowered, "failed to connect") || strings.Contains(lowered, "can't find session") {
		return false, nil
	}
	return false, fmt.Errorf("inspect tmux capture session: %w: %s", err, output)
}

func killTmuxSession(tmux, session string) {
	command := exec.CommandContext(context.Background(), tmux, "-L", TmuxSocket, "kill-session", "-t", session)
	command.Stdout = nil
	command.Stderr = nil
	_ = command.Run()
}

func waitForWorkerStart(ctx context.Context, tmux, session, statusPath string) (workerStatus, error) {
	deadline := time.NewTimer(workerStartupTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return workerStatus{}, ctx.Err()
		case <-deadline.C:
			return workerStatus{}, fmt.Errorf("capture worker did not start in tmux session %s within %s", session, workerStartupTimeout)
		case <-ticker.C:
			status, ok := readWorkerStatus(statusPath)
			if ok && (status.State == "running" || status.State == "completed" || status.State == "failed") {
				return status, nil
			}
			exists, err := tmuxSessionExists(ctx, tmux, session)
			if err != nil {
				return workerStatus{}, err
			}
			if !exists {
				return workerStatus{}, fmt.Errorf("tmux session %s exited before capture startup", session)
			}
		}
	}
}

func waitForCompletion(ctx context.Context, tmux, session, statusPath string) (workerStatus, error) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return workerStatus{}, ctx.Err()
		case <-ticker.C:
			status, ok := readWorkerStatus(statusPath)
			if ok && (status.State == "completed" || status.State == "failed") {
				return status, nil
			}
			exists, err := tmuxSessionExists(ctx, tmux, session)
			if err != nil {
				return workerStatus{}, err
			}
			if !exists {
				return workerStatus{}, fmt.Errorf("tmux session %s disappeared before writing completion status", session)
			}
		}
	}
}

func readWorkerStatus(path string) (workerStatus, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return workerStatus{}, false
	}
	var status workerStatus
	if json.Unmarshal(data, &status) != nil {
		return workerStatus{}, false
	}
	return status, true
}

func workerFailure(status workerStatus) error {
	if status.Error != "" {
		return fmt.Errorf("traffic capture worker failed: %s", status.Error)
	}
	return fmt.Errorf("traffic capture worker failed with exit code %d", status.ExitCode)
}

func shellJoin(arguments ...string) string {
	quoted := make([]string, len(arguments))
	for index, argument := range arguments {
		quoted[index] = "'" + strings.ReplaceAll(argument, "'", "'\"'\"'") + "'"
	}
	return strings.Join(quoted, " ")
}
