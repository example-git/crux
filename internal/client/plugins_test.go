package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/example-git/crux/internal/proto"
	"github.com/stretchr/testify/require"
)

func TestPluginSnapshot(t *testing.T) {
	t.Parallel()
	want := proto.PluginSnapshot{
		Revision:  7,
		ScannedAt: time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC),
		Plugins: []proto.PluginStatus{{
			BundleName:    "example.echo.plugin",
			ID:            "example.echo",
			ProviderID:    "example-echo",
			Version:       "1.0.0",
			Digest:        "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			State:         "untrusted",
			Trust:         "unknown",
			Compatibility: "compatible",
			Capabilities:  []string{"endpoint:api", "operation:generate"},
		}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/v1/plugins", r.URL.Path)
		require.NoError(t, json.NewEncoder(w).Encode(want))
	}))
	defer srv.Close()

	got, err := captureClient(t, srv).PluginSnapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, want, got)
}

//nolint:tparallel // Subtests share one transport fixture.
func TestPluginSnapshotErrors(t *testing.T) {
	t.Parallel()
	t.Run("server error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		_, err := captureClient(t, srv).PluginSnapshot(t.Context())
		require.ErrorContains(t, err, "status code 500")
	})
	t.Run("malformed response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("not json"))
		}))
		defer srv.Close()

		_, err := captureClient(t, srv).PluginSnapshot(t.Context())
		require.ErrorContains(t, err, "failed to decode provider plugin status")
	})
}
