package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const providerOwnershipMigrationVersion = 1

// providerMigrationMu serializes the host-global migration journal. Config
// stores lock their own config paths independently, but the journal and backup
// namespace is shared by every store in this process.
var providerMigrationMu sync.Mutex

type providerMigrationFile struct {
	Path       string `json:"path"`
	Backup     string `json:"backup,omitempty"`
	BeforeHash string `json:"before_sha256,omitempty"`
	AfterHash  string `json:"after_sha256,omitempty"`
	Existed    bool   `json:"existed"`
}

type providerMigrationJournal struct {
	Version             int                   `json:"version"`
	State               string                `json:"state"`
	CreatedAt           time.Time             `json:"created_at"`
	CompletedAt         time.Time             `json:"completed_at,omitempty"`
	RolledBackAt        time.Time             `json:"rolled_back_at,omitempty"`
	Providers           []string              `json:"providers"`
	Config              providerMigrationFile `json:"config"`
	Accounts            providerMigrationFile `json:"accounts"`
	TranscriptMigration string                `json:"transcript_migration"`
}

func providerMigrationDir() string {
	return filepath.Join(GlobalWorkspaceDir(), "provider-migrations", "v1")
}

func providerMigrationJournalPath() string {
	return filepath.Join(providerMigrationDir(), "journal.json")
}

func fileBytesAndHash(path string) ([]byte, string, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", false, nil
	}
	if err != nil {
		return nil, "", false, err
	}
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:]), true, nil
}

func writeMigrationJournal(journal providerMigrationJournal) error {
	data, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return atomicWriteFile(providerMigrationJournalPath(), data, 0o600)
}

func recoverPreparedProviderMigration(configPath string) error {
	data, err := os.ReadFile(providerMigrationJournalPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read provider migration journal: %w", err)
	}
	var journal providerMigrationJournal
	if err := json.Unmarshal(data, &journal); err != nil {
		return fmt.Errorf("decode provider migration journal: %w", err)
	}
	if journal.Version != providerOwnershipMigrationVersion || journal.State != "prepared" {
		return nil
	}
	_, currentHash, exists, err := fileBytesAndHash(configPath)
	if err != nil {
		return fmt.Errorf("recover provider migration: %w", err)
	}
	if exists == journal.Config.Existed && currentHash == journal.Config.BeforeHash {
		// The config commit never happened. A fresh migration can safely replace
		// this prepared journal while retaining its timestamped backups.
		return nil
	}
	if exists && currentHash == journal.Config.AfterHash {
		journal.State = "completed"
		journal.CompletedAt = time.Now().UTC()
		return writeMigrationJournal(journal)
	}
	return errors.New("provider migration was interrupted and the config no longer matches its pre-image or staged post-image")
}

func backupMigrationFile(name, path, migrationID string) (providerMigrationFile, error) {
	data, hash, existed, err := fileBytesAndHash(path)
	if err != nil {
		return providerMigrationFile{}, fmt.Errorf("read %s: %w", name, err)
	}
	result := providerMigrationFile{Path: path, BeforeHash: hash, Existed: existed}
	if !existed {
		return result, nil
	}
	result.Backup = filepath.Join(providerMigrationDir(), name+"."+migrationID+".backup")
	if err := atomicWriteFile(result.Backup, data, 0o600); err != nil {
		return providerMigrationFile{}, fmt.Errorf("backup %s: %w", name, err)
	}
	return result, nil
}

