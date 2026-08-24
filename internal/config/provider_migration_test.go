package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestProviderOwnershipMigrationBacksUpAndRollsBack(t *testing.T) {
	dataDir := t.TempDir()
	accountDir := t.TempDir()
	t.Setenv("CRUX_GLOBAL_DATA", dataDir)
	t.Setenv("AI_CLI_DIR", accountDir)

	configPath := filepath.Join(dataDir, "crux.json")
	accountPath := filepath.Join(accountDir, "accounts.json")
	originalConfig := []byte(`{"providers":{"synthetic":{"api_key":"secret"}},"models":{"large":{"provider":"synthetic","model":"model-1"}}}`)
	originalAccounts := []byte(`{"synthetic":{"active":"default","accounts":{"default":{"id":"default","access":"secret"}}}}`)
	require.NoError(t, os.WriteFile(configPath, originalConfig, 0o600))
	require.NoError(t, os.WriteFile(accountPath, originalAccounts, 0o600))

	store := &ConfigStore{globalDataPath: configPath}
	require.NoError(t, store.migrateProviderOwnership(map[string]ProviderPluginReference{
		"synthetic": {ID: "synthetic.plugin", Version: "1.2.3"},
	}))

	migrated, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, "synthetic.plugin", gjson.GetBytes(migrated, "providers.synthetic.plugin.id").String())
	require.Equal(t, "secret", gjson.GetBytes(migrated, "providers.synthetic.api_key").String())
	accountsAfter, err := os.ReadFile(accountPath)
	require.NoError(t, err)
	require.JSONEq(t, string(originalAccounts), string(accountsAfter))

	journal, err := os.ReadFile(providerMigrationJournalPath())
	require.NoError(t, err)
	require.Equal(t, "completed", gjson.GetBytes(journal, "state").String())
	require.Equal(t, "opaque-on-read-no-write", gjson.GetBytes(journal, "transcript_migration").String())
	require.FileExists(t, gjson.GetBytes(journal, "config.backup").String())
	require.FileExists(t, gjson.GetBytes(journal, "accounts.backup").String())

	require.NoError(t, RollbackProviderMigration())
	restoredConfig, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.JSONEq(t, string(originalConfig), string(restoredConfig))
	restoredAccounts, err := os.ReadFile(accountPath)
	require.NoError(t, err)
	require.JSONEq(t, string(originalAccounts), string(restoredAccounts))
}

func TestProviderOwnershipMigrationRecoversCommittedPreparedJournal(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("CRUX_GLOBAL_DATA", dataDir)
	t.Setenv("AI_CLI_DIR", t.TempDir())
	configPath := filepath.Join(dataDir, "crux.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{"providers":{"synthetic":{}}}`), 0o600))
	store := &ConfigStore{globalDataPath: configPath}
	ownership := map[string]ProviderPluginReference{"synthetic": {ID: "synthetic.plugin"}}
	require.NoError(t, store.migrateProviderOwnership(ownership))

	journalData, err := os.ReadFile(providerMigrationJournalPath())
	require.NoError(t, err)
	journalData, err = sjson.SetBytes(journalData, "state", "prepared")
	require.NoError(t, err)
	journalData, err = sjson.SetBytes(journalData, "completed_at", nil)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(providerMigrationJournalPath(), journalData, 0o600))

	require.NoError(t, store.migrateProviderOwnership(ownership))
	recovered, err := os.ReadFile(providerMigrationJournalPath())
	require.NoError(t, err)
	require.Equal(t, "completed", gjson.GetBytes(recovered, "state").String())
}

func TestProviderOwnershipMigrationRollbackPreservesLaterAccountChanges(t *testing.T) {
	dataDir := t.TempDir()
	accountDir := t.TempDir()
	t.Setenv("CRUX_GLOBAL_DATA", dataDir)
	t.Setenv("AI_CLI_DIR", accountDir)
	configPath := filepath.Join(dataDir, "crux.json")
	accountPath := filepath.Join(accountDir, "accounts.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{"providers":{"synthetic":{}}}`), 0o600))
	require.NoError(t, os.WriteFile(accountPath, []byte(`{"before":true}`), 0o600))
	store := &ConfigStore{globalDataPath: configPath}
	require.NoError(t, store.migrateProviderOwnership(map[string]ProviderPluginReference{"synthetic": {ID: "synthetic.plugin"}}))
	require.NoError(t, os.WriteFile(accountPath, []byte(`{"later_account_edit":true}`), 0o600))

	require.NoError(t, RollbackProviderMigration())
	accountsAfter, err := os.ReadFile(accountPath)
	require.NoError(t, err)
	require.True(t, gjson.GetBytes(accountsAfter, "later_account_edit").Bool())
}

func TestProviderOwnershipMigrationRollbackRejectsLaterEdits(t *testing.T) {
	dataDir := t.TempDir()
	accountDir := t.TempDir()
	t.Setenv("CRUX_GLOBAL_DATA", dataDir)
	t.Setenv("AI_CLI_DIR", accountDir)

	configPath := filepath.Join(dataDir, "crux.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{"providers":{"synthetic":{}}}`), 0o600))
	store := &ConfigStore{globalDataPath: configPath}
	require.NoError(t, store.migrateProviderOwnership(map[string]ProviderPluginReference{"synthetic": {ID: "synthetic.plugin"}}))
	require.NoError(t, os.WriteFile(configPath, []byte(`{"providers":{"synthetic":{"plugin":{"id":"synthetic.plugin"}}},"later_user_edit":true}`), 0o600))

	err := RollbackProviderMigration()
	require.ErrorContains(t, err, "changed after migration")
	current, readErr := os.ReadFile(configPath)
	require.NoError(t, readErr)
	require.True(t, gjson.GetBytes(current, "later_user_edit").Bool())
}
