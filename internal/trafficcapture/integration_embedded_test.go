//go:build embedded_mitmproxy && (darwin || linux)

package trafficcapture

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const embeddedTestTargetCommand = "__traffic-capture-test-target"

func init() {
	captureTestCommands[embeddedTestTargetCommand] = runEmbeddedTestTarget
}

func TestEmbeddedCaptureEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not available")
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write([]byte("captured"))
	}))
	defer server.Close()
	workingDir := t.TempDir()
	globalData := t.TempDir()
	t.Setenv("CRUX_GLOBAL_DATA", globalData)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SHELL", "/bin/sh")
	capturePath := filepath.Join(CaptureDirectory(), "capture.mitm")
	readyPath := filepath.Join(workingDir, "target-ready")
	releasePath := filepath.Join(workingDir, "target-release")
	executable, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	metadata, err := Launch(ctx, Request{
		Executable:         executable,
		Arguments:          []string{embeddedTestTargetCommand, server.URL, readyPath, releasePath},
		WorkingDir:         workingDir,
		WorkingDirExplicit: true,
		CapturePath:        capturePath,
		ManagedCapture:     true,
	})
	if err != nil {
		runtimePaths, _ := filepath.Glob(filepath.Join(runDirectory(), "crux-capture-*"))
		for _, runtimePath := range runtimePaths {
			data, _ := os.ReadFile(filepath.Join(runtimePath, "pane.log"))
			t.Logf("startup pane log:\n%s", data)
		}
	}
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = os.WriteFile(filepath.Join(filepath.Dir(metadata.StatusPath), "stop"), nil, 0o600)
		require.Eventually(t, func() bool {
			status, ok := readWorkerStatus(metadata.StatusPath)
			return ok && (status.State == "completed" || status.State == "failed")
		}, 20*time.Second, 100*time.Millisecond)
	})
	require.Contains(t, metadata.TargetLogPath, globalData)
	require.NotEmpty(t, metadata.ControlSocket)
	require.Contains(t, metadata.CapturePath, globalData)
	require.Contains(t, metadata.StatusPath, globalData)
	require.Contains(t, metadata.PaneLogPath, globalData)
	require.NotContains(t, metadata.CapturePath, workingDir)
	require.NotContains(t, metadata.StatusPath, workingDir)
	require.NotContains(t, metadata.PaneLogPath, workingDir)
	defer func() {
		if t.Failed() {
			data, _ := os.ReadFile(metadata.PaneLogPath)
			t.Logf("pane log:\n%s", data)
		}
	}()
	require.Eventually(t, func() bool {
		_, err := os.Stat(readyPath)
		return err == nil
	}, 30*time.Second, 100*time.Millisecond)
	viewerURL, err := url.Parse(metadata.ViewerURL)
	require.NoError(t, err)
	require.NotEmpty(t, viewerURL.Query().Get("token"))
	bareViewerURL := *viewerURL
	bareViewerURL.RawQuery = ""
	statusCode, err := viewerStatus(ctx, bareViewerURL.String())
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, statusCode)
	wrongTokenURL := bareViewerURL
	wrongQuery := wrongTokenURL.Query()
	wrongQuery.Set("token", "wrong-token")
	wrongTokenURL.RawQuery = wrongQuery.Encode()
	statusCode, err = viewerStatus(ctx, wrongTokenURL.String())
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, statusCode)
	statusCode, err = viewerStatus(ctx, metadata.ViewerURL)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, statusCode)
	targetOutput, err := os.ReadFile(metadata.TargetLogPath)
	require.NoError(t, err)
	require.Contains(t, string(targetOutput), "original target stdout")
	require.Contains(t, string(targetOutput), "original target stderr")
	paneOutput, err := os.ReadFile(metadata.PaneLogPath)
	require.NoError(t, err)
	require.NotContains(t, string(paneOutput), "original target stdout")
	require.NotContains(t, string(paneOutput), "original target stderr")
	require.NoError(t, os.WriteFile(releasePath, nil, 0o600))
	status, err := waitForCompletion(ctx, "tmux", metadata.Session, metadata.StatusPath)
	require.NoError(t, err)
	require.NotNil(t, status.TargetExitCode)
	require.Equal(t, 0, *status.TargetExitCode)
	require.Equal(t, "running", status.State)
	require.NotEmpty(t, status.ViewerURL)
	verifyCaptureManagementShell(t, ctx, metadata)
	statusCode, err = viewerStatus(ctx, metadata.ViewerURL)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, statusCode)
	_, err = runTmux(ctx, "tmux", "send-keys", "-t", metadata.Session, "exit", "Enter")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		status, ok := readWorkerStatus(metadata.StatusPath)
		return ok && status.State == "completed" && status.ExitCode == 0 && status.ViewerURL == ""
	}, 20*time.Second, 100*time.Millisecond)
	info, err := os.Stat(capturePath)
	require.NoError(t, err)
	require.Positive(t, info.Size())
}

type captureTerminalOutput struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (output *captureTerminalOutput) Write(data []byte) (int, error) {
	output.mu.Lock()
	defer output.mu.Unlock()
	return output.data.Write(data)
}

