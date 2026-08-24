package accounts

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/example-git/crux/internal/redact"
	"github.com/stretchr/testify/require"
)

func TestAccountReadsRegisterStoredCredentials(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("AI_CLI_DIR", directory)
	access := "stored-account-access-value"
	refresh := "stored-account-refresh-value"
	content := `{"active":{"codex":"one"},"accounts":{"codex":[{"id":"one","displayName":"One","accessToken":"` + access + `","refreshToken":"` + refresh + `"}]}}`
	require.NoError(t, os.WriteFile(filepath.Join(directory, "accounts.json"), []byte(content), 0o600))
	entries, err := List(t.Context(), "codex")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, redact.Replacement, redact.String(access))
	active, err := Active(t.Context(), "codex")
	require.NoError(t, err)
	require.NotNil(t, active)
	require.Equal(t, redact.Replacement, redact.String(refresh))
}
