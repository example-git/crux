package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestProviderOwnershipMigrationPreCommitFailuresPreserveConfigAndCleanArtifacts(t *testing.T) {
	tests := []struct {
		name      string
		wantError string
		inject    func(t *testing.T)
	}{
		{
			name:      "directory",
			wantError: "prepare provider ownership migration directory",
			inject: func(t *testing.T) {
				original := makeProviderMigrationDir
				makeProviderMigrationDir = func(path string, perm os.FileMode) error {
					require.NoError(t, original(path, perm))
					return errors.New("directory unavailable")
				}
				t.Cleanup(func() { makeProviderMigrationDir = original })
			},
		},
		{
			name:      "backup",
			wantError: "backup provider ownership config",
			inject: func(t *testing.T) {
				original := writeProviderMigrationBackup
				writeProviderMigrationBackup = func(path string, data []byte, perm os.FileMode) error {
					require.NoError(t, original(path, data, perm))
					return errors.New("backup unavailable")
				}
				t.Cleanup(func() { writeProviderMigrationBackup = original })
			},
		},
		{
			name:      "journal",
			wantError: "prepare provider ownership migration journal",
			inject: func(t *testing.T) {
				original := writeProviderMigrationJournal
				writeProviderMigrationJournal = func(journal providerMigrationJournal) error {
					require.NoError(t, original(journal))
					return errors.New("journal unavailable")
				}
				t.Cleanup(func() { writeProviderMigrationJournal = original })
			},
		},
		{
			name:      "commit",
			wantError: "commit provider ownership migration",
			inject: func(t *testing.T) {
				original := writeProviderMigrationConfig
				writeProviderMigrationConfig = func(path string, data []byte, perm os.FileMode) error {
					journal, _, exists, err := readProviderMigrationJournal()
					require.NoError(t, err)
					require.True(t, exists)
					require.Equal(t, "prepared", journal.State)
					require.FileExists(t, journal.Config.Backup)
					return errors.New("config unavailable")
				}
				t.Cleanup(func() { writeProviderMigrationConfig = original })
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dataDir := t.TempDir()
			t.Setenv("CRUX_GLOBAL_DATA", dataDir)
			t.Setenv("AI_CLI_DIR", t.TempDir())
			configPath := filepath.Join(dataDir, "crux.json")
			original := []byte(`{"providers":{"synthetic":{}},"format":"preserve exactly"}`)
			require.NoError(t, os.WriteFile(configPath, original, 0o600))
			test.inject(t)

			store := &ConfigStore{globalDataPath: configPath}
			err := store.migrateProviderOwnership(map[string]ProviderPluginReference{"synthetic": {ID: "synthetic.plugin", Version: "1.0.0"}})
			require.ErrorContains(t, err, test.wantError)
			current, readErr := os.ReadFile(configPath)
			require.NoError(t, readErr)
			require.Equal(t, original, current)
			_, statErr := os.Stat(providerMigrationDir())
			require.ErrorIs(t, statErr, os.ErrNotExist)
		})
	}
}

func TestProviderOwnershipMigrationCompletionFailureRemainsRecoverableWithoutPreimage(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("CRUX_GLOBAL_DATA", dataDir)
	t.Setenv("AI_CLI_DIR", t.TempDir())
	configPath := filepath.Join(dataDir, "crux.json")
	originalWriter := writeProviderMigrationJournal
	writes := 0
	writeProviderMigrationJournal = func(journal providerMigrationJournal) error {
		writes++
		if writes == 2 {
			return errors.New("completion unavailable")
		}
		return originalWriter(journal)
	}
	t.Cleanup(func() { writeProviderMigrationJournal = originalWriter })

	store := &ConfigStore{globalDataPath: configPath}
	require.NoError(t, store.migrateProviderOwnership(map[string]ProviderPluginReference{"synthetic": {ID: "synthetic.plugin", Version: "1.0.0"}}))
	current, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, "synthetic.plugin", gjson.GetBytes(current, "providers.synthetic.plugin.id").String())
	journal, err := os.ReadFile(providerMigrationJournalPath())
	require.NoError(t, err)
	require.Equal(t, "prepared", gjson.GetBytes(journal, "state").String())
	require.False(t, gjson.GetBytes(journal, "config.existed").Bool())
	require.Empty(t, gjson.GetBytes(journal, "config.backup").String())

	writeProviderMigrationJournal = originalWriter
	require.NoError(t, recoverPreparedProviderMigration(configPath))
	journal, err = os.ReadFile(providerMigrationJournalPath())
	require.NoError(t, err)
	require.Equal(t, "completed", gjson.GetBytes(journal, "state").String())
}

