package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const providerOwnershipMigrationVersion = 1

// providerMigrationMu serializes the host-global migration journal. Config
// stores lock their own config paths independently, but the journal and backup
// namespace is shared by every store in this process.
var (
	providerMigrationMu           sync.Mutex
	makeProviderMigrationDir      = os.MkdirAll
	writeProviderMigrationBackup  = atomicWriteFile
	writeProviderMigrationJournal = writeMigrationJournal
	writeProviderMigrationConfig  = atomicWriteFile
)

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
	TranscriptMigration string                `json:"transcript_migration"`
}

func providerMigrationDir() string {
	return providerMigrationDirForConfig(GlobalConfigData())
}

func providerMigrationDirForConfig(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "provider-migrations", "v1")
}

func providerMigrationJournalPath() string {
	return providerMigrationJournalPathForConfig(GlobalConfigData())
}

func providerMigrationJournalPathForConfig(configPath string) string {
	return filepath.Join(providerMigrationDirForConfig(configPath), "journal.json")
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
	return atomicWriteFile(providerMigrationJournalPathForConfig(journal.Config.Path), data, 0o600)
}

func readProviderMigrationJournal() (providerMigrationJournal, []byte, bool, error) {
	return readProviderMigrationJournalForConfig(GlobalConfigData())
}

func readProviderMigrationJournalForConfig(configPath string) (providerMigrationJournal, []byte, bool, error) {
	data, err := os.ReadFile(providerMigrationJournalPathForConfig(configPath))
	if errors.Is(err, os.ErrNotExist) {
		return providerMigrationJournal{}, nil, false, nil
	}
	if err != nil {
		return providerMigrationJournal{}, nil, false, err
	}
	var journal providerMigrationJournal
	if err := json.Unmarshal(data, &journal); err != nil {
		return providerMigrationJournal{}, nil, true, err
	}
	return journal, data, true, nil
}

func validateProviderMigrationRecovery(configPath string, journal providerMigrationJournal) error {
	if journal.Version != providerOwnershipMigrationVersion || journal.State != "prepared" && journal.State != "completed" {
		return fmt.Errorf("invalid provider migration recovery journal (version=%d state=%q)", journal.Version, journal.State)
	}
	if journal.Config.Path != configPath || journal.Config.AfterHash == "" {
		return errors.New("provider migration journal has incomplete config recovery metadata")
	}
	if journal.Config.Existed {
		if journal.Config.Backup == "" || journal.Config.BeforeHash == "" {
			return errors.New("provider migration journal has incomplete config recovery metadata")
		}
		migrationDir := filepath.Clean(providerMigrationDirForConfig(configPath))
		backupPath := filepath.Clean(journal.Config.Backup)
		backupName := filepath.Base(backupPath)
		if filepath.Dir(backupPath) != migrationDir || !strings.HasPrefix(backupName, "config.") || !strings.HasSuffix(backupName, ".backup") {
			return errors.New("provider migration journal has an invalid config backup path")
		}
		_, backupHash, backupExists, err := fileBytesAndHash(backupPath)
		if err != nil {
			return fmt.Errorf("read provider migration config backup: %w", err)
		}
		if !backupExists || backupHash != journal.Config.BeforeHash {
			return errors.New("provider migration config backup failed integrity verification")
		}
	} else if journal.Config.Backup != "" || journal.Config.BeforeHash != "" {
		return errors.New("provider migration journal has invalid absent config recovery metadata")
	}
	_, currentHash, exists, err := fileBytesAndHash(configPath)
	if err != nil {
		return fmt.Errorf("read committed provider migration config: %w", err)
	}
	if !exists || currentHash != journal.Config.AfterHash {
		return errors.New("provider migration config does not match its staged post-image")
	}
	return nil
}

