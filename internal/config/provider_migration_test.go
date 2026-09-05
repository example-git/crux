package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/example-git/crux/internal/providerregistry"
)

func TestPreparedProviderMigrationValidatesBackupBeforeCleanup(t *testing.T) {
	for _, scenario := range []string{"valid", "external", "absent", "directory-symlink", "backup-symlink"} {
		t.Run(scenario, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "crux.json")
			original := []byte(`{"providers":{}}`)
			require.NoError(t, os.WriteFile(configPath, original, 0o600))
			migrationDir := providerMigrationDirForConfig(configPath)
			require.NoError(t, os.MkdirAll(migrationDir, 0o700))
			backup := filepath.Join(migrationDir, "config.test.backup")
			require.NoError(t, os.WriteFile(backup, original, 0o600))
			outside := filepath.Join(t.TempDir(), "config.test.backup")
			require.NoError(t, os.WriteFile(outside, original, 0o600))
			journal := providerMigrationJournal{
				Version: providerOwnershipMigrationVersion,
				State:   "prepared",
				Config:  providerMigrationFile{Path: configPath, Backup: backup, BeforeHash: hashBytes(original), AfterHash: "post-image", Existed: true},
			}
			switch scenario {
			case "external":
				journal.Config.Backup = outside
			case "absent":
				require.NoError(t, os.Remove(configPath))
				journal.Config.Existed = false
				journal.Config.BeforeHash = ""
				journal.Config.Backup = outside
			case "directory-symlink":
				relocated := filepath.Join(t.TempDir(), "v1")
				require.NoError(t, os.Rename(migrationDir, relocated))
				require.NoError(t, os.Symlink(relocated, migrationDir))
			case "backup-symlink":
				require.NoError(t, os.Remove(backup))
				require.NoError(t, os.Symlink(outside, backup))
			}
			require.NoError(t, writeMigrationJournal(journal))
			err := recoverPreparedProviderMigration(configPath)
			if scenario == "valid" {
				require.NoError(t, err)
				require.NoFileExists(t, backup)
				require.NoFileExists(t, providerMigrationJournalPathForConfig(configPath))
			} else {
				require.Error(t, err)
				require.FileExists(t, providerMigrationJournalPathForConfig(configPath))
				require.FileExists(t, backup)
			}
			data, err := os.ReadFile(outside)
			require.NoError(t, err)
			require.Equal(t, original, data)
		})
	}
}

func TestProviderOwnershipMigrationCreatesAndRollsBackMissingConfig(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("CRUX_GLOBAL_DATA", dataDir)
	t.Setenv("AI_CLI_DIR", t.TempDir())
	configPath := filepath.Join(dataDir, "crux.json")
	store := &ConfigStore{globalDataPath: configPath}

	require.NoError(t, store.migrateProviderReferences(map[string]ProviderOwnerReference{
		"custom": {Type: ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat},
	}, nil, nil))
	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, "custom", gjson.GetBytes(data, "providers.custom.owner.type").String())
	require.Equal(t, "openai-compat", gjson.GetBytes(data, "providers.custom.owner.construction").String())
	journal, err := os.ReadFile(providerMigrationJournalPath())
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(journal, "config.existed").Bool())
	require.Empty(t, gjson.GetBytes(journal, "config.backup").String())

	require.NoError(t, RollbackProviderMigration())
	_, err = os.Stat(configPath)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestProviderOwnershipMigrationBacksUpConfigAndRollsBack(t *testing.T) {
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
	require.False(t, gjson.GetBytes(journal, "accounts").Exists())
	accountBackups, err := filepath.Glob(filepath.Join(providerMigrationDir(), "accounts.*.backup"))
	require.NoError(t, err)
	require.Empty(t, accountBackups)

	require.NoError(t, RollbackProviderMigration())
	restoredConfig, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.JSONEq(t, string(originalConfig), string(restoredConfig))
	restoredAccounts, err := os.ReadFile(accountPath)
	require.NoError(t, err)
	require.JSONEq(t, string(originalAccounts), string(restoredAccounts))
}

