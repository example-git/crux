package trafficcapture

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var captureTestCommands = map[string]func([]string) int{}

const processTestCommand = "__capture-process-test"

func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		var err error
		switch os.Args[1] {
		case "__traffic-capture-worker":
			err = RunWorker(os.Args[2])
		case "__traffic-capture-manager":
			err = RunCaptureManager(os.Args[2])
		case "__traffic-capture-pane-log":
			err = WritePaneLog(os.Args[2], os.Stdin)
		case captureProcessCommand:
			err = RunCapturedProcess(os.Args[2:])
		case "__traffic-capture-control":
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
			defer stop()
			err = RunCaptureControl(ctx, os.Args[2:], os.Stdout)
		case processTestCommand:
			os.Exit(runProcessTestTarget(os.Args[2:]))
		default:
			if command, ok := captureTestCommands[os.Args[1]]; ok {
				os.Exit(command(os.Args[2:]))
			}
			os.Exit(m.Run())
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runProcessTestTarget(arguments []string) int {
	if len(arguments) == 0 {
		return 2
	}
	switch arguments[0] {
	case "report":
		directory, _ := os.Getwd()
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"arguments": arguments[1:], "argv0": os.Args[0], "cwd": directory,
			"environment": os.Getenv("CAPTURE_TEST_EXPORT"), "proxy": os.Getenv("HTTPS_PROXY"),
		})
		fmt.Fprintln(os.Stderr, "separate stderr")
		return 0
	case "echo":
		_, _ = io.Copy(os.Stdout, os.Stdin)
		return 0
	case "exit":
		return 7
	case "child":
		command := exec.CommandContext(context.Background(), os.Args[0], processTestCommand, "sleep")
		command.Stdout = os.Stdout
		command.Stderr = os.Stderr
		if err := command.Start(); err != nil {
			return 1
		}
		fmt.Fprintln(os.Stdout, command.Process.Pid)
		return 0
	case "sleep":
		fmt.Fprintln(os.Stdout, "ready")
		time.Sleep(time.Minute)
		return 0
	}
	return 2
}

func testCaptureProcess(t *testing.T, arguments ...string) *captureProcess {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("capture requires macOS or Linux")
	}
	executable, err := os.Executable()
	require.NoError(t, err)
	directory := t.TempDir()
	environment := environmentMap(os.Environ())
	environment["CAPTURE_TEST_EXPORT"] = "value=from-shell"
	environment["HTTPS_PROXY"] = "http://127.0.0.1:12345"
	process, err := startCaptureProcess(executable, captureProcessSpec{
		Name: "main", Command: append([]string{executable, processTestCommand}, arguments...),
		Executable: executable, WorkingDir: directory, Environment: environment,
		LogPath: filepath.Join(directory, "target.log"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, process.stop(true)) })
	return process
}

func processLog(t *testing.T, process *captureProcess) string {
	t.Helper()
	data, err := os.ReadFile(process.snapshot().LogPath)
	require.NoError(t, err)
	return string(data)
}

func TestCaptureProcessPreservesArgumentsEnvironmentAndOutput(t *testing.T) {
	arguments := []string{"report", "with spaces", "", "--flag", "$(not-a-shell)", "quote'and\"double"}
	process := testCaptureProcess(t, arguments...)
	select {
	case <-process.targetDone:
	case <-time.After(5 * time.Second):
		t.Fatal("target did not exit")
	}
	require.Eventually(t, func() bool { return strings.Contains(processLog(t, process), "separate stderr") }, time.Second, 10*time.Millisecond)
	var report struct {
		Arguments   []string `json:"arguments"`
		Argv0       string   `json:"argv0"`
		WorkingDir  string   `json:"cwd"`
		Environment string   `json:"environment"`
		Proxy       string   `json:"proxy"`
	}
	require.NoError(t, json.Unmarshal([]byte(strings.Split(processLog(t, process), "\n")[0]), &report))
	require.Equal(t, arguments[1:], report.Arguments)
	require.Equal(t, process.spec.Command[0], report.Argv0)
	directory, err := filepath.EvalSymlinks(process.spec.WorkingDir)
	require.NoError(t, err)
	require.Equal(t, directory, report.WorkingDir)
	require.Equal(t, "value=from-shell", report.Environment)
	require.Equal(t, "http://127.0.0.1:12345", report.Proxy)
	require.Equal(t, 0, *process.snapshot().ExitCode)
	info, err := os.Stat(process.spec.LogPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	metadata, err := json.Marshal(process.snapshot())
	require.NoError(t, err)
	require.NotContains(t, string(metadata), "value=from-shell")
}

func TestCaptureProcessStopRestartAndSend(t *testing.T) {
	process := testCaptureProcess(t, "echo")
	require.NoError(t, process.send("private terminal input"))
	require.Eventually(t, func() bool { return strings.Contains(processLog(t, process), "private terminal input") }, time.Second, 10*time.Millisecond)
	executable, err := os.Executable()
	require.NoError(t, err)
	manager := &captureManager{executable: executable, processes: map[string]*captureProcess{"main": process}, original: process}
	oldPID := process.snapshot().PID
	result, err := manager.handle(captureControlRequest{Action: "restart", Name: "main"})
	require.NoError(t, err)
	restarted := manager.processes["main"]
	t.Cleanup(func() { require.NoError(t, restarted.stop(true)) })
	require.NotEqual(t, oldPID, result.(captureProcessStatus).PID)
	require.Equal(t, process.spec.Command, restarted.spec.Command)
	require.Equal(t, process.spec.Environment, restarted.spec.Environment)
	_, err = manager.handle(captureControlRequest{Action: "stop", Name: "main"})
	require.NoError(t, err)
	require.Equal(t, "stopped", restarted.snapshot().State)
	require.ErrorContains(t, restarted.send("no input"), "not running")
}

func TestCaptureManagerRetainsInitialExitAcrossRestart(t *testing.T) {
	process := testCaptureProcess(t, "exit")
	<-process.targetDone
	executable, err := os.Executable()
	require.NoError(t, err)
	manager := &captureManager{executable: executable, processes: map[string]*captureProcess{"main": process}, original: process}
	initial := manager.sessionStatus()
	require.Equal(t, 7, *initial.TargetExitCode)
	_, err = manager.handle(captureControlRequest{Action: "restart", Name: "main"})
	require.NoError(t, err)
	restarted := manager.processes["main"]
	t.Cleanup(func() { require.NoError(t, restarted.stop(true)) })
	require.Equal(t, initial, manager.sessionStatus())
	_, err = manager.handle(captureControlRequest{Action: "remove", Name: "main"})
	require.NoError(t, err)
	require.Equal(t, initial, manager.sessionStatus())
}

func TestCaptureManagerUsesCallingShellPathAndDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	executable, err := os.Executable()
	require.NoError(t, err)
	directory := t.TempDir()
	program := filepath.Join(directory, "from-shell-path")
	require.NoError(t, os.WriteFile(program, []byte("#!/bin/sh\nprintf '%s\\n' \"$CAPTURE_TEST_EXPORT\" \"$PWD\"\n"), 0o700))
	manager := &captureManager{executable: executable, config: workerConfig{RuntimePath: directory}, processes: make(map[string]*captureProcess)}
	result, err := manager.handle(captureControlRequest{
		Action: "start", Name: "job", Command: []string{"from-shell-path"}, WorkingDir: directory,
		Environment: map[string]string{"PATH": directory, "CAPTURE_TEST_EXPORT": "shell-export"},
	})
	require.NoError(t, err)
	process := manager.processes["job"]
	t.Cleanup(func() { require.NoError(t, process.stop(true)) })
	<-process.targetDone
	require.Eventually(t, func() bool { return strings.Contains(processLog(t, process), "shell-export") }, time.Second, 10*time.Millisecond)
	require.Equal(t, []string{"from-shell-path"}, result.(captureProcessStatus).Command)
	_, err = manager.handle(captureControlRequest{Action: "start", Name: "job", Command: []string{"ignored"}})
	require.ErrorContains(t, err, "already exists")
	_, err = manager.handle(captureControlRequest{Action: "status", Name: "missing"})
	require.ErrorContains(t, err, "unknown capture process")
}

