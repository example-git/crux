package mcpoauth

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/oauth"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

func trustMCPDestinationServers(t *testing.T, servers ...*httptest.Server) {
	t.Helper()
	previous := http.DefaultTransport
	transport := servers[0].Client().Transport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig.RootCAs = x509.NewCertPool()
	for _, server := range servers {
		transport.TLSClientConfig.RootCAs.AddCert(server.Certificate())
	}
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = previous; transport.CloseIdleConnections() })
}

func writeMCPDestinationJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func TestMCPDiscoveredOAuthCredentialRedirectsHTTPS(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		for _, stage := range []string{"registration", "exchange", "auto-exchange"} {
			for _, external := range []bool{false, true} {
				t.Run(fmt.Sprintf("%d/%s/external=%t", status, stage, external), func(t *testing.T) {
					ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
					defer cancel()
					var foreign, registrations, exchanges, tokenRequests atomic.Int32
					var denyRefresh atomic.Bool
					failures := make(chan string, 16)
					sink := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { foreign.Add(1); w.WriteHeader(http.StatusNoContent) }))
					defer sink.Close()
					var credentialURL string
					credential := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						switch r.URL.Path {
						case "/register", "/token":
							if r.URL.Path == "/token" {
								tokenRequests.Add(1)
							}
							destination := credentialURL + r.URL.Path + "-final"
							if external && (stage == "registration" && r.URL.Path == "/register" || stage != "registration" && r.URL.Path == "/token") || denyRefresh.Load() && r.URL.Path == "/token" {
								destination = sink.URL + r.URL.Path
							}
							http.Redirect(w, r, destination, status)
						case "/register-final":
							registrations.Add(1)
							var registration map[string]any
							if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&registration) != nil || registration["client_name"] != "Crux" {
								failures <- "registration method or body changed"
							}
							writeMCPDestinationJSON(w, map[string]any{"client_id": "synthetic-client", "client_secret": "synthetic-client-secret", "token_endpoint_auth_method": "client_secret_post"})
						case "/token-final":
							exchanges.Add(1)
							_ = r.ParseForm()
							_, basicSecret, _ := r.BasicAuth()
							if r.Method != http.MethodPost || r.Form.Get("code") != "synthetic-code" || r.Form.Get("code_verifier") == "" || (r.Form.Get("client_secret") != "synthetic-client-secret" && basicSecret != "synthetic-client-secret") {
								failures <- "exchange method, credential or body changed"
							}
							writeMCPDestinationJSON(w, map[string]any{"access_token": "synthetic-access", "refresh_token": "synthetic-refresh", "token_type": "Bearer", "expires_in": 3600})
						default:
							http.NotFound(w, r)
						}
					}))
					defer credential.Close()
					credentialURL = credential.URL
					var identityURL string
					identity := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						methods := []string{"client_secret_post"}
						if stage == "auto-exchange" {
							methods = nil
						}
						writeMCPDestinationJSON(w, map[string]any{"issuer": identityURL, "authorization_endpoint": identityURL + "/authorize", "token_endpoint": credentialURL + "/token", "registration_endpoint": credentialURL + "/register", "code_challenge_methods_supported": []string{"S256"}, "token_endpoint_auth_methods_supported": methods})
					}))
					defer identity.Close()
					identityURL = identity.URL
					var resourceURL string
					resource := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						writeMCPDestinationJSON(w, map[string]any{"resource": resourceURL + "/mcp", "authorization_servers": []string{identityURL}})
					}))
					defer resource.Close()
					resourceURL = resource.URL
					trustMCPDestinationServers(t, sink, credential, identity, resource)
					var saved atomic.Int32
					var preregistered *oauth.OAuthClient
					if stage == "auto-exchange" {
						preregistered = &oauth.OAuthClient{ClientID: "synthetic-client", ClientSecret: "synthetic-client-secret"}
					}
					handler, err := NewHandlerWithContext(ctx, "destination-fixture", resource.URL+"/mcp", nil, preregistered, func(*oauth.Token) { saved.Add(1) }, true, 0)
					require.NoError(t, err)
					defer handler.Close()
					handler.openURL = browserRedirect("synthetic-code")
					err = authorizeWith401(t, handler, resource.URL, resource.URL+"/mcp")
					if external {
						require.ErrorContains(t, err, "provider redirect refused")
						require.Zero(t, saved.Load())
						require.Zero(t, exchanges.Load())
						if stage != "registration" {
							require.EqualValues(t, 1, tokenRequests.Load(), "a refused exchange must not trigger auth-style retry")
						}
						if stage == "registration" {
							require.Zero(t, registrations.Load())
						}
					} else {
						require.NoError(t, err)
						source, err := handler.TokenSource(ctx)
						require.NoError(t, err)
						token, err := source.Token()
						require.NoError(t, err)
						require.Equal(t, "synthetic-access", token.AccessToken)
						if stage == "auto-exchange" {
							require.Zero(t, registrations.Load())
						} else {
							require.EqualValues(t, 1, registrations.Load())
						}
						require.EqualValues(t, 1, exchanges.Load())
						require.EqualValues(t, 1, saved.Load())
						// The source installed after a fresh exchange must retain its token
						// endpoint guard on subsequent refresh, not just during initial OAuth.
						if stage != "registration" {
							before := tokenRequests.Load()
							token.Expiry = time.Now().Add(-time.Hour)
							denyRefresh.Store(true)
							_, err = source.Token()
							require.ErrorContains(t, err, "provider redirect refused")
							require.Equal(t, before+1, tokenRequests.Load(), "new-token refresh must not retry a refused destination")
							require.EqualValues(t, 1, exchanges.Load())
							require.EqualValues(t, 1, saved.Load())
						}
					}
					require.Zero(t, foreign.Load(), "refused endpoint must receive neither credentials nor body")
					select {
					case failure := <-failures:
						t.Fatal(failure)
					default:
					}
				})
			}
		}
	}
}

