package trafficcapture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

const captureSocketVariable = "CRUX_CAPTURE_SOCKET"

const captureProcessCommand = "__traffic-capture-process"

type processEvent struct {
	PID      int    `json:"pid,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Error    string `json:"error,omitempty"`
}

type captureProcessStatus struct {
	Name       string   `json:"name"`
	Command    []string `json:"command"`
	WorkingDir string   `json:"cwd"`
	PID        int      `json:"pid"`
	State      string   `json:"state"`
	ExitCode   *int     `json:"exit_code,omitempty"`
	LogPath    string   `json:"log_path"`
}

type captureProcessSpec struct {
	Name           string
	Command        []string
	Executable     string
	WorkingDir     string
	Environment    map[string]string
	LogPath        string
	ExecutableFile *os.File
}

type captureProcess struct {
	mu         sync.Mutex
	stopMu     sync.Mutex
	spec       captureProcessSpec
	status     captureProcessStatus
	command    *exec.Cmd
	terminal   *os.File
	statusPipe *os.File
	targetDone chan struct{}
	outputDone chan struct{}
	stopped    bool
}

func startCaptureProcess(nativeExecutable string, spec captureProcessSpec) (*captureProcess, error) {
	terminal, slave, err := openCaptureTerminal()
	if err != nil {
		return nil, fmt.Errorf("open capture process terminal: %w", err)
	}
	defer slave.Close()
	statusReader, statusWriter, err := os.Pipe()
	if err != nil {
		_ = terminal.Close()
		return nil, err
	}
	defer statusWriter.Close()
	output, err := os.OpenFile(spec.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		_ = terminal.Close()
		_ = statusReader.Close()
		return nil, err
	}
	executable := spec.Executable
	extraFiles := []*os.File{statusWriter}
	if spec.ExecutableFile != nil {
		executable = "/proc/self/fd/4"
		extraFiles = append(extraFiles, spec.ExecutableFile)
	}
	arguments := append([]string{captureProcessCommand, executable}, spec.Command...)
	command := exec.CommandContext(context.Background(), nativeExecutable, arguments...)
	command.Dir = spec.WorkingDir
	command.Env = environmentList(spec.Environment)
	command.Stdin = slave
	command.Stdout = slave
	command.Stderr = slave
	command.ExtraFiles = extraFiles
	configureCaptureProcess(command, terminal)
	if err := command.Start(); err != nil {
		_ = terminal.Close()
		_ = statusReader.Close()
		_ = output.Close()
		return nil, err
	}
	_ = slave.Close()
	_ = statusWriter.Close()
	process := &captureProcess{
		spec: spec, command: command, terminal: terminal, statusPipe: statusReader,
		targetDone: make(chan struct{}), outputDone: make(chan struct{}),
		status: captureProcessStatus{
			Name: spec.Name, Command: slices.Clone(spec.Command), WorkingDir: spec.WorkingDir,
			State: "running", LogPath: spec.LogPath,
		},
	}
	go func() {
		defer close(process.outputDone)
		defer output.Close()
		_, _ = io.Copy(output, terminal)
	}()
	decoder := json.NewDecoder(statusReader)
	var started processEvent
	if err := decoder.Decode(&started); err != nil || started.Error != "" {
		_ = signalCaptureProcess(command.Process.Pid, true)
		_ = command.Wait()
		_ = terminal.Close()
		_ = statusReader.Close()
		<-process.outputDone
		if started.Error != "" {
			return nil, errors.New(started.Error)
		}
		return nil, fmt.Errorf("start capture process supervisor: %w", err)
	}
	process.status.PID = started.PID
	go func() {
		defer close(process.targetDone)
		var event processEvent
		if decoder.Decode(&event) == nil && event.ExitCode != nil {
			process.mu.Lock()
			process.status.ExitCode = event.ExitCode
			process.status.State = "exited"
			process.mu.Unlock()
		}
	}()
	return process, nil
}

func (process *captureProcess) snapshot() captureProcessStatus {
	process.mu.Lock()
	defer process.mu.Unlock()
	return process.status
}

func (process *captureProcess) stop(force bool) error {
	process.stopMu.Lock()
	defer process.stopMu.Unlock()
	if process.stopped {
		return nil
	}
	var result error
	if !force {
		result = signalCaptureProcess(process.command.Process.Pid, false)
		select {
		case <-process.targetDone:
		case <-time.After(3 * time.Second):
		}
	}
	result = errors.Join(result, signalCaptureProcess(process.command.Process.Pid, true))
	_ = process.command.Wait()
	_ = process.statusPipe.Close()
	<-process.targetDone
	select {
	case <-process.outputDone:
	case <-time.After(time.Second):
	}
	_ = process.terminal.Close()
	<-process.outputDone
	process.mu.Lock()
	if process.status.ExitCode == nil {
		code := 137
		process.status.ExitCode = &code
	}
	process.status.State = "stopped"
	process.mu.Unlock()
	process.stopped = true
	return result
}

func (process *captureProcess) send(text string) error {
	process.stopMu.Lock()
	defer process.stopMu.Unlock()
	if process.snapshot().State != "running" {
		return fmt.Errorf("process %s is not running", process.spec.Name)
	}
	_, err := io.WriteString(process.terminal, text+"\n")
	return err
}

type captureControlRequest struct {
	Action                 string            `json:"action"`
	Name                   string            `json:"name,omitempty"`
	Command                []string          `json:"command,omitempty"`
	WorkingDir             string            `json:"cwd,omitempty"`
	Environment            map[string]string `json:"environment,omitempty"`
	Text                   string            `json:"text,omitempty"`
	WireGuardConfiguration string            `json:"wireguard_configuration,omitempty"`
}

type captureControlResponse struct {
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

type captureSessionStatus struct {
	TargetPID      int  `json:"target_pid"`
	TargetExitCode *int `json:"target_exit_code,omitempty"`
	ShellPID       int  `json:"shell_pid"`
	ShellExitCode  *int `json:"shell_exit_code,omitempty"`
}

type captureManager struct {
	mu              sync.Mutex
	config          workerConfig
	executable      string
	boundFile       *os.File
	listener        net.Listener
	socketDir       string
	processes       map[string]*captureProcess
	original        *captureProcess
	shell           *exec.Cmd
	shellDone       chan struct{}
	shellCode       *int
	nextID          int
	closed          bool
	externalAddress string
	packetBridge    *wireGuardBridge
	launcher        *exec.Cmd
	launcherDone    chan struct{}
	lastSession     captureSessionStatus
	initialized     chan struct{}
	finished        chan struct{}
	restoreTerminal func()
	serverDone      chan struct{}
	requests        sync.WaitGroup
	finishOnce      sync.Once
}

func newCaptureManager(config workerConfig, boundFile *os.File) (*captureManager, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	directory, err := os.MkdirTemp("/tmp", "crux-capture-")
	if err != nil {
		return nil, err
	}
	address := filepath.Join(directory, "control.sock")
	if config.Managed {
		address = config.ControlSocket
	}
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", address)
	if err != nil {
		_ = os.RemoveAll(directory)
		return nil, err
	}
	if err := os.Chmod(listener.Addr().String(), 0o600); err != nil {
		_ = listener.Close()
		_ = os.RemoveAll(directory)
		return nil, err
	}
	manager := &captureManager{
		config: config, executable: executable, boundFile: boundFile,
		listener: listener, socketDir: directory, processes: make(map[string]*captureProcess), initialized: make(chan struct{}), finished: make(chan struct{}), serverDone: make(chan struct{}),
	}
	go manager.serve()
	return manager, nil
}

func (manager *captureManager) serve() {
	defer close(manager.serverDone)
	for {
		connection, err := manager.listener.Accept()
		if err != nil {
			return
		}
		manager.requests.Add(1)
		go func() {
			defer manager.requests.Done()
			defer connection.Close()
			var request captureControlRequest
			response := captureControlResponse{}
			if err := json.NewDecoder(connection).Decode(&request); err != nil {
				response.Error = err.Error()
			} else {
				result, err := manager.handle(request)
				if err != nil {
					response.Error = err.Error()
				} else {
					response.Result = result
				}
			}
			_ = json.NewEncoder(connection).Encode(response)
			if request.Action == "shutdown" {
				manager.finishOnce.Do(func() { close(manager.finished) })
			}
		}()
	}
}

func (manager *captureManager) initialize(environment map[string]string) (captureSessionStatus, error) {
	if manager.original != nil {
		return captureSessionStatus{}, fmt.Errorf("capture manager is already initialized")
	}
	workingDir, err := os.Getwd()
	if err != nil {
		return captureSessionStatus{}, err
	}
	spec := captureProcessSpec{
		Name: "main", Command: manager.config.OriginalCommand, Executable: manager.config.Command[0],
		WorkingDir: workingDir, Environment: environment,
		LogPath: filepath.Join(manager.config.RuntimePath, "target.log"),
	}
	if len(manager.config.PassFDs) > 0 {
		spec.ExecutableFile = manager.boundFile
	}
	original, err := startCaptureProcess(manager.executable, spec)
	if err != nil {
		return captureSessionStatus{}, err
	}
	manager.original = original
	manager.processes["main"] = original
	binDirectory := filepath.Join(manager.config.RuntimePath, "bin")
	if err := os.Mkdir(binDirectory, 0o700); err != nil {
		return captureSessionStatus{}, err
	}
	launcher := "#!/bin/sh\nexec " + shellJoin(manager.executable, "__traffic-capture-control") + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(binDirectory, "cap"), []byte(launcher), 0o700); err != nil {
		return captureSessionStatus{}, err
	}
	shellEnvironment := make(map[string]string, len(environment)+2)
	for name, value := range environment {
		shellEnvironment[name] = value
	}
	shellEnvironment[captureSocketVariable] = manager.listener.Addr().String()
	shellEnvironment["PATH"] = binDirectory + string(os.PathListSeparator) + shellEnvironment["PATH"]
	shell := shellEnvironment["SHELL"]
	if shell == "" {
		shell = "/bin/sh"
	}
	shellPath, err := resolveManagedExecutable(shell, workingDir, shellEnvironment)
	if err != nil {
		return captureSessionStatus{}, fmt.Errorf("start management shell: %w", err)
	}
	if manager.config.Managed {
		if err := foregroundCaptureTerminal(); err != nil {
			return captureSessionStatus{}, fmt.Errorf("attach capture management terminal: %w", err)
		}
	}
	manager.shell = exec.CommandContext(context.Background(), shellPath, "-il")
	manager.shell.Env = environmentList(shellEnvironment)
	manager.shell.Dir = workingDir
	manager.shell.Stdin = os.Stdin
	manager.shell.Stdout = os.Stdout
	manager.shell.Stderr = os.Stderr
	fmt.Fprintln(os.Stdout, "Capture management shell: cap list | cap logs main -f | cap stop main | cap restart main")
	fmt.Fprintln(os.Stdout, "Start another quiet process: cap start NAME -- COMMAND [ARGS]")
	fmt.Fprintln(os.Stdout, "Detach with F12 or Ctrl+B then D. Exiting the shell ends this capture and stops its processes.")
	if err := manager.shell.Start(); err != nil {
		manager.shell = nil
		return captureSessionStatus{}, err
	}
	manager.shellDone = make(chan struct{})
	close(manager.initialized)
	go func() {
		_ = manager.shell.Wait()
		code := manager.shell.ProcessState.ExitCode()
		manager.mu.Lock()
		manager.shellCode = &code
		manager.mu.Unlock()
		close(manager.shellDone)
	}()
	return manager.sessionStatus(), nil
}

func (manager *captureManager) sessionStatus() captureSessionStatus {
	status := captureSessionStatus{ShellExitCode: manager.shellCode}
	if manager.original != nil {
		original := manager.original.snapshot()
		status.TargetPID = original.PID
		status.TargetExitCode = original.ExitCode
	}
	if manager.shell != nil && manager.shell.Process != nil {
		status.ShellPID = manager.shell.Process.Pid
	}
	return status
}

func (manager *captureManager) handle(request captureControlRequest) (any, error) {
	if request.Action == "shutdown" {
		return nil, manager.close()
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.closed {
		if request.Action == "session" && manager.config.Managed {
			return manager.sessionStatus(), nil
		}
		return nil, fmt.Errorf("capture session has closed")
	}
	if manager.externalAddress != "" {
		select {
		case <-manager.launcherDone:
			if request.Action == "session" {
				return manager.completedSession()
			}
			return nil, fmt.Errorf("capture management shell has exited")
		default:
		}
		data, err := callCaptureControl(context.Background(), manager.externalAddress, request)
		if err != nil {
			if request.Action == "session" {
				if status, statusErr := manager.completedSession(); statusErr == nil {
					return status, nil
				}
			}
			return nil, err
		}
		if request.Action == "session" {
			if err := json.Unmarshal(data, &manager.lastSession); err != nil {
				return nil, err
			}
		}
		return data, nil
	}
	switch request.Action {
	case "initialize":
		if !manager.config.Managed && request.WireGuardConfiguration != "" {
			return manager.initializeWireGuard(request.Environment, request.WireGuardConfiguration)
		}
		return manager.initialize(request.Environment)
	case "session":
		return manager.sessionStatus(), nil
	case "list":
		result := make([]captureProcessStatus, 0, len(manager.processes))
		for _, process := range manager.processes {
			result = append(result, process.snapshot())
		}
		slices.SortFunc(result, func(left, right captureProcessStatus) int { return strings.Compare(left.Name, right.Name) })
		return result, nil
	case "start":
		if request.Name == "" || len(request.Command) == 0 {
			return nil, fmt.Errorf("start requires a process name and command")
		}
		if _, exists := manager.processes[request.Name]; exists {
			return nil, fmt.Errorf("process %s already exists; use restart or remove", request.Name)
		}
		executable, err := resolveManagedExecutable(request.Command[0], request.WorkingDir, request.Environment)
		if err != nil {
			return nil, err
		}
		manager.nextID++
		process, err := startCaptureProcess(manager.executable, captureProcessSpec{
			Name: request.Name, Command: request.Command, Executable: executable,
			WorkingDir: request.WorkingDir, Environment: request.Environment,
			LogPath: filepath.Join(manager.config.RuntimePath, fmt.Sprintf("process-%d.log", manager.nextID)),
		})
		if err != nil {
			return nil, err
		}
		manager.processes[request.Name] = process
		return process.snapshot(), nil
	}
	name := request.Name
	if name == "" {
		name = "main"
	}
	process, exists := manager.processes[name]
	if !exists {
		return nil, fmt.Errorf("unknown capture process: %s", name)
	}
	switch request.Action {
	case "status", "logs":
		return process.snapshot(), nil
	case "stop", "kill":
		if err := process.stop(request.Action == "kill"); err != nil {
			return nil, err
		}
	case "restart":
		if err := process.stop(false); err != nil {
			return nil, err
		}
		restarted, err := startCaptureProcess(manager.executable, process.spec)
		if err != nil {
			return nil, err
		}
		manager.processes[name] = restarted
		return restarted.snapshot(), nil
	case "remove":
		if err := process.stop(false); err != nil {
			return nil, err
		}
		delete(manager.processes, name)
	case "send":
		if err := process.send(request.Text); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown capture action: %s", request.Action)
	}
	return process.snapshot(), nil
}

func (manager *captureManager) close() error {
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		return nil
	}
	manager.closed = true
	var result error
	if manager.launcher != nil {
		if manager.externalAddress != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, err := callCaptureControl(ctx, manager.externalAddress, captureControlRequest{Action: "shutdown"})
			cancel()
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				result = errors.Join(result, err)
			}
		}
		select {
		case <-manager.launcherDone:
		case <-time.After(10 * time.Second):
			_ = manager.launcher.Process.Kill()
			<-manager.launcherDone
			result = errors.Join(result, fmt.Errorf("capture launcher did not complete policy cleanup"))
		}
	}
	if manager.packetBridge != nil {
		result = errors.Join(result, manager.packetBridge.close())
	}
	if manager.restoreTerminal != nil {
		manager.restoreTerminal()
		manager.restoreTerminal = nil
	}
	for _, process := range manager.processes {
		result = errors.Join(result, process.stop(false))
	}
	shell := manager.shell
	shellDone := manager.shellDone
	shellRunning := shellDone != nil && manager.shellCode == nil
	manager.mu.Unlock()
	if shellRunning {
		_ = shell.Process.Signal(os.Interrupt)
		_ = shell.Process.Kill()
		<-shellDone
	}
	if manager.config.Managed {
		manager.mu.Lock()
		result = errors.Join(result, writePrivateJSON(filepath.Join(manager.config.RuntimePath, "manager-completed.json"), manager.sessionStatus()))
		manager.mu.Unlock()
	}
	result = errors.Join(result, manager.listener.Close())
	result = errors.Join(result, os.RemoveAll(manager.socketDir))
	return result
}

func (manager *captureManager) completedSession() (captureSessionStatus, error) {
	data, err := os.ReadFile(filepath.Join(manager.config.RuntimePath, "manager-completed.json"))
	if err != nil {
		return captureSessionStatus{}, err
	}
	var status captureSessionStatus
	if err := json.Unmarshal(data, &status); err != nil {
		return status, err
	}
	if status.ShellExitCode == nil {
		return status, fmt.Errorf("capture manager did not record its shell exit status")
	}
	manager.lastSession = status
	return status, nil
}

func RunCaptureManager(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var config workerConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return err
	}
	if config.DetachedTerminal {
		config.DetachedTerminal = false
		if err := writePrivateJSON(path, config); err != nil {
			return err
		}
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		command := exec.CommandContext(context.Background(), executable, "__traffic-capture-manager", path)
		command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
		configureCaptureProcess(command, os.Stdin)
		return command.Run()
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	manager, err := newCaptureManager(config, nil)
	if err != nil {
		return err
	}
	defer func() {
		_ = manager.close()
		<-manager.serverDone
		manager.requests.Wait()
	}()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	select {
	case <-manager.initialized:
	case <-manager.finished:
		return nil
	case <-signals:
		return manager.close()
	}
	select {
	case <-manager.shellDone:
	case <-manager.finished:
		return nil
	case <-signals:
	}
	return manager.close()
}

func environmentList(environment map[string]string) []string {
	result := make([]string, 0, len(environment))
	for name, value := range environment {
		result = append(result, name+"="+value)
	}
	slices.Sort(result)
	return result
}

func resolveManagedExecutable(command, workingDir string, environment map[string]string) (string, error) {
	candidates := []string{command}
	if !strings.ContainsRune(command, filepath.Separator) {
		candidates = nil
		for _, directory := range filepath.SplitList(environment["PATH"]) {
			candidates = append(candidates, filepath.Join(directory, command))
		}
	}
	for _, candidate := range candidates {
		if !filepath.IsAbs(candidate) {
			candidate = filepath.Join(workingDir, candidate)
		}
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("executable was not found or is not executable: %s", command)
}