func TestProviderPresetOwnershipMigrationPreservesProviderAndSelectionValues(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("CRUX_GLOBAL_DATA", dataDir)
	t.Setenv("AI_CLI_DIR", t.TempDir())

	configPath := filepath.Join(dataDir, "crux.json")
	originalConfig := []byte(`{"providers":{"synthetic":{"api_key":"secret","base_url":"https://api.example.test/v1","extra_headers":{"X-Test":"value"},"extra_body":{"nested":{"enabled":true}},"configuration":{"region":"test"},"models":[{"id":"model-1","default_max_tokens":4096}]}},"models":{"large":{"provider":"synthetic","model":"model-1","max_tokens":8192,"reasoning_effort":"high","think":true,"temperature":0.21,"top_p":0.81,"top_k":17,"frequency_penalty":0.31,"presence_penalty":0.41,"provider_options":{"mode":"large","nested":{"enabled":true}}},"small":{"provider":"synthetic","model":"model-1","max_tokens":4096,"reasoning_effort":"medium","think":true,"temperature":0.22,"top_p":0.82,"top_k":18,"frequency_penalty":0.32,"presence_penalty":0.42,"provider_options":{"mode":"small","nested":{"enabled":false}}}}}`)
	require.NoError(t, os.WriteFile(configPath, originalConfig, 0o600))

	store := &ConfigStore{globalDataPath: configPath}
	require.NoError(t, store.migrateProviderReferences(
		map[string]ProviderOwnerReference{
			"synthetic": {Type: ProviderOwnerPreset, Construction: providerregistry.ConstructionOpenAICompat},
		},
		nil,
		map[string]ProviderPresetReference{
			"synthetic": {ID: "synthetic.preset", Version: "1.2.3", Digest: "sha256:synthetic"},
		},
	))

	migrated, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, "preset", gjson.GetBytes(migrated, "providers.synthetic.owner.type").String())
	require.Equal(t, "openai-compat", gjson.GetBytes(migrated, "providers.synthetic.owner.construction").String())
	require.Equal(t, "synthetic.preset", gjson.GetBytes(migrated, "providers.synthetic.preset.id").String())
	require.Equal(t, "1.2.3", gjson.GetBytes(migrated, "providers.synthetic.preset.version").String())
	require.Equal(t, "sha256:synthetic", gjson.GetBytes(migrated, "providers.synthetic.preset.digest").String())
	withoutOwner, err := sjson.DeleteBytes(migrated, "providers.synthetic.owner")
	require.NoError(t, err)
	withoutPreset, err := sjson.DeleteBytes(withoutOwner, "providers.synthetic.preset")
	require.NoError(t, err)
	require.JSONEq(t, string(originalConfig), string(withoutPreset))
}

func TestProviderPresetOwnershipMigrationPinsLegacyEmptyVersionWithoutChangingValues(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("CRUX_GLOBAL_DATA", dataDir)
	t.Setenv("AI_CLI_DIR", t.TempDir())

	configPath := filepath.Join(dataDir, "crux.json")
	originalConfig := []byte(`{"providers":{"deepseek":{"preset":{"id":"crux.catwalk.deepseek"},"api_key":"secret","base_url":"https://api.example.test/v1","configuration":{"region":"test"},"models":[{"id":"model-1","default_max_tokens":4096}]}},"models":{"large":{"provider":"deepseek","model":"model-1","max_tokens":8192,"reasoning_effort":"high","think":true,"temperature":0.21,"top_p":0.81,"top_k":17,"frequency_penalty":0.31,"presence_penalty":0.41,"provider_options":{"mode":"large"}},"small":{"provider":"deepseek","model":"model-1","max_tokens":4096,"reasoning_effort":"medium","think":true,"temperature":0.22,"top_p":0.82,"top_k":18,"frequency_penalty":0.32,"presence_penalty":0.42,"provider_options":{"mode":"small"}}}}`)
	require.NoError(t, os.WriteFile(configPath, originalConfig, 0o600))

	store := &ConfigStore{globalDataPath: configPath}
	require.NoError(t, store.migrateProviderReferences(
		map[string]ProviderOwnerReference{
			"deepseek": {Type: ProviderOwnerPreset, Construction: providerregistry.ConstructionOpenAICompat},
		},
		nil,
		map[string]ProviderPresetReference{
			"deepseek": {ID: "crux.catwalk.deepseek", Version: "0.51.23", Digest: "sha256:deepseek"},
		},
	))

	migrated, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, "preset", gjson.GetBytes(migrated, "providers.deepseek.owner.type").String())
	require.Equal(t, "openai-compat", gjson.GetBytes(migrated, "providers.deepseek.owner.construction").String())
	require.Equal(t, "0.51.23", gjson.GetBytes(migrated, "providers.deepseek.preset.version").String())
	require.Equal(t, "sha256:deepseek", gjson.GetBytes(migrated, "providers.deepseek.preset.digest").String())
	withoutOwner, err := sjson.DeleteBytes(migrated, "providers.deepseek.owner")
	require.NoError(t, err)
	withoutVersion, err := sjson.DeleteBytes(withoutOwner, "providers.deepseek.preset.version")
	require.NoError(t, err)
	withoutDigest, err := sjson.DeleteBytes(withoutVersion, "providers.deepseek.preset.digest")
	require.NoError(t, err)
	require.JSONEq(t, string(originalConfig), string(withoutDigest))
}

