//go:build !windows

package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func makeAuthenticationJournalPublic(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.Chmod(path, 0o644))
}
