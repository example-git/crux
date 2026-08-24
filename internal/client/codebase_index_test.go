package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/example-git/crux/internal/proto"
	"github.com/stretchr/testify/require"
)

func TestCodebaseIndexStatus(t *testing.T) {
	t.Parallel()
	want := proto.CodebaseIndexStatus{
		Enabled:          true,
		State:            "indexing",
		ProjectRoot:      "/project",
		DatabasePath:     "/indexes/source.db",
		StoreDirectory:   "/indexes/store",
		SourceMode:       "native",
		CredentialStatus: "signed-in",
		Model:            "model-a",
		IncludePaths:     []string{"src"},
		ExcludePaths:     []string{"src/generated"},
		FilesTotal:       10,
		FilesProcessed:   3,
		ChunksCreated:    24,
		FilesSkipped:     1,
		CurrentPath:      "src/main.go",
		Stage:            "Indexing files",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/v1/workspaces/workspace-1/codebase-index", r.URL.Path)
		require.NoError(t, json.NewEncoder(w).Encode(want))
	}))
	defer srv.Close()

	got, err := captureClient(t, srv).CodebaseIndexStatus(t.Context(), "workspace-1")
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestUpdateCodebaseIndex(t *testing.T) {
	t.Parallel()
	update := proto.CodebaseIndexUpdate{
		Enabled:        false,
		Reindex:        true,
		DatabasePath:   "/indexes/source.db",
		StoreDirectory: "/indexes/store",
		IncludePaths:   []string{"internal"},
		ExcludePaths:   []string{"internal/generated"},
	}
	want := proto.CodebaseIndexStatus{
		Enabled:      false,
		State:        "disabled",
		IncludePaths: update.IncludePaths,
		ExcludePaths: update.ExcludePaths,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/v1/workspaces/workspace-1/codebase-index", r.URL.Path)
		require.Equal(t, "application/json", r.Header.Get("Content-Type"))
		var got proto.CodebaseIndexUpdate
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		require.Equal(t, update, got)
		require.NoError(t, json.NewEncoder(w).Encode(want))
	}))
	defer srv.Close()

	got, err := captureClient(t, srv).UpdateCodebaseIndex(t.Context(), "workspace-1", update)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

//nolint:tparallel // Subtests share one transport fixture.
func TestCodebaseIndexStatusErrors(t *testing.T) {
	t.Parallel()
	t.Run("server error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		_, err := captureClient(t, srv).CodebaseIndexStatus(t.Context(), "workspace-1")
		require.ErrorContains(t, err, "status code 500")
	})
	t.Run("malformed response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("not json"))
		}))
		defer srv.Close()

		_, err := captureClient(t, srv).CodebaseIndexStatus(t.Context(), "workspace-1")
		require.ErrorContains(t, err, "failed to decode codebase index status")
	})
}
