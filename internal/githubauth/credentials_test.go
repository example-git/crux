package githubauth

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLegacyIndexFileSourceIsPurposeScopedAndBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"accessToken":"index-token","authMode":"vscode"}`), 0o600))
	source := LegacyIndexFileSource{Path: path}

	token, err := source.Token(context.Background(), CodebaseIndex)
	require.NoError(t, err)
	require.Equal(t, "index-token", token)
	_, err = source.Token(context.Background(), CopilotInference)
	require.ErrorContains(t, err, `does not serve purpose "copilot-inference"`)

	require.NoError(t, os.WriteFile(path, make([]byte, maxCredentialBytes+1), 0o600))
	_, err = source.Token(context.Background(), CodebaseIndex)
	require.ErrorContains(t, err, "bounded regular file")
}

func TestLegacyIndexFileSourceRejectsSymlinkAndHonorsCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"accessToken":"index-token","authMode":"vscode"}`), 0o600))
	if runtime.GOOS != "windows" {
		link := path + ".link"
		require.NoError(t, os.Symlink(path, link))
		_, err := (LegacyIndexFileSource{Path: link}).Token(context.Background(), CodebaseIndex)
		require.ErrorContains(t, err, "bounded regular file")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (LegacyIndexFileSource{Path: path}).Token(ctx, CodebaseIndex)
	require.ErrorIs(t, err, context.Canceled)
}

func TestLegacyIndexFileSourceMissingIsUnsigned(t *testing.T) {
	token, err := (LegacyIndexFileSource{Path: filepath.Join(t.TempDir(), "missing.json")}).Token(context.Background(), CodebaseIndex)
	require.NoError(t, err)
	require.Empty(t, token)
}