func TestProviderOwnershipMigrationCompletionFailureRemainsRecoverable(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("CRUX_GLOBAL_DATA", dataDir)
	t.Setenv("AI_CLI_DIR", t.TempDir())
	configPath := filepath.Join(dataDir, "crux.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{"providers":{"synthetic":{}}}`), 0o600))
	originalWriter := writeProviderMigrationJournal
	writes := 0
	writeProviderMigrationJournal = func(journal providerMigrationJournal) error {
		writes++
		if writes == 2 {
			return errors.New("completion unavailable")
		}
		return originalWriter(journal)
	}
	t.Cleanup(func() { writeProviderMigrationJournal = originalWriter })

	store := &ConfigStore{globalDataPath: configPath}
	require.NoError(t, store.migrateProviderOwnership(map[string]ProviderPluginReference{"synthetic": {ID: "synthetic.plugin", Version: "1.0.0"}}))
	current, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, "synthetic.plugin", gjson.GetBytes(current, "providers.synthetic.plugin.id").String())
	journal, err := os.ReadFile(providerMigrationJournalPath())
	require.NoError(t, err)
	require.Equal(t, "prepared", gjson.GetBytes(journal, "state").String())
	require.FileExists(t, gjson.GetBytes(journal, "config.backup").String())

	writeProviderMigrationJournal = originalWriter
	require.NoError(t, recoverPreparedProviderMigration(configPath))
	journal, err = os.ReadFile(providerMigrationJournalPath())
	require.NoError(t, err)
	require.Equal(t, "completed", gjson.GetBytes(journal, "state").String())
}

func TestProviderOwnershipMigrationCompletionErrorAcceptsPersistedCompletedRecovery(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("CRUX_GLOBAL_DATA", dataDir)
	configPath := filepath.Join(dataDir, "crux.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{"providers":{"synthetic":{}}}`), 0o600))
	originalWriter := writeProviderMigrationJournal
	writes := 0
	writeProviderMigrationJournal = func(journal providerMigrationJournal) error {
		writes++
		if writes == 2 {
			require.NoError(t, originalWriter(journal))
			return errors.New("completion acknowledgement unavailable")
		}
		return originalWriter(journal)
	}
	t.Cleanup(func() { writeProviderMigrationJournal = originalWriter })

	store := &ConfigStore{globalDataPath: configPath}
	require.NoError(t, store.migrateProviderOwnership(map[string]ProviderPluginReference{"synthetic": {ID: "synthetic.plugin", Version: "1.0.0"}}))
	journal, err := os.ReadFile(providerMigrationJournalPath())
	require.NoError(t, err)
	require.Equal(t, "completed", gjson.GetBytes(journal, "state").String())
}

func TestProviderOwnershipMigrationCompletionFailureRejectsInvalidRecovery(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("CRUX_GLOBAL_DATA", dataDir)
	t.Setenv("AI_CLI_DIR", t.TempDir())
	configPath := filepath.Join(dataDir, "crux.json")
	original := []byte(`{"providers":{"synthetic":{}},"format":"preserve exactly"}`)
	require.NoError(t, os.WriteFile(configPath, original, 0o600))
	originalWriter := writeProviderMigrationJournal
	writes := 0
	writeProviderMigrationJournal = func(journal providerMigrationJournal) error {
		writes++
		if writes == 2 {
			require.NoError(t, os.Remove(journal.Config.Backup))
			return errors.New("completion unavailable")
		}
		return originalWriter(journal)
	}
	t.Cleanup(func() { writeProviderMigrationJournal = originalWriter })

	store := &ConfigStore{globalDataPath: configPath}
	err := store.migrateProviderOwnership(map[string]ProviderPluginReference{"synthetic": {ID: "synthetic.plugin", Version: "1.0.0"}})
	require.ErrorContains(t, err, "prepared recovery is invalid")
	current, readErr := os.ReadFile(configPath)
	require.NoError(t, readErr)
	require.Equal(t, original, current)
	_, statErr := os.Stat(providerMigrationDir())
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestRecoverPreparedProviderMigrationCleansUncommittedArtifacts(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("CRUX_GLOBAL_DATA", dataDir)
	configPath := filepath.Join(dataDir, "crux.json")
	original := []byte(`{"providers":{"synthetic":{}}}`)
	require.NoError(t, os.WriteFile(configPath, original, 0o600))
	require.NoError(t, os.MkdirAll(providerMigrationDir(), 0o700))
	backupPath := filepath.Join(providerMigrationDir(), "config.interrupted.backup")
	require.NoError(t, os.WriteFile(backupPath, original, 0o600))
	journal := providerMigrationJournal{
		Version:   providerOwnershipMigrationVersion,
		State:     "prepared",
		Providers: []string{"synthetic"},
		Config: providerMigrationFile{
			Path:       configPath,
			Backup:     backupPath,
			BeforeHash: hashBytes(original),
			AfterHash:  hashBytes([]byte(`{"providers":{"synthetic":{"plugin":{"id":"synthetic.plugin","version":"1.0.0"}}}}`)),
			Existed:    true,
		},
	}
	require.NoError(t, writeMigrationJournal(journal))

	require.NoError(t, recoverPreparedProviderMigration(configPath))
	current, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, original, current)
	_, statErr := os.Stat(providerMigrationDir())
	require.ErrorIs(t, statErr, os.ErrNotExist)
}
