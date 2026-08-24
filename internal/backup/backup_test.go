package backup

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExportImportRestoresProviderPluginAndAccountData(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	setRoots(t, source)

	globalConfig := filepath.Join(source, "config", "crux.json")
	globalRC := filepath.Join(source, "config", "cruxrc")
	globalData := filepath.Join(source, "data", "crux.json")
	connections := filepath.Join(source, "data", "connections.json")
	accountStore := filepath.Join(source, "accounts", "accounts.json")
	pluginManifest := filepath.Join(source, "data", "plugins", "private.plugin", "manifest.json")
	trustStore := filepath.Join(source, "data", "plugin-state", "trust.json")
	provenanceStore := filepath.Join(source, "data", "plugin-state", "provenance.json")

	writeTestFile(t, globalConfig, `{"providers":{"custom":{"api_key":"provider-secret"}}}`)
	writeTestFile(t, globalRC, `provider add custom --type openai-compat`)
	writeTestFile(t, globalData, `{"models":{"large":{"provider":"custom","model":"model-1"}}}`)
	writeTestFile(t, connections, `{"version":1,"connections":{"remote":{"name":"remote"}}}`)
	writeTestFile(t, accountStore, `{"accounts":{"custom":[{"access_token":"account-secret"}]}}`)
	writeTestFile(t, pluginManifest, `{"schema_version":1,"id":"private.plugin"}`)
	writeTestFile(t, trustStore, `{"private.plugin":"trusted-digest"}`)
	writeTestFile(t, provenanceStore, `{"private.plugin":{"source":"local"}}`)

	archivePath := filepath.Join(t.TempDir(), "providers.crux")
	result, err := Export(archivePath, []byte("correct horse battery staple"))
	require.NoError(t, err)
	require.Equal(t, 8, result.Files)
	encrypted, err := os.ReadFile(archivePath)
	require.NoError(t, err)
	require.NotContains(t, string(encrypted), "provider-secret")
	require.NotContains(t, string(encrypted), "account-secret")
	if runtime.GOOS != "windows" {
		info, err := os.Stat(archivePath)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}

	destination := filepath.Join(t.TempDir(), "destination")
	setRoots(t, destination)
	result, err = Import(archivePath, []byte("correct horse battery staple"))
	require.NoError(t, err)
	require.Equal(t, 8, result.Files)

	requireTestFile(t, filepath.Join(destination, "config", "crux.json"), `{"providers":{"custom":{"api_key":"provider-secret"}}}`)
	requireTestFile(t, filepath.Join(destination, "config", "cruxrc"), `provider add custom --type openai-compat`)
	requireTestFile(t, filepath.Join(destination, "data", "crux.json"), `{"models":{"large":{"provider":"custom","model":"model-1"}}}`)
	requireTestFile(t, filepath.Join(destination, "data", "connections.json"), `{"version":1,"connections":{"remote":{"name":"remote"}}}`)
	requireTestFile(t, filepath.Join(destination, "accounts", "accounts.json"), `{"accounts":{"custom":[{"access_token":"account-secret"}]}}`)
	requireTestFile(t, filepath.Join(destination, "data", "plugins", "private.plugin", "manifest.json"), `{"schema_version":1,"id":"private.plugin"}`)
	requireTestFile(t, filepath.Join(destination, "data", "plugin-state", "trust.json"), `{"private.plugin":"trusted-digest"}`)
	requireTestFile(t, filepath.Join(destination, "data", "plugin-state", "provenance.json"), `{"private.plugin":{"source":"local"}}`)
}

func TestImportRejectsWrongPasswordWithoutWritingData(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	setRoots(t, source)
	writeTestFile(t, filepath.Join(source, "accounts", "accounts.json"), `{"secret":true}`)
	archivePath := filepath.Join(t.TempDir(), "providers.crux")
	_, err := Export(archivePath, []byte("correct password"))
	require.NoError(t, err)

	destination := filepath.Join(t.TempDir(), "destination")
	setRoots(t, destination)
	_, err = Import(archivePath, []byte("wrong password"))
	require.EqualError(t, err, "invalid password or damaged backup archive")
	require.NoFileExists(t, filepath.Join(destination, "accounts", "accounts.json"))
}

func TestExportRefusesToOverwriteExistingArchive(t *testing.T) {
	root := t.TempDir()
	setRoots(t, root)
	writeTestFile(t, filepath.Join(root, "accounts", "accounts.json"), `{"secret":true}`)
	archivePath := filepath.Join(t.TempDir(), "providers.crux")
	writeTestFile(t, archivePath, "existing archive")

	_, err := Export(archivePath, []byte("password"))
	require.ErrorContains(t, err, "file exists")
	requireTestFile(t, archivePath, "existing archive")
}

