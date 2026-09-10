package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/filepathext"
	"github.com/example-git/crux/internal/fsext"
	"github.com/example-git/crux/internal/lock"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// UpdateInstalledProviderReferences is used only by an explicit plugin update.
// It repairs same-plugin references even when an earlier install already
// replaced the bundle but left configuration on an older version. Ordinary
// loading still requires exact ownership; it never opts into this operation.
// Config and credential expressions are not evaluated by this writer.
func UpdateInstalledProviderReferences(ctx context.Context, cwd, dataDir string, installed providerplugin.InstalledBundle) error {
	if installed.PluginType != manifest.PluginTypeProvider && installed.PluginType != manifest.PluginTypeProviderPreset {
		return nil
	}
	if installed.ID == "" || installed.ProviderID == "" || installed.Version == "" || installed.Digest == "" {
		return errors.New("installed provider identity is incomplete")
	}
	paths, err := providerUpdateConfigPaths(cwd, dataDir, env.New())
	if err != nil {
		return err
	}
	for _, path := range paths {
		if err := updateProviderReferenceFile(ctx, path, installed); err != nil {
			return fmt.Errorf("update provider reference in %s: %w", path, err)
		}
	}
	return nil
}

func providerUpdateConfigPaths(cwd, dataDir string, environment env.Env) ([]string, error) {
	var paths []string
	configuredDataDir := ""
	for _, path := range lookupConfigsFromEnvironment(cwd, environment) {
		// System configuration and executable shell config are not writable
		// plugin state. Do not run a shell just to repair a saved reference.
		if path == systemConfigPath || isShellConfig(path) {
			continue
		}
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !json.Valid(data) {
			return nil, fmt.Errorf("invalid JSON in config file %s", path)
		}
		paths = append(paths, filepath.Clean(path))
		if value := gjson.GetBytes(data, "options.data_directory"); value.Type == gjson.String && value.String() != "" {
			configuredDataDir = value.String()
		}
	}
	if dataDir == "" {
		dataDir = configuredDataDir
	}
	if dataDir == "" {
		if closest, found := fsext.LookupClosestBounded(cwd, projectBoundary(cwd), defaultDataDirectory); found {
			dataDir = closest
		} else {
			dataDir = defaultDataDirectory
		}
	}
	paths = append(paths, filepath.Join(filepathext.SmartJoin(cwd, dataDir), appName+".json"))
	slices.Sort(paths)
	return slices.Compact(paths), nil
}

func updateProviderReferenceFile(ctx context.Context, path string, installed providerplugin.InstalledBundle) error {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, configLockDeadline)
	defer cancel()
	unlock, err := lock.File(ctx, path+".lock")
	if err != nil {
		return err
	}
	defer unlock()
	// Finish a pending startup migration before editing its source file.
	if err := recoverPreparedProviderMigration(path); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !json.Valid(data) {
		return errors.New("invalid JSON")
	}
	providerPath := "providers." + strings.NewReplacer(`\`, `\\`, ".", `\.`).Replace(installed.ProviderID)
	provider := gjson.GetBytes(data, providerPath)
	kind := "plugin"
	if installed.PluginType == manifest.PluginTypeProviderPreset {
		if provider.Get("plugin").Exists() {
			return nil
		}
		kind = "preset"
	}
	if provider.Get(kind+".id").String() != installed.ID {
		return nil
	}
	updated, err := sjson.SetBytes(data, providerPath+"."+kind+".version", installed.Version)
	if err != nil {
		return err
	}
	if kind == "preset" {
		updated, err = sjson.SetBytes(updated, providerPath+".preset.digest", installed.Digest)
		if err != nil {
			return err
		}
	}
	if bytes.Equal(data, updated) {
		return nil
	}
	return atomicWriteFile(path, updated, 0o600)
}
