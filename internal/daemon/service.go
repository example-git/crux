package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/example-git/crux/internal/config"
)

const serviceStateVersion = 1

var ErrServiceNotInstalled = errors.New("Crux server service is not installed")

type Metadata struct {
	Version        int      `json:"version"`
	Manager        Manager  `json:"manager"`
	Path           string   `json:"path"`
	Host           string   `json:"host,omitempty"`
	WorkspaceRoots []string `json:"workspace_roots,omitempty"`
	LogPath        string   `json:"log_path,omitempty"`
}

type ServiceStatus struct {
	Metadata Metadata
	Active   bool
	Detail   string
}

var safeLogName = regexp.MustCompile(`[^a-zA-Z0-9._-]`)

func Status(ctx context.Context) (ServiceStatus, error) {
	metadata, err := loadServiceMetadata()
	if err != nil {
		return ServiceStatus{}, err
	}
	active, detail := serviceActive(ctx, metadata)
	return ServiceStatus{Metadata: metadata, Active: active, Detail: detail}, nil
}

func Start(ctx context.Context) error {
	metadata, err := loadServiceMetadata()
	if err != nil {
		return err
	}
	return serviceCommand(ctx, metadata, "start")
}

func Stop(ctx context.Context) error {
	metadata, err := loadServiceMetadata()
	if err != nil {
		return err
	}
	return serviceCommand(ctx, metadata, "stop")
}

func Restart(ctx context.Context) error {
	metadata, err := loadServiceMetadata()
	if err != nil {
		return err
	}
	return serviceCommand(ctx, metadata, "restart")
}

func Uninstall(ctx context.Context) error {
	metadata, err := loadServiceMetadata()
	if err != nil {
		return err
	}
	if err := validateManagedServicePath(metadata); err != nil {
		return err
	}
	if err := uninstallService(ctx, metadata); err != nil {
		return err
	}
	if err := os.Remove(serviceStatePath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove service metadata: %w", err)
	}
	return nil
}