func TestCaptureProcessExecFailureIsVisible(t *testing.T) {
	executable, err := os.Executable()
	require.NoError(t, err)
	directory := t.TempDir()
	_, err = startCaptureProcess(executable, captureProcessSpec{
		Name: "missing", Command: []string{"/no/such/capture-test-process"}, Executable: "/no/such/capture-test-process",
		WorkingDir: directory, Environment: environmentMap(os.Environ()), LogPath: filepath.Join(directory, "target.log"),
	})
	require.ErrorContains(t, err, "no such file")
}

func TestCaptureManagerFinalStatusRequiresExplicitCompletion(t *testing.T) {
	directory := t.TempDir()
	manager := &captureManager{config: workerConfig{RuntimePath: directory}}
	_, err := manager.completedSession()
	require.Error(t, err)
	path := filepath.Join(directory, "manager-completed.json")
	require.NoError(t, writePrivateJSON(path, captureSessionStatus{}))
	_, err = manager.completedSession()
	require.ErrorContains(t, err, "did not record")
	code := 7
	require.NoError(t, writePrivateJSON(path, captureSessionStatus{ShellExitCode: &code}))
	status, err := manager.completedSession()
	require.NoError(t, err)
	require.Equal(t, 7, *status.ShellExitCode)
}

func TestCaptureControlParsingPreservesExplicitArguments(t *testing.T) {
	options, err := parseCaptureControl([]string{"start", "job", "--", "/a path/tool", "", "--flag", "$(literal)"})
	require.NoError(t, err)
	require.Equal(t, []string{"/a path/tool", "", "--flag", "$(literal)"}, options.request.Command)
	options, err = parseCaptureControl([]string{"logs", "main", "-f", "-n", "0"})
	require.NoError(t, err)
	require.True(t, options.follow)
	require.Zero(t, options.lines)
	for _, arguments := range [][]string{
		{"start", "job", "--"}, {"logs", "-n", "-1"}, {"logs", "--typo"}, {"status", "a", "b"}, {"invalid"},
	} {
		_, err := parseCaptureControl(arguments)
		require.Error(t, err, arguments)
	}
}

func TestCaptureOutputFollowsUntilContextIsCanceled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "target.log")
	require.NoError(t, os.WriteFile(path, []byte("first\nsecond\nthird\n"), 0o600))
	var output bytes.Buffer
	require.NoError(t, readCaptureOutput(t.Context(), path, 2, false, &output))
	require.Equal(t, "second\nthird\n", output.String())
	ctx, cancel := context.WithCancel(t.Context())
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- readCaptureOutput(ctx, path, 0, true, writer) }()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	time.Sleep(150 * time.Millisecond)
	_, err = file.WriteString("followed\n")
	require.NoError(t, err)
	require.NoError(t, file.Close())
	data := make([]byte, len("followed\n"))
	_, err = io.ReadFull(reader, data)
	require.NoError(t, err)
	require.Equal(t, "followed\n", string(data))
	cancel()
	require.NoError(t, <-done)
	require.NoError(t, reader.Close())
	require.NoError(t, writer.Close())
}