func TestImportRejectsInvalidAndDamagedArchives(t *testing.T) {
	root := t.TempDir()
	setRoots(t, root)
	writeTestFile(t, filepath.Join(root, "accounts", "accounts.json"), `{"secret":true}`)
	validArchive := filepath.Join(t.TempDir(), "valid.crux")
	_, err := Export(validArchive, []byte("password"))
	require.NoError(t, err)
	validData, err := os.ReadFile(validArchive)
	require.NoError(t, err)
	validData[len(validData)-1] ^= 0xff

	invalidArchive := filepath.Join(t.TempDir(), "invalid.crux")
	damagedArchive := filepath.Join(t.TempDir(), "damaged.crux")
	writeTestFile(t, invalidArchive, "not a backup")
	require.NoError(t, os.WriteFile(damagedArchive, validData, 0o600))

	destination := filepath.Join(t.TempDir(), "destination")
	setRoots(t, destination)
	_, err = Import(invalidArchive, []byte("password"))
	require.EqualError(t, err, "not a Crux backup archive")
	_, err = Import(damagedArchive, []byte("password"))
	require.EqualError(t, err, "invalid password or damaged backup archive")
	require.NoFileExists(t, filepath.Join(destination, "accounts", "accounts.json"))
}

func TestExportRefusesPluginSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires additional privileges on Windows")
	}
	root := t.TempDir()
	setRoots(t, root)
	writeTestFile(t, filepath.Join(root, "accounts", "accounts.json"), `{"secret":true}`)
	external := filepath.Join(t.TempDir(), "secret.json")
	writeTestFile(t, external, `{"outside":true}`)
	pluginDir := filepath.Join(root, "data", "plugins", "private.plugin")
	require.NoError(t, os.MkdirAll(pluginDir, 0o700))
	require.NoError(t, os.Symlink(external, filepath.Join(pluginDir, "manifest.json")))

	_, err := Export(filepath.Join(t.TempDir(), "providers.crux"), []byte("password"))
	require.ErrorContains(t, err, "plugin backup source is a symlink")
}

func TestRestoreFilesRollsBackEveryDestinationOnCommitFailure(t *testing.T) {
	root := t.TempDir()
	setRoots(t, root)
	accountPath := filepath.Join(root, "accounts", "accounts.json")
	configPath := filepath.Join(root, "config", "crux.json")
	writeTestFile(t, accountPath, "old-account")
	writeTestFile(t, configPath, "old-config")

	originalRename := renameImportFile
	calls := 0
	renameImportFile = func(oldPath, newPath string) error {
		calls++
		if calls == 4 {
			return errors.New("injected promotion failure")
		}
		return os.Rename(oldPath, newPath)
	}
	t.Cleanup(func() { renameImportFile = originalRename })

	err := restoreFiles(map[string][]byte{
		"accounts/accounts.json":  []byte("new-account"),
		"global-config/crux.json": []byte("new-config"),
	})
	require.ErrorContains(t, err, "injected promotion failure")
	requireTestFile(t, accountPath, "old-account")
	requireTestFile(t, configPath, "old-config")
}

func TestRestoreFilesRejectsSymlinkedDestinationParents(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires additional privileges on Windows")
	}
	root := t.TempDir()
	setRoots(t, root)
	external := t.TempDir()
	pluginsPath := filepath.Join(root, "data", "plugins")
	require.NoError(t, os.MkdirAll(filepath.Dir(pluginsPath), 0o700))
	require.NoError(t, os.Symlink(external, pluginsPath))

	err := restoreFiles(map[string][]byte{
		"global-data/plugins/private.plugin/manifest.json": []byte(`{"schema_version":1}`),
	})
	require.ErrorContains(t, err, "destination parent is not a real directory")
	require.NoFileExists(t, filepath.Join(external, "private.plugin", "manifest.json"))
}

func setRoots(t *testing.T, root string) {
	t.Helper()
	t.Setenv("CRUX_GLOBAL_CONFIG", filepath.Join(root, "config"))
	t.Setenv("CRUX_GLOBAL_DATA", filepath.Join(root, "data"))
	t.Setenv("CRUX_CACHE_DIR", filepath.Join(root, "cache"))
	t.Setenv("AI_CLI_DIR", filepath.Join(root, "accounts"))
}

func writeTestFile(t *testing.T, filePath, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(filePath), 0o700))
	require.NoError(t, os.WriteFile(filePath, []byte(content), 0o600))
}

func requireTestFile(t *testing.T, filePath, expected string) {
	t.Helper()
	content, err := os.ReadFile(filePath)
	require.NoError(t, err)
	require.Equal(t, expected, string(content))
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filePath)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
}
