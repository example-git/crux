package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRestoreConfigPreimagesOnlyRevertsRecordedPostimages(t *testing.T) {
	t.Run("existing file externally replaced", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "crux.json")
		original := []byte(`{"value":"original"}`)
		corrected := []byte(`{"value":"corrected"}`)
		external := []byte(`{"value":"external"}`)
		require.NoError(t, os.WriteFile(path, original, 0o600))
		preimages, err := captureConfigPreimages(path)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, corrected, 0o600))
		require.NoError(t, recordConfigPostimage(preimages, path, corrected))
		require.NoError(t, os.WriteFile(path, external, 0o600))

		require.NoError(t, restoreConfigPreimages(preimages))
		actual, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, external, actual)
	})

	t.Run("created file unchanged", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "crux.json")
		corrected := []byte(`{"value":"corrected"}`)
		preimages, err := captureConfigPreimages(path)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, corrected, 0o600))
		require.NoError(t, recordConfigPostimage(preimages, path, corrected))

		require.NoError(t, restoreConfigPreimages(preimages))
		_, err = os.Stat(path)
		require.ErrorIs(t, err, os.ErrNotExist)
	})

	t.Run("created file externally replaced", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "crux.json")
		corrected := []byte(`{"value":"corrected"}`)
		external := []byte(`{"value":"external"}`)
		preimages, err := captureConfigPreimages(path)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, corrected, 0o600))
		require.NoError(t, recordConfigPostimage(preimages, path, corrected))
		require.NoError(t, os.WriteFile(path, external, 0o600))

		require.NoError(t, restoreConfigPreimages(preimages))
		actual, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, external, actual)
	})
}

func TestConfigPostimagesVerifyWithoutAdoptingPeerWrites(t *testing.T) {
	for _, written := range []bool{false, true} {
		t.Run(fmt.Sprintf("written=%t", written), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "crux.json")
			original := []byte(`{"value":"original"}`)
			require.NoError(t, os.WriteFile(path, original, 0o600))
			preimages, err := captureConfigPreimages(path)
			require.NoError(t, err)
			if written {
				own := []byte(`{"value":"own"}`)
				require.NoError(t, os.WriteFile(path, own, 0o600))
				require.NoError(t, recordConfigPostimage(preimages, path, own))
			}
			expected := slices.Clone(preimages[0].expectedData)
			peer := []byte(`{"value":"peer"}`)
			require.NoError(t, os.WriteFile(path, peer, 0o600))
			require.ErrorContains(t, captureConfigPostimages(preimages), "changed after correction")
			require.Equal(t, expected, preimages[0].expectedData)
			require.Equal(t, written, preimages[0].written)
			require.NoError(t, restoreConfigPreimages(preimages))
			actual, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, peer, actual)
		})
	}
}

func TestRestoreUnwrittenPreimagesDoesNotReplaceFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crux.json")
	require.NoError(t, os.WriteFile(path, []byte(`{}`), 0o600))
	preimages, err := captureConfigPreimages(path)
	require.NoError(t, err)
	before, err := os.Stat(path)
	require.NoError(t, err)
	require.NoError(t, captureConfigPostimages(preimages))
	require.NoError(t, restoreConfigPreimages(preimages))
	after, err := os.Stat(path)
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after), "observing an unchanged file never authorizes replacing it")
	require.NoFileExists(t, path+".lock")
}

func TestStartupCorrectionDeduplicatesSharedGlobalPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crux.json")
	original := []byte(`{"options":{"disable_notifications":true}}`)
	require.NoError(t, os.WriteFile(path, original, 0o600))
	plan := prepareDisableNotificationsMigration(path, path)
	preimages, err := captureConfigPreimages(path, path)
	require.NoError(t, err)
	require.Len(t, preimages, 1)
	require.NoError(t, commitStartupCorrections(&ConfigStore{globalDataPath: path}, plan, nil, preimages))
	require.True(t, preimages[0].written)
	require.NoError(t, captureConfigPostimages(preimages))
	require.NoError(t, restoreConfigPreimages(preimages))
	actual, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, original, actual)
}

func TestNotificationPlanRequiresOriginalSourceEvenWhenCorrectionMatches(t *testing.T) {
	root := t.TempDir()
	source, data := filepath.Join(root, "config.json"), filepath.Join(root, "data.json")
	require.NoError(t, os.WriteFile(source, []byte(`{"options":{"disable_notifications":true}}`), 0o600))
	require.NoError(t, os.WriteFile(data, []byte(`{"options":{"notifications":"auto"}}`), 0o600))
	plan := prepareDisableNotificationsMigration(source, data)
	peer := []byte(`{"options":{"disable_notifications":false}}`)
	require.NoError(t, os.WriteFile(source, peer, 0o600))
	preimages, err := captureConfigPreimages(source, data)
	require.NoError(t, err)
	transformed, err := plan.apply(source, peer)
	require.NoError(t, err)
	require.Equal(t, plan.overrides[source], transformed, "removed fields alone cannot prove the planning preimage")
	require.ErrorContains(t, plan.validatePreimages(preimages), "changed before capture")
	require.ErrorContains(t, commitStartupCorrections(&ConfigStore{globalDataPath: data}, plan, nil, preimages), "changed before capture")
	require.NoError(t, restoreConfigPreimages(preimages))
	actual, err := os.ReadFile(source)
	require.NoError(t, err)
	require.Equal(t, peer, actual)
}
