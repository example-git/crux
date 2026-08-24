package config

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSetDefaultsDoesNotAddAiCliInstructionFiles(t *testing.T) {
	contextPath := filepath.Join(t.TempDir(), "context.md")
	cfg := Config{Options: &Options{GlobalContextPaths: []string{contextPath}}}

	cfg.setDefaults(t.TempDir(), t.TempDir())

	require.Equal(t, []string{contextPath}, cfg.Options.GlobalContextPaths)
}
