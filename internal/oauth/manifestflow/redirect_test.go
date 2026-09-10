package manifestflow

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providertransport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOAuthRefreshRedirectsKeepCapturedCredentialDestinations(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		for _, mode := range []string{"relative", "forbidden-relative", "forbidden-port", "other-port", "other-host", "downgrade", "disabled", "allowed-port", "owner-changed", "loop"} {
			t.Run(fmt.Sprintf("%d/%s", status, mode), func(t *testing.T) {
				var starts, reached atomic.Int32
				var active atomic.Bool
				active.Store(true)
				validate := func() error {
					if !active.Load() {
						return errors.New("selected owner changed")
					}
					return nil
				}
				finish := func(w http.ResponseWriter, r *http.Request) {
					reached.Add(1)
					assert.Equal(t, http.MethodPost, r.Method)
					assert.Equal(t, "synthetic-private", r.Header.Get("X-Private"))
					assert.NoError(t, r.ParseForm())
					assert.Equal(t, "synthetic-refresh", r.Form.Get("refresh_token"))
					assert.Equal(t, "example-client", r.Form.Get("client_id"))
					assert.Equal(t, "synthetic-client-secret", r.Form.Get("client_secret"))
					_, _ = io.WriteString(w, `{"access_token":"redirected-access","refresh_token":"redirected-refresh","expires_in":120}`)
				}
				sink := httptest.NewTLSServer(http.HandlerFunc(finish))
				defer sink.Close()
				plain := httptest.NewServer(http.HandlerFunc(finish))
				defer plain.Close()
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/accepted" {
						finish(w, r)
						return
					}
					starts.Add(1)
					assert.Equal(t, http.MethodPost, r.Method)
					assert.Equal(t, "synthetic-private", r.Header.Get("X-Private"))
					assert.NoError(t, r.ParseForm())
					assert.Equal(t, "synthetic-refresh", r.Form.Get("refresh_token"))
					assert.Equal(t, "example-client", r.Form.Get("client_id"))
					assert.Equal(t, "synthetic-client-secret", r.Form.Get("client_secret"))
					target := "/accepted?captured=yes"
					switch mode {
					case "other-port", "forbidden-port", "allowed-port":
						target = sink.URL + "/accepted"
					case "other-host":
						target = strings.Replace(sink.URL, "127.0.0.1", "localhost", 1) + "/accepted"
					case "downgrade":
						target = plain.URL + "/accepted"
					case "owner-changed":
						active.Store(false)
					case "loop":
						target = r.URL.Path
					}
					http.Redirect(w, r, target, status)
				}))
				defer server.Close()
				endpoint := manifest.Endpoint{BaseURL: server.URL, AllowedSchemes: []string{"https"}, AllowedHosts: []string{"127.0.0.1"}, Override: "same-origin", FollowRedirects: mode != "disabled"}
				if mode == "allowed-port" || mode == "other-host" || mode == "downgrade" {
					endpoint.Override = "allowed-hosts"
				}
				if strings.HasPrefix(mode, "forbidden-") {
					endpoint.Override = "forbidden"
				}
				executor, _ := examplePluginFlow(t, server)
				endpoint.BaseURL = server.URL + "/token"
				executor.endpoints["token"] = endpoint
				executor.client.Timeout = time.Second
				executor.flow.ClientSecret = &manifest.Template{Kind: "literal", Value: "synthetic-client-secret"}
				executor.flow.TokenRequest.Headers = append(executor.flow.TokenRequest.Headers, manifest.HeaderRule{Operation: "set", Name: "X-Private", Value: &manifest.Template{Kind: "literal", Value: "synthetic-private"}})
				ctx := providertransport.ContextWithOwnerValidator(t.Context(), validate)
				result, err := executor.Refresh(ctx, "synthetic-refresh")
				if mode == "relative" || mode == "forbidden-relative" || mode == "allowed-port" {
					require.NoError(t, err)
					require.Equal(t, "redirected-access", result.AccessToken)
					require.EqualValues(t, 1, reached.Load())
				} else {
					require.Error(t, err)
					require.Zero(t, reached.Load(), "rejected destination must receive neither credentials nor body")
					if mode == "other-host" || mode == "other-port" || mode == "forbidden-port" || mode == "downgrade" {
						require.ErrorContains(t, err, "provider redirect refused", "a TLS or connection error is not destination-policy evidence")
					}
					if mode == "loop" {
						require.ErrorContains(t, err, "redirect limit")
						require.EqualValues(t, 10, starts.Load())
						return
					}
					if mode == "owner-changed" {
						require.ErrorContains(t, err, "selected owner changed")
					}
				}
				require.EqualValues(t, 1, starts.Load(), "redirect refusal must not replay the original credentialed request")
			})
		}
	}
}