func validatePreparedProviderMigration(configPath string, journal providerMigrationJournal) error {
	if journal.State != "prepared" {
		return fmt.Errorf("invalid prepared provider migration journal state %q", journal.State)
	}
	return validateProviderMigrationRecovery(configPath, journal)
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func removeProviderMigrationDirIfEmpty(configPath string) {
	migrationDir := providerMigrationDirForConfig(configPath)
	_ = os.Remove(migrationDir)
	_ = os.Remove(filepath.Dir(migrationDir))
}

func cleanupAbortedProviderMigration(configPath, backupPath string, previousJournal []byte, previousJournalExisted, removeEmptyDir bool) error {
	journalPath := providerMigrationJournalPathForConfig(configPath)
	var err error
	if previousJournalExisted {
		err = atomicWriteFile(journalPath, previousJournal, 0o600)
	} else if removeErr := os.Remove(journalPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		err = removeErr
	}
	if err != nil {
		return fmt.Errorf("restore provider migration journal: %w", err)
	}
	if backupPath != "" {
		if removeErr := os.Remove(backupPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return fmt.Errorf("remove provider migration backup: %w", removeErr)
		}
	}
	if removeEmptyDir {
		removeProviderMigrationDirIfEmpty(configPath)
	}
	return nil
}

func recoverPreparedProviderMigration(configPath string) error {
	journal, _, exists, err := readProviderMigrationJournalForConfig(configPath)
	if err != nil {
		return fmt.Errorf("read provider migration journal: %w", err)
	}
	if !exists || journal.Version != providerOwnershipMigrationVersion || journal.State != "prepared" {
		return nil
	}
	if journal.Config.Path != configPath {
		return errors.New("recover provider migration: prepared journal belongs to a different config path")
	}
	_, currentHash, configExists, err := fileBytesAndHash(configPath)
	if err != nil {
		return fmt.Errorf("recover provider migration: %w", err)
	}
	if configExists == journal.Config.Existed && currentHash == journal.Config.BeforeHash {
		if err := cleanupAbortedProviderMigration(configPath, journal.Config.Backup, nil, false, true); err != nil {
			return fmt.Errorf("clean uncommitted provider migration: %w", err)
		}
		return nil
	}
	if err := validatePreparedProviderMigration(configPath, journal); err != nil {
		return fmt.Errorf("recover provider migration: %w", err)
	}
	journal.State = "completed"
	journal.CompletedAt = time.Now().UTC()
	if err := writeProviderMigrationJournal(journal); err != nil {
		return fmt.Errorf("complete recovered provider migration: %w", err)
	}
	return nil
}

// migrateProviderOwnership records active manifest ownership in the global
// configuration. Existing credentials, account records, selected models, and
// transcript bytes are not rewritten. Recovery artifacts are persisted before
// the atomic config commit.
func (s *ConfigStore) migrateProviderOwnership(ownership map[string]ProviderPluginReference) error {
	return s.migrateProviderReferences(nil, ownership, nil)
}

type providerMigrationPrecondition struct {
	hash   string
	exists bool
}

func (s *ConfigStore) migrateProviderReferences(owners map[string]ProviderOwnerReference, plugins map[string]ProviderPluginReference, presets map[string]ProviderPresetReference) error {
	return s.migrateProviderReferencesWithPrecondition(owners, plugins, presets, nil)
}

func (s *ConfigStore) migrateProviderReferencesIfCurrent(owners map[string]ProviderOwnerReference, plugins map[string]ProviderPluginReference, presets map[string]ProviderPresetReference, expectedData []byte, expectedExists bool) error {
	return s.migrateProviderReferencesWithPrecondition(owners, plugins, presets, &providerMigrationPrecondition{hash: hashBytes(expectedData), exists: expectedExists})
}

func (s *ConfigStore) migrateProviderReferencesWithPrecondition(owners map[string]ProviderOwnerReference, plugins map[string]ProviderPluginReference, presets map[string]ProviderPresetReference, precondition *providerMigrationPrecondition) error {
	if len(owners) == 0 && len(plugins) == 0 && len(presets) == 0 {
		return nil
	}
	providerMigrationMu.Lock()
	defer providerMigrationMu.Unlock()
	configPath, err := s.configPath(ScopeGlobal)
	if err != nil {
		return err
	}
	if configPath == "" {
		return nil
	}
	migrationDir := providerMigrationDirForConfig(configPath)

	unlock, err := s.lockConfig(ScopeGlobal)
	if err != nil {
		return err
	}
	defer unlock()
	if err := recoverPreparedProviderMigration(configPath); err != nil {
		return err
	}

	configData, configHash, exists, err := fileBytesAndHash(configPath)
	if err != nil {
		return err
	}
	if precondition != nil && (exists != precondition.exists || exists && configHash != precondition.hash) {
		return errors.New("provider ownership migration source changed before mutation")
	}
	if !exists {
		configData = []byte("{}")
	}
	providerSet := make(map[string]bool, len(owners)+len(plugins)+len(presets))
	keys := make([]string, 0, len(owners)+len(plugins)+len(presets))
	values := make(map[string]any, len(owners)+len(plugins)+len(presets))
	for providerID, owner := range owners {
		if owner.Type == "" || owner.Construction == "" {
			continue
		}
		if owner.Type == ProviderOwnerPlugin {
			plugin, exact := plugins[providerID]
			if !exact || plugin.ID == "" || plugin.Version == "" {
				continue
			}
		}
		if owner.Type == ProviderOwnerPreset {
			preset, exact := presets[providerID]
			if !exact || preset.ID == "" || preset.Version == "" || preset.Digest == "" {
				continue
			}
		}
		key := fmt.Sprintf("providers.%s.owner", providerID)
		current := gjson.GetBytes(configData, key)
		if current.Exists() {
			currentType := current.Get("type").String()
			currentConstruction := current.Get("construction").String()
			currentAdapter := current.Get("compatibility_adapter").String()
			if currentType != "" && currentType != string(owner.Type) ||
				currentConstruction != "" && currentConstruction != string(owner.Construction) ||
				currentAdapter != "" && currentAdapter != string(owner.CompatibilityAdapter) {
				continue
			}
			if currentType == string(owner.Type) && currentConstruction == string(owner.Construction) && currentAdapter == string(owner.CompatibilityAdapter) {
				continue
			}
		}
		providerSet[providerID] = true
		keys = append(keys, key)
		values[key] = owner
	}
	for providerID, plugin := range plugins {
		if plugin.ID == "" || plugin.Version == "" {
			continue
		}
		key := fmt.Sprintf("providers.%s.plugin", providerID)
		current := gjson.GetBytes(configData, key)
		if current.Exists() {
			currentID := current.Get("id").String()
			currentVersion := current.Get("version").String()
			if currentID != "" && currentID != plugin.ID || currentVersion != "" && currentVersion != plugin.Version {
				continue
			}
			if currentID == plugin.ID && currentVersion == plugin.Version {
				continue
			}
		}
		providerSet[providerID] = true
		keys = append(keys, key)
		values[key] = plugin
	}
	for providerID, preset := range presets {
		if _, pluginOwned := plugins[providerID]; pluginOwned || preset.ID == "" || preset.Version == "" || preset.Digest == "" {
			continue
		}
		key := fmt.Sprintf("providers.%s.preset", providerID)
		current := gjson.GetBytes(configData, key)
		if current.Exists() {
			currentID := current.Get("id").String()
			currentVersion := current.Get("version").String()
			currentDigest := current.Get("digest").String()
			if currentID != "" && currentID != preset.ID || currentVersion != "" && currentVersion != preset.Version ||
				currentDigest != "" && currentDigest != preset.Digest {
				continue
			}
			if currentID == preset.ID && currentVersion == preset.Version && currentDigest == preset.Digest {
				continue
			}
		}
		providerSet[providerID] = true
		keys = append(keys, key)
		values[key] = preset
	}
	if len(keys) == 0 {
		return nil
	}
	providers := make([]string, 0, len(providerSet))
	for providerID := range providerSet {
		providers = append(providers, providerID)
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

	migrationDirExisted := false
	if stat, statErr := os.Stat(migrationDir); statErr == nil {
		if !stat.IsDir() {
			return errors.New("prepare provider ownership migration directory: path is not a directory")
		}
		migrationDirExisted = true
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("inspect provider ownership migration directory: %w", statErr)
	}
	if err := makeProviderMigrationDir(migrationDir, 0o700); err != nil {
		if !migrationDirExisted {
			removeProviderMigrationDirIfEmpty(configPath)
		}
		return fmt.Errorf("prepare provider ownership migration directory: %w", err)
	}

	_, previousJournalData, previousJournalExisted, err := readProviderMigrationJournalForConfig(configPath)
	if err != nil {
		if !migrationDirExisted {
			removeProviderMigrationDirIfEmpty(configPath)
		}
		return fmt.Errorf("read previous provider migration journal: %w", err)
	}
	migrationID := time.Now().UTC().Format("20060102T150405.000000000Z")
	configBackup := providerMigrationFile{
		Path:       configPath,
		BeforeHash: configHash,
		AfterHash:  hashBytes(updatedData),
		Existed:    exists,
	}
	if exists {
		configBackup.Backup = filepath.Join(migrationDir, "config."+migrationID+".backup")
		if err := writeProviderMigrationBackup(configBackup.Backup, configData, 0o600); err != nil {
			cleanupErr := cleanupAbortedProviderMigration(configPath, configBackup.Backup, previousJournalData, previousJournalExisted, !migrationDirExisted)
			return errors.Join(fmt.Errorf("backup provider ownership config: %w", err), cleanupErr)
		}
		_, backupHash, backupExists, err := fileBytesAndHash(configBackup.Backup)
		if err != nil || !backupExists || backupHash != configBackup.BeforeHash {
			if err == nil {
				err = errors.New("config backup failed integrity verification")
			}
			cleanupErr := cleanupAbortedProviderMigration(configPath, configBackup.Backup, previousJournalData, previousJournalExisted, !migrationDirExisted)
			return errors.Join(fmt.Errorf("verify provider ownership config backup: %w", err), cleanupErr)
		}
	}
	journal := providerMigrationJournal{
		Version:             providerOwnershipMigrationVersion,
		State:               "prepared",
		CreatedAt:           time.Now().UTC(),
		Providers:           providers,
		Config:              configBackup,
		TranscriptMigration: "opaque-on-read-no-write",
	}
	if err := writeProviderMigrationJournal(journal); err != nil {
		cleanupErr := cleanupAbortedProviderMigration(configPath, configBackup.Backup, previousJournalData, previousJournalExisted, !migrationDirExisted)
		return errors.Join(fmt.Errorf("prepare provider ownership migration journal: %w", err), cleanupErr)
	}
	persistedJournal, _, persisted, err := readProviderMigrationJournalForConfig(configPath)
	if err != nil || !persisted {
		if err == nil {
			err = errors.New("prepared journal was not persisted")
		}
		cleanupErr := cleanupAbortedProviderMigration(configPath, configBackup.Backup, previousJournalData, previousJournalExisted, !migrationDirExisted)
		return errors.Join(fmt.Errorf("verify prepared provider ownership migration journal: %w", err), cleanupErr)
	}
	if persistedJournal.Config != journal.Config || persistedJournal.Version != journal.Version || persistedJournal.State != journal.State || !slices.Equal(persistedJournal.Providers, journal.Providers) {
		cleanupErr := cleanupAbortedProviderMigration(configPath, configBackup.Backup, previousJournalData, previousJournalExisted, !migrationDirExisted)
		return errors.Join(errors.New("verify prepared provider ownership migration journal: persisted journal does not match staged migration"), cleanupErr)
	}

	if err := writeProviderMigrationConfig(configPath, updatedData, 0o600); err != nil {
		cleanupErr := cleanupAbortedProviderMigration(configPath, configBackup.Backup, previousJournalData, previousJournalExisted, !migrationDirExisted)
		return errors.Join(fmt.Errorf("commit provider ownership migration: %w", err), cleanupErr)
	}
	journal.State = "completed"
	journal.CompletedAt = time.Now().UTC()
	if err := writeProviderMigrationJournal(journal); err != nil {
		prepared, _, preparedExists, readErr := readProviderMigrationJournalForConfig(configPath)
		if readErr == nil && !preparedExists {
			readErr = errors.New("prepared recovery journal is missing")
		}
		if readErr == nil {
			readErr = validateProviderMigrationRecovery(configPath, prepared)
		}
		if readErr == nil {
			slog.Warn("Provider ownership migration committed with a prepared recovery journal", "error", err)
			return nil
		}
		var restoreErr error
		if exists {
			restoreErr = atomicWriteFile(configPath, configData, 0o600)
		} else if removeErr := os.Remove(configPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			restoreErr = removeErr
		}
		var cleanupErr error
		if restoreErr == nil {
			cleanupErr = cleanupAbortedProviderMigration(configPath, configBackup.Backup, previousJournalData, previousJournalExisted, !migrationDirExisted)
		}
		return errors.Join(fmt.Errorf("complete provider ownership migration journal: %w", err), fmt.Errorf("prepared recovery is invalid: %w", readErr), restoreErr, cleanupErr)
	}
	return nil
}

// RollbackProviderMigration restores migration-owned pre-images only when the
// current files still match the journaled post-images. This compare-and-swap
// check prevents rollback from overwriting later user edits.
func RollbackProviderMigration() error {
	return rollbackProviderMigration(GlobalConfigData())
}

func (s *ConfigStore) rollbackProviderMigration() error {
	return rollbackProviderMigration(s.globalDataPath)
}

func rollbackProviderMigration(configPath string) error {
	providerMigrationMu.Lock()
	defer providerMigrationMu.Unlock()
	data, err := os.ReadFile(providerMigrationJournalPathForConfig(configPath))
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
	if !exists || journal.Config.AfterHash != currentHash {
		return errors.New("refusing provider migration rollback: config changed after migration")
	}
	var configBackup []byte
	if journal.Config.Existed {
		configBackup, err = os.ReadFile(journal.Config.Backup)
		if err != nil {
			return fmt.Errorf("read config backup: %w", err)
		}
		if hashBytes(configBackup) != journal.Config.BeforeHash {
			return errors.New("refusing provider migration rollback: config backup failed integrity verification")
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
