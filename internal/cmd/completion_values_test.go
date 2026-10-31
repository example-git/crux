package cmd

import (
	"bytes"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/connection"
	"github.com/example-git/crux/internal/db"
	"github.com/example-git/crux/internal/projects"
	"github.com/example-git/crux/internal/session"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func dynamicCompletionTestRoot(t *testing.T) *cobra.Command {
	root := completionTestRoot(t)
	root.PersistentFlags().String("data-dir", "", "Data directory")
	root.PersistentFlags().String("host", "", "Server")
	root.Flags().StringP("session", "s", "", "Session")
	run, _, err := root.Find([]string{"run"})
	require.NoError(t, err)
	run.Flags().StringP("session", "s", "", "Session")
	sessions, _, err := root.Find([]string{"session"})
	require.NoError(t, err)
	for _, name := range []string{"show", "delete", "rename"} {
		sessions.AddCommand(&cobra.Command{Use: name, RunE: func(*cobra.Command, []string) error {
			t.Fatal("completion executed session command")
			return nil
		}})
	}
	return root
}

func dynamicCompletionRequest(t *testing.T, args ...string) (string, string) {
	t.Helper()
	var out, diagnostic bytes.Buffer
	handled, err := executeShellCompletion(dynamicCompletionTestRoot(t), "crux-test", args, &out, &diagnostic)
	require.True(t, handled)
	require.NoError(t, err)
	return out.String(), diagnostic.String()
}

func TestDynamicShellSessionCompletions(t *testing.T) {
	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	id := "12345678-1234-1234-1234-123456789abc"
	_, err = db.New(conn).CreateSession(t.Context(), db.CreateSessionParams{ID: id, Title: "Fix\tbuild\n\x1b[31merrors\x1b[0m"})
	require.NoError(t, err)
	_, err = db.New(conn).CreateSession(t.Context(), db.CreateSessionParams{ID: "child", Title: "Nested", ParentSessionID: sql.NullString{String: id, Valid: true}})
	require.NoError(t, err)
	for _, args := range [][]string{
		{"--session", ""},
		{"-s", "1234"},
		{"run", "--session", ""},
		{"session", "show", ""},
		{"session", "delete", ""},
		{"session", "rename", ""},
	} {
		out, _ := dynamicCompletionRequest(t, append([]string{"__complete", "--data-dir", dataDir}, args...)...)
		require.Equal(t, id+"\tFix build errors\n:4\n", out)
	}
	hash := session.HashID(id)
	out, _ := dynamicCompletionRequest(t, "__complete", "--data-dir", dataDir, "--session", hash[:7])
	require.Equal(t, hash+"\tFix build errors\n:4\n", out)
	out, _ = dynamicCompletionRequest(t, "__completeNoDesc", "--data-dir", dataDir, "--session", "")
	require.Equal(t, id+"\n:4\n", out)
	out, _ = dynamicCompletionRequest(t, "__complete", "--data-dir", dataDir, "session", "rename", id, "")
	require.Equal(t, ":4\n", out)
	out, _ = dynamicCompletionRequest(t, "__complete", "--data-dir", dataDir, "--session", "no-match")
	require.Equal(t, ":4\n", out)
}

func TestDynamicShellCompletionMissingStorage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent")
	out, _ := dynamicCompletionRequest(t, "__complete", "--data-dir", path, "--session", "")
	require.Equal(t, ":4\n", out)
	require.NoDirExists(t, path)
}

func TestDynamicShellCompletionInvalidWorkingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent")
	out, diagnostic := dynamicCompletionRequest(t, "__complete", "--cwd", path, "--session", "")
	require.Equal(t, ":5\n", out)
	require.Contains(t, diagnostic, "completion:")
	require.NoDirExists(t, path)
}

