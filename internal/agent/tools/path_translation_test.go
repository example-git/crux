package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	powernap "github.com/charmbracelet/x/powernap/pkg/lsp"
	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/db"
	lsputil "github.com/example-git/crux/internal/lsp/util"
	"github.com/example-git/crux/internal/pathtranslate"
	"github.com/example-git/crux/internal/permission"
	"github.com/example-git/crux/internal/shell"
	"github.com/stretchr/testify/require"
)

func TestProtectedShellAccessRequiresApprovalAndRejectsMoves(t *testing.T) {
	workingDir := t.TempDir()
	protected := filepath.Join(workingDir, "protected")
	require.NoError(t, os.Mkdir(protected, 0o700))
	original := filepath.Join(protected, "original.txt")
	require.NoError(t, os.WriteFile(original, []byte("original contents"), 0o600))
	state := t.TempDir()
	conn, err := db.Connect(t.Context(), state)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(state)) })
	store := pathtranslate.NewStore(conn)
	policy, err := store.Save(t.Context(), pathtranslate.Policy{
		Scope:    pathtranslate.Scope{Kind: "session", SessionID: "session"},
		Settings: pathtranslate.Settings{Enabled: true, Targets: []pathtranslate.Target{{Path: protected, Recursive: true}}},
	})
	require.NoError(t, err)
	names, err := store.Names(t.Context())
	require.NoError(t, err)
	translator, err := pathtranslate.New(names, policy.Settings, workingDir)
	require.NoError(t, err)
	for _, test := range []struct {
		name      string
		command   string
		grant     bool
		prompt    bool
		wantError bool
	}{
		{name: "unprotected", command: "printf ordinary"},
		{name: "denied read", command: "cat protected/original.txt", prompt: true, wantError: true},
		{name: "approved read", command: "cat protected/original.txt", prompt: true, grant: true},
		{name: "denied redirection", command: "printf changed > protected/original.txt", prompt: true, wantError: true},
		{name: "denied glob", command: "printf '%s' protected/*", prompt: true, wantError: true},
		{name: "move", command: "mv protected/original.txt elsewhere.txt", wantError: true},
		{name: "ancestor move", command: "mv protected renamed", wantError: true},
		{name: "expanded move", command: "source=protected/original.txt; mv \"$source\" elsewhere.txt", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			permissions := permission.NewPermissionService(workingDir, true, []string{BashToolName})
			events := permissions.Subscribe(ctx)
			filter := &protectedShellAccess{translator: translator, permissions: permissions, sessionID: "session", callID: "call"}
			ctx = shell.WithAccessFilter(permission.WithRunApproval(ctx), filter)
			type result struct {
				output string
				err    error
			}
			done := make(chan result, 1)
			go func() {
				var output bytes.Buffer
				err := shell.Run(ctx, shell.RunOptions{Cwd: workingDir, Command: test.command, Env: os.Environ(), Stdout: &output, Stderr: &output})
				done <- result{output.String(), err}
			}()
			if test.prompt {
				select {
				case event := <-events:
					require.Equal(t, "protected_path", event.Payload.Action)
					if test.grant {
						require.True(t, permissions.Grant(event.Payload))
					} else {
						require.True(t, permissions.Deny(event.Payload))
					}
				case <-ctx.Done():
					t.Fatal("protected-path approval was not requested")
				}
			}
			var got result
			select {
			case got = <-done:
			case <-ctx.Done():
				t.Fatal("shell did not complete")
			}
			if test.wantError {
				require.Error(t, got.err)
			} else {
				require.NoError(t, got.err)
			}
			if test.grant {
				require.Contains(t, got.output, "original contents")
			}
			data, err := os.ReadFile(original)
			require.NoError(t, err)
			require.Equal(t, "original contents", string(data))
		})
	}
}

func TestProtectedShellAccessRejectsIdentityState(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	conn, err := db.Connect(t.Context(), state)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(state)) })
	store := pathtranslate.NewStore(conn)
	_, err = store.Save(t.Context(), pathtranslate.Policy{Scope: pathtranslate.Scope{Kind: "session", SessionID: "session"}})
	require.NoError(t, err)
	names, err := store.Names(t.Context())
	require.NoError(t, err)
	translator, err := pathtranslate.New(names, pathtranslate.Settings{}, root)
	require.NoError(t, err)
	require.NoError(t, os.Symlink(filepath.Join(state, "crux.db"), filepath.Join(root, "linked.txt")))
	require.NoError(t, os.WriteFile(filepath.Join(state, "ordinary.txt"), []byte("ordinary data"), 0o600))
	for _, command := range []string{
		"cat state/crux.db", "cat < state/crux.db", "cat linked.txt", "file=state/crux.db; cat \"$file\"",
		"cp state/crux.db copied.db", "printf changed > state/crux.db-journal",
	} {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		filter := &protectedShellAccess{translator: translator}
		var output bytes.Buffer
		err := shell.Run(shell.WithAccessFilter(ctx, filter), shell.RunOptions{Command: command, Cwd: root, Env: os.Environ(), Stdout: &output, Stderr: &output})
		cancel()
		require.Error(t, err, command)
		require.ErrorIs(t, filter.denied, pathtranslate.ErrInternalStateAccess, command)
		require.NotContains(t, output.String(), "SQLite format", command)
	}
	var output bytes.Buffer
	filter := &protectedShellAccess{translator: translator}
	err = shell.Run(shell.WithAccessFilter(t.Context(), filter), shell.RunOptions{Command: "cat state/ordinary.txt", Cwd: root, Env: os.Environ(), Stdout: &output})
	require.NoError(t, err)
	require.Equal(t, "ordinary data", output.String())
}

