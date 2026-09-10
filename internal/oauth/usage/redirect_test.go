package usage

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

func TestNormalizedUsageRedirectsKeepCapturedCredentialDestinations(t *testing.T) {
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
					assert.Equal(t, "Bearer synthetic-access", r.Header.Get("Authorization"))
					assert.Equal(t, "synthetic-private", r.Header.Get("X-Private"))
					body, readErr := io.ReadAll(r.Body)
					assert.NoError(t, readErr)
					assert.JSONEq(t, `{"credential":"synthetic-access"}`, string(body))
					_, _ = io.WriteString(w, `{"used":25}`)
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
					assert.Equal(t, "Bearer synthetic-access", r.Header.Get("Authorization"))
					assert.Equal(t, "synthetic-private", r.Header.Get("X-Private"))
					body, readErr := io.ReadAll(r.Body)
					assert.NoError(t, readErr)
					assert.JSONEq(t, `{"credential":"synthetic-access"}`, string(body))
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
				previous := http.DefaultTransport
				http.DefaultTransport = server.Client().Transport
				defer func() { http.DefaultTransport = previous }()
				endpoint.Credential = "key"
				operation := &providertransport.Operation{
					ID: "quota", Endpoint: endpoint, Method: http.MethodPost, Path: "/start", RequestTimeout: time.Second,
					Headers:          []manifest.HeaderRule{{Operation: "set", Name: "X-Private", Value: &manifest.Template{Kind: "literal", Value: "synthetic-private"}}},
					RequestTransform: &manifest.JSONPipeline{MaxOperations: 1, Operations: []manifest.JSONOperation{{Operation: "set", Path: "/credential", Value: &manifest.Template{Kind: "credential", Ref: "key"}}}},
				}
				fetch, err := ManifestFetcher(map[string]*providertransport.Operation{"quota": operation}, manifest.UsagePolicy{Operation: "quota", Source: "operation", Fallback: "unavailable", Windows: []manifest.WindowMap{{ID: "quota", UsedPointer: "/used"}}})
				require.NoError(t, err)
				result, err := FetchWithTokenForOwner(t.Context(), "synthetic", "synthetic-access", fetch, validate)
				if mode == "relative" || mode == "forbidden-relative" || mode == "allowed-port" {
					require.NoError(t, err)
					require.Len(t, result.Windows, 1)
					require.Equal(t, 25, result.Windows[0].Percent)
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