func TestMCPRestoredRefreshCredentialRedirectsHTTPS(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		for _, style := range []oauth2.AuthStyle{oauth2.AuthStyleAutoDetect, oauth2.AuthStyleInParams} {
			for _, external := range []bool{false, true} {
				t.Run(fmt.Sprintf("%d/style=%d/external=%t", status, style, external), func(t *testing.T) {
					ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
					defer cancel()
					var foreign, initial, completed, saved atomic.Int32
					failures := make(chan string, 8)
					sink := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { foreign.Add(1); w.WriteHeader(http.StatusNoContent) }))
					defer sink.Close()
					var endpointURL string
					endpoint := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/token" {
							initial.Add(1)
							destination := endpointURL + "/token-final"
							if external {
								destination = sink.URL + "/token"
							}
							http.Redirect(w, r, destination, status)
							return
						}
						completed.Add(1)
						_ = r.ParseForm()
						_, secret, basic := r.BasicAuth()
						if r.Method != http.MethodPost || r.Form.Get("refresh_token") != "synthetic-refresh" || secret != "synthetic-secret" && r.Form.Get("client_secret") != "synthetic-secret" {
							failures <- "refresh lost captured credential/body"
						}
						if style == oauth2.AuthStyleAutoDetect && basic {
							// Ordinary auth-style discovery remains legitimate: Basic is rejected,
							// then the same selected endpoint accepts client_secret in the body.
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(http.StatusUnauthorized)
							_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
							return
						}
						writeMCPDestinationJSON(w, map[string]any{"access_token": "rotated-access", "refresh_token": "rotated-refresh", "token_type": "Bearer", "expires_in": 3600})
					}))
					defer endpoint.Close()
					endpointURL = endpoint.URL
					trustMCPDestinationServers(t, sink, endpoint)
					token := &oauth.Token{AccessToken: "expired", RefreshToken: "synthetic-refresh", ExpiresAt: time.Now().Add(-time.Hour).Unix(), Client: &oauth.OAuthClient{ClientID: "synthetic-client", ClientSecret: "synthetic-secret", TokenURL: endpoint.URL + "/token", AuthStyle: int(style)}}
					handler, err := NewHandlerWithContext(ctx, "restored", "https://separate-resource.invalid/mcp", token, nil, func(*oauth.Token) { saved.Add(1) }, false, 40704)
					require.NoError(t, err)
					defer handler.Close()
					source, err := handler.TokenSource(ctx)
					require.NoError(t, err)
					result, err := source.Token()
					if external {
						require.ErrorContains(t, err, "provider redirect refused")
						require.Nil(t, result)
						require.EqualValues(t, 1, initial.Load(), "redirect refusal must not trigger automatic auth-style retry")
						require.Zero(t, completed.Load())
						require.Zero(t, saved.Load())
						_, err = source.Token() // A separate explicit attempt is not permanently poisoned.
						require.ErrorContains(t, err, "provider redirect refused")
						require.EqualValues(t, 2, initial.Load())
					} else {
						require.NoError(t, err)
						require.Equal(t, "rotated-access", result.AccessToken)
						want := int32(1)
						if style == oauth2.AuthStyleAutoDetect {
							want = 2
						}
						require.Equal(t, want, initial.Load())
						require.Equal(t, want, completed.Load())
						require.EqualValues(t, 1, saved.Load())
					}
					require.Zero(t, foreign.Load())
					select {
					case failure := <-failures:
						t.Fatal(failure)
					default:
					}
				})
			}
		}
	}
}

func TestMCPOAuthMetadataRepairDoesNotExemptCredentials(t *testing.T) {
	var foreign atomic.Int32
	sink := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { foreign.Add(1); w.WriteHeader(http.StatusNoContent) }))
	defer sink.Close()
	var endpointURL string
	endpoint := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/repaired") {
			writeMCPDestinationJSON(w, map[string]any{"issuer": endpointURL + "/"})
			return
		}
		http.Redirect(w, r, sink.URL+"/.well-known/oauth-authorization-server/repaired", http.StatusTemporaryRedirect)
	}))
	defer endpoint.Close()
	endpointURL = endpoint.URL
	trustMCPDestinationServers(t, sink, endpoint)
	client := newOAuthMetadataClient(NewSessionHTTPClient(t.Context()).Transport)
	for _, kind := range []string{"metadata", "token-body", "authorization-header"} {
		t.Run(kind, func(t *testing.T) {
			method := http.MethodGet
			var body *strings.Reader
			if kind == "token-body" {
				method = http.MethodPost
				body = strings.NewReader("refresh_token=synthetic-private")
			} else {
				body = strings.NewReader("")
			}
			request, err := http.NewRequestWithContext(t.Context(), method, endpoint.URL+"/.well-known/oauth-authorization-server", body)
			require.NoError(t, err)
			if kind == "authorization-header" {
				request.Header.Set("Authorization", "Bearer synthetic-private")
			}
			response, err := client.Do(request)
			if kind == "metadata" {
				require.NoError(t, err)
				defer response.Body.Close()
				var value map[string]any
				require.NoError(t, json.NewDecoder(response.Body).Decode(&value))
				require.Equal(t, endpointURL, value["issuer"])
			} else {
				require.ErrorContains(t, err, "provider redirect refused")
				if response != nil {
					response.Body.Close()
				}
			}
			require.Zero(t, foreign.Load())
		})
	}
}