func TestDynamicShellCompletionConfiguredDirectory(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CRUX_GLOBAL_CONFIG", filepath.Join(root, "global-config"))
	t.Setenv("CRUX_GLOBAL_DATA", filepath.Join(root, "global-data"))
	require.NoError(t, os.WriteFile(filepath.Join(root, "crux.json"), []byte(`{"options":{"data_directory":"state"}}`), 0o600))
	conn, err := db.Connect(t.Context(), filepath.Join(root, "state"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(filepath.Join(root, "state"))) })
	_, err = db.New(conn).CreateSession(t.Context(), db.CreateSessionParams{ID: "configured-session", Title: "Configured storage"})
	require.NoError(t, err)
	cwd, err := os.Getwd()
	require.NoError(t, err)
	out, _ := dynamicCompletionRequest(t, "__complete", "--cwd", root, "--session", "")
	require.Contains(t, out, "configured-session\tConfigured storage")
	after, err := os.Getwd()
	require.NoError(t, err)
	require.Equal(t, cwd, after)
}

func TestDynamicShellCompletionRemoteSessions(t *testing.T) {
	var paths []string
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/v1/workspaces":
			fmt.Fprint(w, `[{"id":"workspace","path":"/remote/project"}]`)
		case "/v1/workspaces/workspace/sessions":
			fmt.Fprint(w, `[{"id":"remote-session","title":"Remote work"}]`)
		default:
			t.Errorf("unexpected completion request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer host.Close()
	address := "tcp://" + strings.TrimPrefix(host.URL, "http://")
	out, diagnostic := dynamicCompletionRequest(t, "__complete", "--host", address, "--cwd", "/remote/project", "--session", "")
	require.Equal(t, "remote-session\tRemote work\n:4\n", out, diagnostic)
	require.Equal(t, []string{"GET /v1/workspaces", "GET /v1/workspaces/workspace/sessions"}, paths)
	out, diagnostic = dynamicCompletionRequest(t, "__complete", "--host", address, "--cwd", "/missing", "--session", "")
	require.Equal(t, ":5\n", out)
	require.Contains(t, diagnostic, "no existing server workspace")
}

func TestDynamicShellCompletionSavedConnections(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CRUX_GLOBAL_DATA", root)
	t.Setenv("CRUX_CACHE_DIR", filepath.Join(root, "cache"))
	code, err := connection.EnsureServerIdentity(t.Context())
	require.NoError(t, err)
	host := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			t.Error("missing client authentication")
		}
		switch r.URL.Path {
		case "/v1/workspaces":
			fmt.Fprint(w, `[{"id":"workspace","path":"/remote/project"}]`)
		case "/v1/workspaces/workspace/sessions":
			fmt.Fprint(w, `[{"id":"authenticated-session","title":"Authenticated work"}]`)
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	address := "tcp://" + host.Listener.Addr().String()
	_, clientCode, err := connection.Add(t.Context(), "workstation", address, code)
	require.NoError(t, err)
	require.NoError(t, connection.AuthorizeClient(t.Context(), "workstation", clientCode))
	tlsConfig, err := connection.ServerTLSConfig(t.Context())
	require.NoError(t, err)
	host.TLS = tlsConfig
	host.StartTLS()
	defer host.Close()
	out, _ := dynamicCompletionRequest(t, "__complete", "--connection", "work")
	require.Equal(t, "workstation\t"+address+"\n:4\n", out)
	out, diagnostic := dynamicCompletionRequest(t, "__complete", "--connection", "workstation", "--cwd", "/remote/project", "--session", "")
	require.Equal(t, "authenticated-session\tAuthenticated work\n:4\n", out, diagnostic)
	out, diagnostic = dynamicCompletionRequest(t, "__complete", "--connection", "missing", "--session", "")
	require.Equal(t, ":5\n", out)
	require.Contains(t, diagnostic, "saved connection not found")
}

func TestDynamicShellCompletionProjectDirectories(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CRUX_GLOBAL_DATA", root)
	require.NoError(t, projects.Register("/known/project", "/known/project/.crux"))
	out, _ := dynamicCompletionRequest(t, "__complete", "--cwd", "/known")
	require.Equal(t, "/known/project\tKnown project\n:4\n", out)
	out, _ = dynamicCompletionRequest(t, "__complete", "--cwd", "/different")
	require.Equal(t, ":16\n", out)
}
