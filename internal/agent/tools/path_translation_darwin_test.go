package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/db"
	"github.com/example-git/crux/internal/pathtranslate"
	"github.com/example-git/crux/internal/permission"
	"github.com/example-git/crux/internal/shell"
	"github.com/stretchr/testify/require"
)

func TestProtectedShellTTYAndBackgroundRequireActualApproval(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	protected := filepath.Join(root, "private.txt")
	require.NoError(t, os.WriteFile(protected, []byte("original contents"), 0o600))
	state := t.TempDir()
	conn, err := db.Connect(t.Context(), state)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(state)) })
	store := pathtranslate.NewStore(conn)
	policy, err := store.Save(t.Context(), pathtranslate.Policy{Scope: pathtranslate.Scope{Kind: "session", SessionID: "shell"}, Settings: pathtranslate.Settings{Enabled: true, Targets: []pathtranslate.Target{{Path: protected}}}})
	require.NoError(t, err)
	names, err := store.Names(t.Context())
	require.NoError(t, err)
	translator, err := pathtranslate.New(names, policy.Settings, root)
	require.NoError(t, err)
	for _, mode := range []string{"tty", "background"} {
		for _, grant := range []bool{false, true} {
			t.Run(mode+map[bool]string{false: "-denied", true: "-approved"}[grant], func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				ctx = context.WithValue(ctx, SessionIDContextKey, "shell")
				ctx = pathtranslate.WithTranslator(ctx, translator)
				permissions := permission.NewPermissionService(root, true, []string{BashToolName})
				events := permissions.Subscribe(ctx)
				type outcome struct {
					text string
					err  error
				}
				done := make(chan outcome, 1)
				go func() {
					if mode == "background" {
						manager := shell.NewBackgroundShellManager(root)
						defer manager.KillAll(context.Background())
						tool := NewBashTool(manager, permissions, root)
						input, marshalErr := json.Marshal(BashParams{Command: "cat private.txt", RunInBackground: true})
						if marshalErr != nil {
							done <- outcome{err: marshalErr}
							return
						}
						response, runErr := tool.Run(ctx, fantasy.ToolCall{ID: "read", Name: BashToolName, Input: string(input)})
						done <- outcome{text: response.Content, err: runErr}
						return
					}
					master, terminal, openErr := pty.Open()
					if openErr != nil {
						done <- outcome{err: openErr}
						return
					}
					defer master.Close()
					defer terminal.Close()
					ttyCtx := shell.WithAccessFilter(ctx, &protectedShellAccess{translator: translator, permissions: permissions, sessionID: "shell", callID: "read"})
					var output bytes.Buffer
					runErr := shell.NewShell(&shell.Options{WorkingDir: root}).ExecTTY(ttyCtx, "cat private.txt", terminal, &output)
					done <- outcome{text: output.String(), err: runErr}
				}()
				select {
				case event := <-events:
					require.Equal(t, "protected_path", event.Payload.Action)
					if grant {
						require.True(t, permissions.Grant(event.Payload))
					} else {
						require.True(t, permissions.Deny(event.Payload))
					}
				case <-ctx.Done():
					t.Fatal("no protected-path approval request")
				}
				select {
				case result := <-done:
					if grant {
						require.NoError(t, result.err)
						require.Contains(t, result.text, "original contents")
					} else {
						require.NotContains(t, result.text, "original contents")
						require.True(t, result.err != nil || strings.Contains(result.text, "not approved"))
					}
				case <-ctx.Done():
					t.Fatal("shell did not finish after approval decision")
				}
			})
		}
	}
}
