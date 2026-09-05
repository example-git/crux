package localaddon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/example-git/crux/internal/proto"
)

var copilotBindingsMu sync.Mutex

type copilotBindings map[string]string

func copilotBindingsPath(workspace *proto.Workspace) string {
	dataDir := workspace.DataDir
	if dataDir == "" {
		dataDir = filepath.Join(workspace.Path, ".crux")
	} else if !filepath.IsAbs(dataDir) {
		dataDir = filepath.Join(workspace.Path, dataDir)
	}
	return filepath.Join(filepath.Clean(dataDir), "compatibility", "copilot-sessions.json")
}

func loadCopilotBindings(workspace *proto.Workspace) (copilotBindings, error) {
	copilotBindingsMu.Lock()
	defer copilotBindingsMu.Unlock()
	return loadCopilotBindingsLocked(workspace)
}

func loadCopilotBindingsLocked(workspace *proto.Workspace) (copilotBindings, error) {
	bindings := make(copilotBindings)
	data, err := os.ReadFile(copilotBindingsPath(workspace))
	if os.IsNotExist(err) {
		return bindings, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read Copilot session bindings: %w", err)
	}
	if err := json.Unmarshal(data, &bindings); err != nil {
		return nil, fmt.Errorf("decode Copilot session bindings: %w", err)
	}
	return bindings, nil
}

func saveCopilotBinding(workspace *proto.Workspace, externalID, nativeID string) error {
	copilotBindingsMu.Lock()
	defer copilotBindingsMu.Unlock()
	bindings, err := loadCopilotBindingsLocked(workspace)
	if err != nil {
		return err
	}
	bindings[externalID] = nativeID
	return writeCopilotBindingsLocked(workspace, bindings)
}

func deleteCopilotBinding(workspace *proto.Workspace, externalID string) error {
	copilotBindingsMu.Lock()
	defer copilotBindingsMu.Unlock()
	bindings, err := loadCopilotBindingsLocked(workspace)
	if err != nil {
		return err
	}
	delete(bindings, externalID)
	return writeCopilotBindingsLocked(workspace, bindings)
}

func writeCopilotBindingsLocked(workspace *proto.Workspace, bindings copilotBindings) error {
	path := copilotBindingsPath(workspace)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create Copilot binding directory: %w", err)
	}
	data, err := json.MarshalIndent(bindings, "", "  ")
	if err != nil {
		return fmt.Errorf("encode Copilot session bindings: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".copilot-sessions-*")
	if err != nil {
		return fmt.Errorf("create Copilot binding temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write Copilot session bindings: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync Copilot session bindings: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close Copilot session bindings: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace Copilot session bindings: %w", err)
	}
	return nil
}
