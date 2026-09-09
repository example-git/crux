package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func migrateProviderWithReceipt(t *testing.T, store *ConfigStore, id string, receipt *providerMigrationReceipt) {
	t.Helper()
	before, _, exists, err := fileBytesAndHash(store.globalDataPath)
	require.NoError(t, err)
	require.NoError(t, store.migrateProviderReferencesIfCurrent(map[string]ProviderOwnerReference{
		id: {Type: ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat},
	}, nil, nil, before, exists, receipt))
}

func TestProviderMigrationAutomaticRollbackPreservesLaterTransaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crux.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"sentinel":"original"}`), 0o600))
	own, peer := &ConfigStore{globalDataPath: path}, &ConfigStore{globalDataPath: path}
	var ownReceipt, peerReceipt providerMigrationReceipt
	migrateProviderWithReceipt(t, own, "own", &ownReceipt)
	migrateProviderWithReceipt(t, peer, "peer", &peerReceipt)
	require.True(t, ownReceipt.written())
	require.True(t, peerReceipt.written())
	peerConfig, err := os.ReadFile(path)
	require.NoError(t, err)
	peerJournal, err := os.ReadFile(providerMigrationJournalPathForConfig(path))
	require.NoError(t, err)

	require.ErrorContains(t, own.rollbackProviderMigration(ownReceipt), "different transaction")
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, peerConfig, after)
	journalAfter, err := os.ReadFile(providerMigrationJournalPathForConfig(path))
	require.NoError(t, err)
	require.Equal(t, peerJournal, journalAfter)
	// Explicit user rollback still intentionally selects the latest transaction.
	require.NoError(t, rollbackProviderMigration(path, nil))
	after, err = os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(after), `"own"`)
	require.NotContains(t, string(after), `"peer"`)
}

func TestProviderMigrationZeroWriteReceiptCannotRollbackPriorJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crux.json")
	store := &ConfigStore{globalDataPath: path}
	var receipt providerMigrationReceipt
	migrateProviderWithReceipt(t, store, "own", &receipt)
	require.True(t, receipt.written())
	configBefore, err := os.ReadFile(path)
	require.NoError(t, err)
	journalBefore, err := os.ReadFile(providerMigrationJournalPathForConfig(path))
	require.NoError(t, err)
	migrateProviderWithReceipt(t, store, "own", &receipt) // intentionally reuse the output
	require.False(t, receipt.written())
	require.Empty(t, receipt.fields)
	require.NoError(t, store.rollbackProviderMigration(receipt))
	configAfter, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, configBefore, configAfter)
	journalAfter, err := os.ReadFile(providerMigrationJournalPathForConfig(path))
	require.NoError(t, err)
	require.Equal(t, journalBefore, journalAfter)
}

func TestProviderMigrationAutomaticRollbackWithLostJournalAcknowledgement(t *testing.T) {
	for _, completionPersisted := range []bool{false, true} {
		t.Run(map[bool]string{false: "prepared", true: "completed"}[completionPersisted], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "crux.json")
			original := []byte(`{"sentinel":"original"}`)
			require.NoError(t, os.WriteFile(path, original, 0o600))
			oldWriter := writeProviderMigrationJournal
			writeProviderMigrationJournal = func(journal providerMigrationJournal) error {
				if journal.State == "completed" {
					if completionPersisted {
						require.NoError(t, oldWriter(journal))
					}
					return errors.New("journal acknowledgement unavailable")
				}
				return oldWriter(journal)
			}
			t.Cleanup(func() { writeProviderMigrationJournal = oldWriter })
			store := &ConfigStore{globalDataPath: path}
			var receipt providerMigrationReceipt
			migrateProviderWithReceipt(t, store, "own", &receipt)
			require.True(t, receipt.written())
			if !completionPersisted {
				require.Equal(t, "prepared", receipt.journal.State)
				require.ErrorContains(t, rollbackProviderMigration(path, nil), "not rollbackable")
			}
			writeProviderMigrationJournal = oldWriter
			require.NoError(t, store.rollbackProviderMigration(receipt))
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, original, after)
		})
	}
}

func TestProviderMigrationRollbackRejectsJournalPathOutsideLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crux.json")
	store := &ConfigStore{globalDataPath: path}
	var receipt providerMigrationReceipt
	migrateProviderWithReceipt(t, store, "own", &receipt)
	outside := filepath.Join(t.TempDir(), "outside.json")
	original, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(outside, original, 0o600))
	journal := receipt.journal
	journal.Config.Path = outside
	// Persist the forged journal at the captured path's journal location.
	encoded, err := json.Marshal(journal)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(providerMigrationJournalPathForConfig(path), encoded, 0o600))
	require.ErrorContains(t, rollbackProviderMigration(path, nil), "config recovery metadata")
	after, err := os.ReadFile(outside)
	require.NoError(t, err)
	require.Equal(t, original, after)
}
