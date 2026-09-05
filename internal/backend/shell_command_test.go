package backend

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/example-git/crux/internal/app"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/db"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/session"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestRunShellCommandUsesWorkspaceEnvironment(t *testing.T) {
	const environmentKey = "CRUX_T4_4_4_SHELL_ENV"
	root := t.TempDir()
	t.Setenv("CRUX_GLOBAL_CONFIG", filepath.Join(root, "global-config"))
	t.Setenv("CRUX_GLOBAL_DATA", filepath.Join(root, "global-data"))
	t.Setenv("CRUX_CACHE_DIR", filepath.Join(root, "cache"))
	t.Setenv("CRUX_PROVIDER_PROFILE", "core-only")
	t.Setenv(environmentKey, "process")
	for _, path := range []string{os.Getenv("CRUX_GLOBAL_CONFIG"), os.Getenv("CRUX_GLOBAL_DATA")} {
		require.NoError(t, os.MkdirAll(path, 0o755))
	}
	baseEnvironment := config.SnapshotEnvironment()

	loadStore := func(name, value string) (*config.ConfigStore, string) {
		workspacePath := filepath.Join(root, name)
		dataPath := filepath.Join(root, name+"-data")
		require.NoError(t, os.MkdirAll(workspacePath, 0o755))
		require.NoError(t, os.MkdirAll(dataPath, 0o755))
		require.NoError(t, os.WriteFile(
			filepath.Join(dataPath, "crux.json"),
			[]byte(`{"env":{"`+environmentKey+`":"`+value+`"}}`),
			0o600,
		))
		store, err := config.LoadIsolated(workspacePath, dataPath, false, baseEnvironment)
		require.NoError(t, err)
		return store, workspacePath
	}
	storeA, pathA := loadStore("workspace-a", "workspace-a")
	storeB, pathB := loadStore("workspace-b", "workspace-b")

	backend, _ := newTestBackend(t)
	workspaceA := &Workspace{ID: uuid.New().String(), Path: pathA, Cfg: storeA, clients: make(map[string]*clientState), shutdownFn: func() {}}
	workspaceB := &Workspace{ID: uuid.New().String(), Path: pathB, Cfg: storeB, clients: make(map[string]*clientState), shutdownFn: func() {}}
	InsertWorkspaceForTest(backend, workspaceA)
	InsertWorkspaceForTest(backend, workspaceB)

	for _, test := range []struct {
		workspaceID string
		expected    string
	}{
		{workspaceID: workspaceA.ID, expected: "workspace-a"},
		{workspaceID: workspaceB.ID, expected: "workspace-b"},
	} {
		response, err := backend.RunShellCommand(t.Context(), test.workspaceID, proto.ShellCommandRequest{
			Command: `printf '%s' "$` + environmentKey + `"`,
		})
		require.NoError(t, err)
		require.Equal(t, test.expected, response.Output)
		require.Zero(t, response.ExitCode)
	}
	require.Equal(t, "process", os.Getenv(environmentKey))
}

func TestRunShellCommand_SkipsPersistenceForMissingSession(t *testing.T) {
	t.Parallel()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	q := db.New(conn)
	sessions := session.NewService(q, conn)
	messages := message.NewService(q)

	b, _ := newTestBackend(t)
	ws := &Workspace{
		ID:           uuid.New().String(),
		Path:         t.TempDir(),
		Cfg:          config.NewTestStore(&config.Config{}),
		resolvedPath: t.TempDir(),
		clients:      make(map[string]*clientState),
		shutdownFn:   func() {},
	}
	ws.App = &app.App{
		Sessions: sessions,
		Messages: messages,
	}
	ws.ctx, ws.cancel = context.WithCancel(b.ctx)
	InsertWorkspaceForTest(b, ws)

	missingSessionID := uuid.New().String()
	resp, err := b.RunShellCommand(t.Context(), ws.ID, proto.ShellCommandRequest{
		SessionID: missingSessionID,
		Command:   "echo hello",
	})
	require.NoError(t, err)
	require.Equal(t, "hello\n", resp.Output)
	require.Zero(t, resp.ExitCode)

	stored, err := messages.List(t.Context(), missingSessionID)
	require.NoError(t, err)
	require.Empty(t, stored)
}