func TestProviderOwnershipMigrationRequiresExactActiveVersion(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("CRUX_GLOBAL_DATA", dataDir)
	configPath := filepath.Join(dataDir, "crux.json")
	original := []byte(`{"providers":{"synthetic":{}}}`)
	require.NoError(t, os.WriteFile(configPath, original, 0o600))

	store := &ConfigStore{globalDataPath: configPath}
	require.NoError(t, store.migrateProviderReferences(
		map[string]ProviderOwnerReference{
			"synthetic":   {Type: ProviderOwnerPlugin, Construction: providerregistry.ConstructionGenericJSON},
			"preset-only": {Type: ProviderOwnerPreset, Construction: providerregistry.ConstructionOpenAICompat},
		},
		map[string]ProviderPluginReference{"synthetic": {ID: "synthetic.plugin"}},
		map[string]ProviderPresetReference{"preset-only": {ID: "synthetic.preset"}},
	))
	current, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, original, current)
	_, statErr := os.Stat(providerMigrationDir())
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestProviderOwnershipMigrationPrefersPluginOverPreset(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("CRUX_GLOBAL_DATA", dataDir)
	configPath := filepath.Join(dataDir, "crux.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{"providers":{"synthetic":{}}}`), 0o600))

	store := &ConfigStore{globalDataPath: configPath}
	require.NoError(t, store.migrateProviderReferences(
		map[string]ProviderOwnerReference{
			"synthetic": {
				Type:                 ProviderOwnerPlugin,
				Construction:         providerregistry.ConstructionGenericJSON,
				CompatibilityAdapter: providerregistry.ConstructionOpenAIResponses,
			},
		},
		map[string]ProviderPluginReference{"synthetic": {ID: "synthetic.plugin", Version: "2.0.0"}},
		map[string]ProviderPresetReference{"synthetic": {ID: "synthetic.preset", Version: "1.0.0", Digest: "sha256:synthetic"}},
	))
	current, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, "plugin", gjson.GetBytes(current, "providers.synthetic.owner.type").String())
	require.Equal(t, "generic-json", gjson.GetBytes(current, "providers.synthetic.owner.construction").String())
	require.Equal(t, "openai-responses", gjson.GetBytes(current, "providers.synthetic.owner.compatibility_adapter").String())
	require.Equal(t, "synthetic.plugin", gjson.GetBytes(current, "providers.synthetic.plugin.id").String())
	require.Equal(t, "2.0.0", gjson.GetBytes(current, "providers.synthetic.plugin.version").String())
	require.False(t, gjson.GetBytes(current, "providers.synthetic.preset").Exists())
}

func TestProviderOwnershipMigrationRecoversCommittedPreparedJournal(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("CRUX_GLOBAL_DATA", dataDir)
	t.Setenv("AI_CLI_DIR", t.TempDir())
	configPath := filepath.Join(dataDir, "crux.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{"providers":{"synthetic":{}}}`), 0o600))
	store := &ConfigStore{globalDataPath: configPath}
	ownership := map[string]ProviderPluginReference{"synthetic": {ID: "synthetic.plugin", Version: "1.0.0"}}
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
	require.NoError(t, store.migrateProviderOwnership(map[string]ProviderPluginReference{"synthetic": {ID: "synthetic.plugin", Version: "1.0.0"}}))
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
	require.NoError(t, store.migrateProviderOwnership(map[string]ProviderPluginReference{"synthetic": {ID: "synthetic.plugin", Version: "1.0.0"}}))
	require.NoError(t, os.WriteFile(configPath, []byte(`{"providers":{"synthetic":{"plugin":{"id":"synthetic.plugin"}}},"later_user_edit":true}`), 0o600))

	err := RollbackProviderMigration()
	require.ErrorContains(t, err, "changed after migration")
	current, readErr := os.ReadFile(configPath)
	require.NoError(t, readErr)
	require.True(t, gjson.GetBytes(current, "later_user_edit").Bool())
}
