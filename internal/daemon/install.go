package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

type Manager string

const (
	ManagerAuto    Manager = "auto"
	ManagerSystemd Manager = "systemd"
	ManagerOpenRC  Manager = "openrc"
	ManagerRunit   Manager = "runit"
)

type Result struct {
	Manager Manager
	Path    string
}

var runManagerCommand = run

func Install(ctx context.Context, manager Manager, executable, host string, workspaceRoots []string) (Result, error) {
	if runtime.GOOS != "linux" {
		return Result{}, errors.New("server daemon installation is only supported on Linux")
	}
	if _, err := loadServiceMetadata(); err == nil {
		return Result{}, errors.New("Crux server service is already installed")
	} else if !errors.Is(err, ErrServiceNotInstalled) {
		return Result{}, err
	}
	resolvedManager, err := detectManager(manager)
	if err != nil {
		return Result{}, err
	}
	var result Result
	switch resolvedManager {
	case ManagerSystemd:
		result, err = installSystemd(ctx, executable, host, workspaceRoots)
	case ManagerOpenRC:
		result, err = installOpenRC(ctx, executable, host, workspaceRoots)
	case ManagerRunit:
		result, err = installRunit(ctx, executable, host, workspaceRoots)
	default:
		return Result{}, fmt.Errorf("unsupported service manager: %s", resolvedManager)
	}
	if err != nil {
		return Result{}, err
	}
	if err := saveServiceMetadata(result, host, workspaceRoots); err != nil {
		return Result{}, fmt.Errorf("save service metadata: %w", err)
	}
	return result, nil
}

func detectManager(requested Manager) (Manager, error) {
	if requested != ManagerAuto {
		if !slicesContains([]Manager{ManagerSystemd, ManagerOpenRC, ManagerRunit}, requested) {
			return "", fmt.Errorf("unsupported service manager: %s", requested)
		}
		return requested, nil
	}
	if _, err := os.Stat("/run/systemd/system"); err == nil && commandExists("systemctl") {
		return ManagerSystemd, nil
	}
	if commandExists("rc-service") && commandExists("rc-update") {
		return ManagerOpenRC, nil
	}
	if commandExists("runsvdir") && commandExists("sv") {
		return ManagerRunit, nil
	}
	return "", errors.New("no supported service manager found; expected systemd, OpenRC, or runit")
}

func installSystemd(ctx context.Context, executable, host string, workspaceRoots []string) (Result, error) {
	configHome, err := userConfigHome()
	if err != nil {
		return Result{}, err
	}
	servicePath := filepath.Join(configHome, "systemd", "user", "crux-server.service")
	arguments := serverArguments(host, workspaceRoots)
	quoted := make([]string, 0, len(arguments)+1)
	quoted = append(quoted, systemdQuote(executable))
	for _, argument := range arguments {
		quoted = append(quoted, systemdQuote(argument))
	}
	content := "[Unit]\nDescription=Crux authenticated server\nAfter=network-online.target\n\n[Service]\nExecStart=" + strings.Join(quoted, " ") + "\nRestart=on-failure\nRestartSec=2\n\n[Install]\nWantedBy=default.target\n"
	if err := writeFile(servicePath, []byte(content), 0o600); err != nil {
		return Result{}, err
	}
	if err := runManagerCommand(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return Result{}, err
	}
	if err := runManagerCommand(ctx, "systemctl", "--user", "enable", "--now", "crux-server.service"); err != nil {
		return Result{}, err
	}
	return Result{Manager: ManagerSystemd, Path: servicePath}, nil
}

func installOpenRC(ctx context.Context, executable, host string, workspaceRoots []string) (Result, error) {
	if os.Getenv("XDG_RUNTIME_DIR") == "" {
		return Result{}, errors.New("XDG_RUNTIME_DIR must be set for OpenRC user services")
	}
	configHome, err := userConfigHome()
	if err != nil {
		return Result{}, err
	}
	servicePath := filepath.Join(configHome, "rc", "init.d", "crux-server")
	content := "#!/sbin/openrc-run\nname=\"Crux authenticated server\"\nsupervisor=supervise-daemon\ncommand=" + shellQuote(executable) + "\ncommand_args=" + shellQuote(shellJoin(serverArguments(host, workspaceRoots))) + "\ndepend() {\n  need net\n}\n"
	if err := writeFile(servicePath, []byte(content), 0o700); err != nil {
		return Result{}, err
	}
	if err := runManagerCommand(ctx, "rc-update", "--user", "add", "crux-server", "default"); err != nil {
		return Result{}, err
	}
	if err := runManagerCommand(ctx, "rc-service", "--user", "crux-server", "start"); err != nil {
		return Result{}, err
	}
	return Result{Manager: ManagerOpenRC, Path: servicePath}, nil
}

func installRunit(ctx context.Context, executable, host string, workspaceRoots []string) (Result, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Result{}, fmt.Errorf("resolve home directory: %w", err)
	}
	servicePath := filepath.Join(home, "service", "crux-server", "run")
	content := "#!/bin/sh\nexec 2>&1\nexec " + shellQuote(executable) + " " + shellJoin(serverArguments(host, workspaceRoots)) + "\n"
	if err := writeFile(servicePath, []byte(content), 0o700); err != nil {
		return Result{}, err
	}
	if err := runManagerCommand(ctx, "sv", "up", filepath.Dir(servicePath)); err != nil {
		return Result{}, fmt.Errorf("start runit service; ensure runsvdir supervises ~/service: %w", err)
	}
	return Result{Manager: ManagerRunit, Path: servicePath}, nil
}

func writeFile(filePath string, content []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(filePath), 0o700); err != nil {
		return fmt.Errorf("create service directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(filePath), ".crux-service-*")
	if err != nil {
		return fmt.Errorf("create temporary service file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Remove(filePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(temporaryPath, filePath); err != nil {
		return fmt.Errorf("install service file: %w", err)
	}
	return nil
}

func run(ctx context.Context, name string, args ...string) error {
	command := exec.CommandContext(ctx, name, args...)
	output, err := command.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message != "" {
			return fmt.Errorf("%s failed: %s: %w", name, message, err)
		}
		return fmt.Errorf("%s failed: %w", name, err)
	}
	return nil
}

func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func userConfigHome() (string, error) {
	if configured := os.Getenv("XDG_CONFIG_HOME"); configured != "" {
		return configured, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".config"), nil
}

func serverArguments(host string, workspaceRoots []string) []string {
	arguments := []string{"server", "--host", host}
	for _, root := range workspaceRoots {
		arguments = append(arguments, "--workspace-root", root)
	}
	return arguments
}

func systemdQuote(value string) string {
	return strconv.Quote(strings.ReplaceAll(value, "%", "%%"))
}

func shellJoin(arguments []string) string {
	quoted := make([]string, len(arguments))
	for index, argument := range arguments {
		quoted[index] = shellQuote(argument)
	}
	return strings.Join(quoted, " ")
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func slicesContains(values []Manager, target Manager) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