func (output *captureTerminalOutput) String() string {
	output.mu.Lock()
	defer output.mu.Unlock()
	return output.data.String()
}

func attachCaptureTerminal(t *testing.T, ctx context.Context, session string) (*os.File, *captureTerminalOutput, <-chan struct{}, *error) {
	t.Helper()
	terminal, slave, err := openCaptureTerminal()
	require.NoError(t, err)
	command := exec.CommandContext(ctx, "tmux", "-L", TmuxSocket, "attach-session", "-t", session)
	environment := environmentMap(os.Environ())
	delete(environment, "TMUX")
	delete(environment, "TMUX_PANE")
	environment["TERM"] = "xterm-256color"
	command.Env = environmentList(environment)
	command.Stdin, command.Stdout, command.Stderr = slave, slave, slave
	configureCaptureProcess(command, terminal)
	require.NoError(t, command.Start())
	require.NoError(t, slave.Close())
	output := &captureTerminalOutput{}
	go func() { _, _ = io.Copy(output, terminal) }()
	done := make(chan struct{})
	var waitErr error
	go func() { waitErr = command.Wait(); close(done) }()
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			_ = command.Process.Kill()
			<-done
		}
		_ = terminal.Close()
	})
	require.Eventually(t, func() bool {
		clients, err := runTmux(ctx, "tmux", "list-clients", "-t", session, "-F", "#{client_pid}")
		return err == nil && strings.Contains(clients, fmt.Sprint(command.Process.Pid))
	}, 10*time.Second, 50*time.Millisecond)
	return terminal, output, done, &waitErr
}

func verifyCaptureManagementShell(t *testing.T, ctx context.Context, metadata Metadata) {
	t.Helper()
	terminal, output, done, waitErr := attachCaptureTerminal(t, ctx, metadata.Session)
	_, err := io.WriteString(terminal, "cap logs main -f\n")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return strings.Contains(output.String(), "original target stderr")
	}, 10*time.Second, 50*time.Millisecond)
	_, err = terminal.Write([]byte{3})
	require.NoError(t, err)
	_, err = io.WriteString(terminal, "printf '%s%s\\n' management_ shell_ready\n")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return strings.Contains(output.String(), "management_shell_ready")
	}, 10*time.Second, 50*time.Millisecond)
	_, err = io.WriteString(terminal, "export CAPTURE_TEST_EXPORT=calling-shell; cap start extra -- /bin/sh -c 'printf \"%s%s\\n\" quiet_ child_output; printf \"%s\\n\" \"$CAPTURE_TEST_EXPORT\" \"$HTTPS_PROXY\"; sleep 60'\n")
	require.NoError(t, err)
	var extra captureProcessStatus
	require.Eventually(t, func() bool {
		data, err := callCaptureControl(ctx, metadata.ControlSocket, captureControlRequest{Action: "status", Name: "extra"})
		return err == nil && json.Unmarshal(data, &extra) == nil && extra.State == "running"
	}, 10*time.Second, 50*time.Millisecond)
	require.Eventually(t, func() bool {
		data, _ := os.ReadFile(extra.LogPath)
		return strings.Contains(string(data), "quiet_child_output") && strings.Contains(string(data), "calling-shell") && !strings.Contains(string(data), "http://127.0.0.1:")
	}, 5*time.Second, 50*time.Millisecond)
	require.NotContains(t, output.String(), "quiet_child_output")
	_, err = io.WriteString(terminal, "cap logs extra\n")
	require.NoError(t, err)
	require.Eventually(t, func() bool { return strings.Contains(output.String(), "quiet_child_output") }, 5*time.Second, 50*time.Millisecond)
	_, err = io.WriteString(terminal, "cap stop extra\n")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		data, err := callCaptureControl(ctx, metadata.ControlSocket, captureControlRequest{Action: "status", Name: "extra"})
		return err == nil && json.Unmarshal(data, &extra) == nil && extra.State == "stopped"
	}, 10*time.Second, 50*time.Millisecond)
	_, err = io.WriteString(terminal, "cap restart main\n")
	require.NoError(t, err)
	var restarted captureProcessStatus
	require.Eventually(t, func() bool {
		data, err := callCaptureControl(ctx, metadata.ControlSocket, captureControlRequest{Action: "status", Name: "main"})
		return err == nil && json.Unmarshal(data, &restarted) == nil && restarted.State == "running"
	}, 10*time.Second, 10*time.Millisecond)
	_, err = terminal.Write([]byte("\x1b[24~"))
	require.NoError(t, err)
	select {
	case <-done:
		require.NoError(t, *waitErr)
	case <-time.After(10 * time.Second):
		t.Fatal("F12 did not detach the live client")
	}
	second, _, secondDone, secondErr := attachCaptureTerminal(t, ctx, metadata.Session)
	_, err = second.Write([]byte{2, 'd'})
	require.NoError(t, err)
	select {
	case <-secondDone:
		require.NoError(t, *secondErr)
	case <-time.After(10 * time.Second):
		t.Fatal("Ctrl+B then D did not detach the live client")
	}
	data, err := callCaptureControl(ctx, metadata.ControlSocket, captureControlRequest{Action: "session"})
	require.NoError(t, err)
	var session captureSessionStatus
	require.NoError(t, json.Unmarshal(data, &session))
	require.Nil(t, session.ShellExitCode)
	require.NotNil(t, session.TargetExitCode)
	require.Equal(t, 0, *session.TargetExitCode)
}

