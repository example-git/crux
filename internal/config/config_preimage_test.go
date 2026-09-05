package config

import (
	"os"
	"path/filepath"
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
