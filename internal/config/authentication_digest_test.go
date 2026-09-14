package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/example-git/crux/internal/fsext"
	"github.com/stretchr/testify/require"
)

func TestAuthenticationDigestKeyPersistsAcrossStores(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "crux.json")
	first, err := (&ConfigStore{globalDataPath: path}).loadAuthenticationDigest(t.Context())
	require.NoError(t, err)
	second, err := (&ConfigStore{globalDataPath: path}).loadAuthenticationDigest(t.Context())
	require.NoError(t, err)
	require.Equal(t, first.bytesID(authenticationDigestCredential, []byte("credential")), second.bytesID(authenticationDigestCredential, []byte("credential")))
	keyFile, err := os.Open(path + ".authentication-hmac.key")
	require.NoError(t, err)
	defer keyFile.Close()
	require.NoError(t, fsext.ValidatePrivateFile(keyFile))
	if runtime.GOOS != "windows" {
		info, err := keyFile.Stat()
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}

	other, err := (&ConfigStore{globalDataPath: filepath.Join(t.TempDir(), "crux.json")}).loadAuthenticationDigest(t.Context())
	require.NoError(t, err)
	require.NotEqual(t, first.bytesID(authenticationDigestCredential, []byte("credential")), other.bytesID(authenticationDigestCredential, []byte("credential")))
}

func TestAuthenticationDigestRejectsInvalidKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crux.json")
	keyPath := path + ".authentication-hmac.key"
	require.NoError(t, os.WriteFile(keyPath, []byte("short"), 0o600))
	_, err := (&ConfigStore{globalDataPath: path}).loadAuthenticationDigest(t.Context())
	require.ErrorContains(t, err, "invalid identity, size, or permissions")
	data, readErr := os.ReadFile(keyPath)
	require.NoError(t, readErr)
	require.Equal(t, []byte("short"), data)
}