func ReadLogs(lines int) (string, error) {
	if lines < 1 || lines > 10000 {
		return "", errors.New("log line count must be between 1 and 10000")
	}
	metadata, err := loadServiceMetadata()
	if err != nil {
		return "", err
	}
	if metadata.LogPath == "" {
		return "", errors.New("the installed service has no recorded log path; reinstall it to enable log discovery")
	}
	file, err := os.Open(metadata.LogPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("server log is not available yet: %s", metadata.LogPath)
		}
		return "", fmt.Errorf("open server log: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	const maxLogRead = int64(4 << 20)
	start := max(int64(0), info.Size()-maxLogRead)
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return "", err
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	selected := make([]string, 0, lines)
	for scanner.Scan() {
		if len(selected) == lines {
			copy(selected, selected[1:])
			selected[len(selected)-1] = scanner.Text()
		} else {
			selected = append(selected, scanner.Text())
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read server log: %w", err)
	}
	return strings.Join(selected, "\n"), nil
}

func saveServiceMetadata(result Result, host string, workspaceRoots []string) error {
	metadata := Metadata{
		Version:        serviceStateVersion,
		Manager:        result.Manager,
		Path:           result.Path,
		Host:           host,
		WorkspaceRoots: append([]string(nil), workspaceRoots...),
		LogPath:        serviceLogPath(host),
	}
	if err := validateManagedServicePath(metadata); err != nil {
		return err
	}
	content, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	content = append(content, '\n')
	return writePrivateFile(serviceStatePath(), content)
}

func loadServiceMetadata() (Metadata, error) {
	content, err := os.ReadFile(serviceStatePath())
	if errors.Is(err, os.ErrNotExist) {
		return discoverServiceMetadata()
	}
	if err != nil {
		return Metadata{}, fmt.Errorf("read service metadata: %w", err)
	}
	if len(content) > 64<<10 {
		return Metadata{}, errors.New("service metadata is too large")
	}
	decoder := json.NewDecoder(strings.NewReader(string(content)))
	decoder.DisallowUnknownFields()
	var metadata Metadata
	if err := decoder.Decode(&metadata); err != nil {
		return Metadata{}, fmt.Errorf("decode service metadata: %w", err)
	}
	if metadata.Version != serviceStateVersion {
		return Metadata{}, fmt.Errorf("unsupported service metadata version: %d", metadata.Version)
	}
	if err := validateManagedServicePath(metadata); err != nil {
		return Metadata{}, err
	}
	return metadata, nil
}

func discoverServiceMetadata() (Metadata, error) {
	paths, err := managedServicePaths()
	if err != nil {
		return Metadata{}, err
	}
	var found []Metadata
	for manager, path := range paths {
		if _, err := os.Lstat(path); err == nil {
			found = append(found, Metadata{Version: serviceStateVersion, Manager: manager, Path: path})
		} else if !errors.Is(err, os.ErrNotExist) {
			return Metadata{}, fmt.Errorf("inspect %s service: %w", manager, err)
		}
	}
	if len(found) == 0 {
		return Metadata{}, ErrServiceNotInstalled
	}
	if len(found) > 1 {
		return Metadata{}, errors.New("multiple Crux server service installations were found; remove stale installations before continuing")
	}
	return found[0], nil
}

func validateManagedServicePath(metadata Metadata) error {
	paths, err := managedServicePaths()
	if err != nil {
		return err
	}
	expected, ok := paths[metadata.Manager]
	if !ok {
		return fmt.Errorf("unsupported service manager in metadata: %s", metadata.Manager)
	}
	if filepath.Clean(metadata.Path) != filepath.Clean(expected) {
		return fmt.Errorf("refusing to manage unexpected %s service path: %s", metadata.Manager, metadata.Path)
	}
	return nil
}

func managedServicePaths() (map[Manager]string, error) {
	configHome, err := userConfigHome()
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory: %w", err)
	}
	return map[Manager]string{
		ManagerSystemd: filepath.Join(configHome, "systemd", "user", "crux-server.service"),
		ManagerOpenRC:  filepath.Join(configHome, "rc", "init.d", "crux-server"),
		ManagerRunit:   filepath.Join(home, "service", "crux-server", "run"),
	}, nil
}

func serviceActive(ctx context.Context, metadata Metadata) (bool, string) {
	var err error
	switch metadata.Manager {
	case ManagerSystemd:
		err = runManagerCommand(ctx, "systemctl", "--user", "is-active", "crux-server.service")
	case ManagerOpenRC:
		err = runManagerCommand(ctx, "rc-service", "--user", "crux-server", "status")
	case ManagerRunit:
		err = runManagerCommand(ctx, "sv", "status", filepath.Dir(metadata.Path))
	default:
		return false, "unsupported service manager"
	}
	if err != nil {
		return false, err.Error()
	}
	return true, "active"
}

func serviceCommand(ctx context.Context, metadata Metadata, operation string) error {
	if err := validateManagedServicePath(metadata); err != nil {
		return err
	}
	switch metadata.Manager {
	case ManagerSystemd:
		return runManagerCommand(ctx, "systemctl", "--user", operation, "crux-server.service")
	case ManagerOpenRC:
		return runManagerCommand(ctx, "rc-service", "--user", "crux-server", operation)
	case ManagerRunit:
		runitOperation := map[string]string{"start": "up", "stop": "down", "restart": "restart"}[operation]
		if runitOperation == "" {
			return fmt.Errorf("unsupported runit operation: %s", operation)
		}
		return runManagerCommand(ctx, "sv", runitOperation, filepath.Dir(metadata.Path))
	default:
		return fmt.Errorf("unsupported service manager: %s", metadata.Manager)
	}
}

func uninstallService(ctx context.Context, metadata Metadata) error {
	switch metadata.Manager {
	case ManagerSystemd:
		if err := runManagerCommand(ctx, "systemctl", "--user", "disable", "--now", "crux-server.service"); err != nil {
			return err
		}
		if err := os.Remove(metadata.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return runManagerCommand(ctx, "systemctl", "--user", "daemon-reload")
	case ManagerOpenRC:
		if err := runManagerCommand(ctx, "rc-service", "--user", "crux-server", "stop"); err != nil {
			return err
		}
		if err := runManagerCommand(ctx, "rc-update", "--user", "del", "crux-server", "default"); err != nil {
			return err
		}
		return os.Remove(metadata.Path)
	case ManagerRunit:
		serviceDirectory := filepath.Dir(metadata.Path)
		if err := runManagerCommand(ctx, "sv", "down", serviceDirectory); err != nil {
			return err
		}
		return os.RemoveAll(serviceDirectory)
	default:
		return fmt.Errorf("unsupported service manager: %s", metadata.Manager)
	}
}

func writePrivateFile(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".crux-service-state-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func serviceLogPath(host string) string {
	name := safeLogName.ReplaceAllString(host, "_")
	return filepath.Join(config.GlobalCacheDir(), "server-"+name, "crux.log")
}

func serviceStatePath() string {
	return filepath.Join(config.GlobalWorkspaceDir(), "server-service.json")
}