func TestEmbeddedCaptureWaitKeepsShellAvailable(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not available")
	}
	globalData := t.TempDir()
	t.Setenv("CRUX_GLOBAL_DATA", globalData)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SHELL", "/bin/sh")
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	t.Cleanup(func() {
		paths, _ := filepath.Glob(filepath.Join(runDirectory(), "crux-capture-*"))
		for _, path := range paths {
			_ = os.WriteFile(filepath.Join(path, "stop"), nil, 0o600)
		}
	})
	metadata, err := Launch(ctx, Request{
		Executable: "/bin/sh", Arguments: []string{"-c", "printf wait-target-output"},
		WorkingDir: t.TempDir(), CapturePath: filepath.Join(CaptureDirectory(), "wait.mitm"), ManagedCapture: true, Wait: true,
	})
	if err != nil {
		for _, name := range []string{"pane.log", "target.log", "status.json", "policy-status.json"} {
			paths, _ := filepath.Glob(filepath.Join(runDirectory(), "crux-capture-*", name))
			for _, path := range paths {
				data, _ := os.ReadFile(path)
				t.Logf("%s:\n%s", name, data)
				if name == "status.json" {
					var state workerStatus
					if json.Unmarshal(data, &state) == nil && state.ControlSocket != "" {
						requestContext, cancelRequest := context.WithTimeout(context.Background(), 2*time.Second)
						result, requestErr := callCaptureControl(requestContext, state.ControlSocket, captureControlRequest{Action: "list"})
						cancelRequest()
						t.Logf("processes: %s (%v)", result, requestErr)
					}
				}
			}
		}
	}
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = os.WriteFile(filepath.Join(filepath.Dir(metadata.StatusPath), "stop"), nil, 0o600)
		require.Eventually(t, func() bool {
			status, ok := readWorkerStatus(metadata.StatusPath)
			return ok && (status.State == "completed" || status.State == "failed")
		}, 20*time.Second, 100*time.Millisecond)
		status, ok := readWorkerStatus(metadata.StatusPath)
		require.True(t, ok)
		require.Equal(t, 130, status.ExitCode, status.Error)
		require.Empty(t, status.Error)
	})
	require.NotNil(t, metadata.TargetExitCode)
	require.Zero(t, *metadata.TargetExitCode)
	require.NotEmpty(t, metadata.ViewerURL)
	status, ok := readWorkerStatus(metadata.StatusPath)
	require.True(t, ok)
	require.Equal(t, "running", status.State)
	data, err := callCaptureControl(ctx, metadata.ControlSocket, captureControlRequest{Action: "session"})
	require.NoError(t, err)
	var session captureSessionStatus
	require.NoError(t, json.Unmarshal(data, &session))
	require.Nil(t, session.ShellExitCode)
}

func viewerStatus(ctx context.Context, rawURL string) (int, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, err
	}
	response, err := (&http.Client{
		Transport: &http.Transport{},
		Jar:       jar,
		Timeout:   10 * time.Second,
	}).Do(request)
	if err != nil {
		return 0, err
	}
	_, copyErr := io.Copy(io.Discard, response.Body)
	closeErr := response.Body.Close()
	if copyErr != nil {
		return 0, copyErr
	}
	if closeErr != nil {
		return 0, closeErr
	}
	return response.StatusCode, nil
}

func captureTestTransport() (*http.Transport, error) {
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	if path := os.Getenv("SSL_CERT_FILE"); path != "" {
		bundle, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(bundle) {
			return nil, fmt.Errorf("capture trust bundle contains no certificates")
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	}
	return transport, nil
}

func runEmbeddedTestTarget(arguments []string) int {
	if len(arguments) != 3 {
		return 2
	}
	if os.Getenv("HTTP_PROXY") != "" || os.Getenv("HTTPS_PROXY") != "" {
		return 3
	}
	transport, err := captureTestTransport()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 3
	}
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	response, err := client.Get(arguments[0])
	if err != nil {
		return 4
	}
	body, err := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if err != nil || closeErr != nil || response.StatusCode != http.StatusOK || string(body) != "captured" {
		return 5
	}
	tlsResponse, err := client.Get("https://mitm.it/")
	if err != nil {
		return 6
	}
	_, copyErr := io.Copy(io.Discard, tlsResponse.Body)
	closeErr = tlsResponse.Body.Close()
	if copyErr != nil || closeErr != nil || tlsResponse.StatusCode != http.StatusOK {
		return 7
	}
	fmt.Fprintln(os.Stdout, "original target stdout")
	fmt.Fprintln(os.Stderr, "original target stderr")
	if err := os.WriteFile(arguments[1], nil, 0o600); err != nil {
		return 8
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(arguments[2]); err == nil {
			return 0
		}
		time.Sleep(100 * time.Millisecond)
	}
	return 9
}
