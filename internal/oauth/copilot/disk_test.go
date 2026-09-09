package copilot

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestImportReadsCapturedTokenPath(t *testing.T) {
	write := func(root, token string) {
		t.Helper()
		path := tokenFilePathForEnvironment(func(string) string { return root })
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(fmt.Sprintf(`{"github.com:Iv1.b507a08c87ecfe98":{"oauth_token":%q}}`, token)), 0o600))
	}
	clientRoot, hostRoot := t.TempDir(), t.TempDir()
	write(clientRoot, "synthetic-client-github")
	write(hostRoot, "synthetic-host-github")
	ctx := ContextWithImportEnvironment(t.Context(), func(string) string { return clientRoot })
	t.Setenv("HOME", hostRoot)
	t.Setenv("LOCALAPPDATA", hostRoot)
	token, found := RefreshTokenFromDiskForContext(ctx)
	require.True(t, found)
	require.Equal(t, "synthetic-client-github", token)
	token, found = RefreshTokenFromDiskForContext(t.Context())
	require.True(t, found)
	require.Equal(t, "synthetic-host-github", token, "ordinary local import retains its process environment")
	empty := ContextWithImportEnvironment(t.Context(), func(string) string { return "" })
	token, found = RefreshTokenFromDiskForContext(empty)
	require.False(t, found)
	require.Empty(t, token, "explicit absent client path must not use the host token")
}
