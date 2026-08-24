package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/example-git/crux/internal/proto"
	"github.com/stretchr/testify/require"
)

func TestListProjects(t *testing.T) {
	t.Parallel()
	want := []proto.ProjectInfo{{Slug: "durable", Name: "Durable", Status: "active", Selected: true, Completed: 2, Total: 5}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		require.Equal(t, http.MethodGet, request.Method)
		require.Equal(t, "/v1/workspaces/workspace-1/projects", request.URL.Path)
		require.NoError(t, json.NewEncoder(w).Encode(want))
	}))
	defer server.Close()

	got, err := captureClient(t, server).ListProjects(t.Context(), "workspace-1")
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestSelectAndDisableProject(t *testing.T) {
	t.Parallel()
	var received []proto.ProjectSelectionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		require.Equal(t, http.MethodPost, request.Method)
		require.Equal(t, "/v1/workspaces/workspace-1/projects/selection", request.URL.Path)
		require.Equal(t, "application/json", request.Header.Get("Content-Type"))
		var body proto.ProjectSelectionRequest
		require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
		received = append(received, body)
	}))
	defer server.Close()
	client := captureClient(t, server)

	require.NoError(t, client.SelectProject(t.Context(), "workspace-1", "durable"))
	require.NoError(t, client.SelectProject(t.Context(), "workspace-1", ""))
	require.Equal(t, []proto.ProjectSelectionRequest{{Slug: "durable"}, {Slug: ""}}, received)
}

func TestProjectClientErrors(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet {
			_, _ = w.Write([]byte("not json"))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()
	client := captureClient(t, server)

	_, err := client.ListProjects(t.Context(), "workspace-1")
	require.ErrorContains(t, err, "decoding projects")
	err = client.SelectProject(t.Context(), "workspace-1", "missing")
	require.ErrorContains(t, err, "status code 400")
}