func TestShellAliasDecodingPreservesArgumentBoundaries(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	protected := filepath.Join(root, "private $folder 'with spaces'")
	require.NoError(t, os.Mkdir(protected, 0o700))
	original := filepath.Join(protected, "literal* $name 'quote'.txt")
	require.NoError(t, os.WriteFile(original, []byte("read the exact original"), 0o600))
	state := t.TempDir()
	conn, err := db.Connect(t.Context(), state)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(state)) })
	store := pathtranslate.NewStore(conn)
	policy, err := store.Save(t.Context(), pathtranslate.Policy{Scope: pathtranslate.Scope{Kind: "session", SessionID: "shell-alias"}, Settings: pathtranslate.Settings{Enabled: true, Targets: []pathtranslate.Target{{Path: protected, Recursive: true}}}})
	require.NoError(t, err)
	names, err := store.Names(t.Context())
	require.NoError(t, err)
	translator, err := pathtranslate.New(names, policy.Settings, root)
	require.NoError(t, err)
	alias, err := translator.Path(t.Context(), original)
	require.NoError(t, err)
	directoryAlias, err := translator.Path(t.Context(), protected)
	require.NoError(t, err)
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "shell-alias")
	ctx = pathtranslate.WithTranslator(ctx, translator)
	tool, manager := newBashToolAndManagerForTest(root)
	t.Cleanup(func() { manager.KillAll(context.Background()) })
	for _, command := range []string{
		"cat " + alias,
		"cat '" + alias + "'",
		"cat \"" + alias + "\"",
		"file=" + alias + "; cat \"$file\"",
		"cat " + directoryAlias + "/*.txt",
	} {
		input, err := json.Marshal(BashParams{Command: command})
		require.NoError(t, err)
		resolved, err := translator.JSON(ctx, string(input), true)
		require.NoError(t, err)
		response, err := tool.Run(ctx, fantasy.ToolCall{ID: "read", Name: BashToolName, Input: resolved})
		require.NoError(t, err, command)
		require.False(t, response.IsError, response.Content)
		require.Contains(t, response.Content, "read the exact original", command)
	}
	plain := "printf '%s' \"keep $this\"\n"
	decoded, err := translator.ResolveShell(ctx, plain)
	require.NoError(t, err)
	require.Equal(t, plain, decoded)
}

func TestProtectedWorkspaceMovesPreserveOriginalAndAllowUnprotectedMove(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	state := t.TempDir()
	conn, err := db.Connect(t.Context(), state)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(state)) })
	store := pathtranslate.NewStore(conn)
	protected := filepath.Join(root, "protected.txt")
	policy, err := store.Save(t.Context(), pathtranslate.Policy{Scope: pathtranslate.Scope{Kind: "session", SessionID: "move"}, Settings: pathtranslate.Settings{Enabled: true, Targets: []pathtranslate.Target{{Path: protected}}}})
	require.NoError(t, err)
	names, err := store.Names(t.Context())
	require.NoError(t, err)
	translator, err := pathtranslate.New(names, policy.Settings, root)
	require.NoError(t, err)
	ctx := pathtranslate.WithTranslator(t.Context(), translator)
	for _, source := range []string{protected, filepath.Join(root, "ordinary.txt")} {
		require.NoError(t, os.WriteFile(source, []byte("original"), 0o600))
		destination := source + ".moved"
		edit := &protocol.WorkspaceEdit{DocumentChanges: []protocol.DocumentChange{{RenameFile: &protocol.RenameFile{OldURI: protocol.DocumentURI(protocol.URIFromPath(source)), NewURI: protocol.DocumentURI(protocol.URIFromPath(destination))}}}}
		require.NoError(t, canonicalizeWorkspaceEdit(edit, root))
		err := checkProtectedWorkspaceMoves(ctx, edit)
		if source == protected {
			require.ErrorContains(t, err, "not permitted")
			require.FileExists(t, source)
			require.NoFileExists(t, destination)
		} else {
			require.NoError(t, err)
			require.NoError(t, lsputil.ApplyWorkspaceEdit(*edit, powernap.UTF16))
			require.NoFileExists(t, source)
			require.FileExists(t, destination)
		}
	}
}