// migrateProviderOwnership records active manifest ownership in the global
// configuration. Existing credentials, account records, selected models, and
// transcript bytes are not rewritten. A journal and pre-image backups are
// durable before the one atomic config mutation.
func (s *ConfigStore) migrateProviderOwnership(ownership map[string]ProviderPluginReference) error {
	if len(ownership) == 0 {
		return nil
	}
	providerMigrationMu.Lock()
	defer providerMigrationMu.Unlock()
	configPath, err := s.configPath(ScopeGlobal)
	if err != nil {
		return err
	}
	accountPath, err := accounts.Path()
	if err != nil {
		return fmt.Errorf("resolve account store: %w", err)
	}
	if err := os.MkdirAll(providerMigrationDir(), 0o700); err != nil {
		return fmt.Errorf("create provider migration directory: %w", err)
	}

	// Accounts are deliberately not rewritten: stable account namespaces and
	// opaque account metadata already satisfy the plugin contract. Retain a
	// pre-image as recovery evidence before taking the config transaction lock.
	migrationID := time.Now().UTC().Format("20060102T150405.000000000Z")
	accountBackup, err := backupMigrationFile("accounts", accountPath, migrationID)
	if err != nil {
		return err
	}
	accountBackup.AfterHash = accountBackup.BeforeHash

	unlock, err := s.lockConfig(ScopeGlobal)
	if err != nil {
		return err
	}
	defer unlock()
	if err := recoverPreparedProviderMigration(configPath); err != nil {
		return err
	}

	configData, _, exists, err := fileBytesAndHash(configPath)
	if err != nil || !exists {
		return err
	}
	providers := make([]string, 0, len(ownership))
	keys := make([]string, 0, len(ownership))
	values := make(map[string]ProviderPluginReference, len(ownership))
	for providerID, plugin := range ownership {
		key := fmt.Sprintf("providers.%s.plugin", providerID)
		if current := gjson.GetBytes(configData, key); current.Exists() {
			continue
		}
		providers = append(providers, providerID)
		keys = append(keys, key)
		values[key] = plugin
	}
	if len(keys) == 0 {
		return nil
	}
	slices.Sort(providers)
	slices.Sort(keys)
	updated := string(configData)
	for _, key := range keys {
		updated, err = sjson.Set(updated, key, values[key])
		if err != nil {
			return fmt.Errorf("set provider ownership %s: %w", key, err)
		}
	}
	updatedData := []byte(updated)

	configBackup, err := backupMigrationFile("config", configPath, migrationID)
	if err != nil {
		return err
	}
	postSum := sha256.Sum256(updatedData)
	configBackup.AfterHash = hex.EncodeToString(postSum[:])
	journal := providerMigrationJournal{
		Version:             providerOwnershipMigrationVersion,
		State:               "prepared",
		CreatedAt:           time.Now().UTC(),
		Providers:           providers,
		Config:              configBackup,
		Accounts:            accountBackup,
		TranscriptMigration: "opaque-on-read-no-write",
	}
	if err := writeMigrationJournal(journal); err != nil {
		return fmt.Errorf("write provider migration journal: %w", err)
	}
	if err := atomicWriteFile(configPath, updatedData, 0o600); err != nil {
		return fmt.Errorf("commit provider ownership migration: %w", err)
	}
	journal.State = "completed"
	journal.CompletedAt = time.Now().UTC()
	if err := writeMigrationJournal(journal); err != nil {
		return fmt.Errorf("complete provider migration journal: %w", err)
	}
	return nil
}

// RollbackProviderMigration restores migration-owned pre-images only when the
// current files still match the journaled post-images. This compare-and-swap
// check prevents rollback from overwriting later user edits.
func RollbackProviderMigration() error {
	providerMigrationMu.Lock()
	defer providerMigrationMu.Unlock()
	data, err := os.ReadFile(providerMigrationJournalPath())
	if err != nil {
		return err
	}
	var journal providerMigrationJournal
	if err := json.Unmarshal(data, &journal); err != nil {
		return fmt.Errorf("decode provider migration journal: %w", err)
	}
	if journal.Version != providerOwnershipMigrationVersion || journal.State != "completed" {
		return fmt.Errorf("provider migration is not rollbackable (version=%d state=%q)", journal.Version, journal.State)
	}
	_, currentHash, exists, err := fileBytesAndHash(journal.Config.Path)
	if err != nil {
		return fmt.Errorf("read current config: %w", err)
	}
	if journal.Config.AfterHash != currentHash || exists != journal.Config.Existed {
		return errors.New("refusing provider migration rollback: config changed after migration")
	}
	var configBackup []byte
	if journal.Config.Existed {
		configBackup, err = os.ReadFile(journal.Config.Backup)
		if err != nil {
			return fmt.Errorf("read config backup: %w", err)
		}
	}
	// Accounts are not mutated by this migration, but their pre-image remains a
	// recovery artifact. Verify that backup before changing the config; do not
	// compare or restore the live account file because later account changes are
	// unrelated user state and must survive rollback.
	if journal.Accounts.Existed {
		accountBackup, err := os.ReadFile(journal.Accounts.Backup)
		if err != nil {
			return fmt.Errorf("read accounts backup: %w", err)
		}
		sum := sha256.Sum256(accountBackup)
		if hex.EncodeToString(sum[:]) != journal.Accounts.BeforeHash {
			return errors.New("refusing provider migration rollback: accounts backup failed integrity verification")
		}
	}
	if journal.Config.Existed {
		if err := atomicWriteFile(journal.Config.Path, configBackup, 0o600); err != nil {
			return fmt.Errorf("restore config: %w", err)
		}
	} else if err := os.Remove(journal.Config.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove migrated config: %w", err)
	}
	journal.State = "rolled-back"
	journal.RolledBackAt = time.Now().UTC()
	return writeMigrationJournal(journal)
}
