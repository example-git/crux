package client

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/example-git/crux/internal/providerauth"
	"github.com/stretchr/testify/require"
)

func TestProviderAccountRemovalSDKNeverRedirects(t *testing.T) {
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1); w.WriteHeader(400) }))
	defer destination.Close()
	request := providerauth.RemoveRequest{OperationID: strings.Repeat("a", 32), Target: providerauth.Target{WorkspaceID: "workspace", Generation: providerauth.Generation{Epoch: strings.Repeat("b", 32), Sequence: 1}, Owner: providerauth.Owner{ProviderID: "fixture", HasOAuth: true}}, AccountID: "exact-account"}
	for _, status := range []int{307, 308} {
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, "/v1/workspaces/workspace/auth/remove", r.URL.Path)
			http.Redirect(w, r, destination.URL, status)
		}))
		sdk := captureClient(t, origin)
		result, err := sdk.RemoveProviderAccount(t.Context(), "workspace", request)
		require.Error(t, err)
		require.Zero(t, result)
		require.Nil(t, sdk.h.CheckRedirect)
		origin.Close()
	}
	require.Zero(t, redirected.Load())
}
