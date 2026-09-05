package log

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/redact"
	"github.com/stretchr/testify/require"
)

func registerOpaqueAccountSecret(t *testing.T, secret string) {
	t.Helper()
	directory := t.TempDir()
	t.Setenv("AI_CLI_DIR", directory)
	content := `{"active":{"synthetic":"one"},"accounts":{"synthetic":[{"id":"one","displayName":"One","accessToken":"access","raw":{"metadata":{"tenant":"` + secret + `"}}}]}}`
	require.NoError(t, os.WriteFile(filepath.Join(directory, "accounts.json"), []byte(content), 0o600))
	_, err := accounts.List(t.Context(), "synthetic")
	require.NoError(t, err)
}

func TestRedactingHandlerScrubsOpaqueAccountMaterial(t *testing.T) {
	secret := "handler-account-raw-secret-value"
	registerOpaqueAccountSecret(t, secret)
	var output bytes.Buffer
	handler := redactingHandler{handler: slog.NewJSONHandler(&output, nil)}
	slog.New(handler).InfoContext(t.Context(), "account metadata", "value", secret)
	require.NotContains(t, output.String(), secret)
	require.Contains(t, output.String(), redact.Replacement)
}

func TestRedactingHandlerScrubsMessagesAttributesGroupsAndErrors(t *testing.T) {
	secret := "handler-secret-value"
	redact.Register(secret)
	var output bytes.Buffer
	handler := redactingHandler{handler: slog.NewJSONHandler(&output, nil)}
	logger := slog.New(handler.WithAttrs([]slog.Attr{slog.String("bound", secret)}))
	logger.ErrorContext(t.Context(), "failed "+secret,
		slog.String("value", secret),
		slog.Group("nested", slog.String("token", secret)),
		slog.Any("error", errors.New("error "+secret)),
	)
	require.NotContains(t, output.String(), secret)
	require.GreaterOrEqual(t, bytes.Count(output.Bytes(), []byte(redact.Replacement)), 5)
}

func TestRecoverPanicScrubsKnownSecrets(t *testing.T) {
	secret := "panic-secret-value"
	redact.Register(secret)
	workingDirectory, err := os.Getwd()
	require.NoError(t, err)
	temporary := t.TempDir()
	require.NoError(t, os.Chdir(temporary))
	t.Cleanup(func() { require.NoError(t, os.Chdir(workingDirectory)) })
	func() {
		defer RecoverPanic("test", nil)
		panic("panic " + secret)
	}()
	matches, err := filepath.Glob(filepath.Join(temporary, "crux-panic-test-*.log"))
	require.NoError(t, err)
	require.Len(t, matches, 1)
	content, err := os.ReadFile(matches[0])
	require.NoError(t, err)
	require.NotContains(t, string(content), secret)
	require.Contains(t, string(content), redact.Replacement)
}

func TestRedactingHandlerEnabledDelegates(t *testing.T) {
	handler := redactingHandler{handler: slog.NewTextHandler(&bytes.Buffer{}, nil)}
	require.True(t, handler.Enabled(context.Background(), slog.LevelInfo))
}
