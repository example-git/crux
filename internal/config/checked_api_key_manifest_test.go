package config

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func newCheckedManifestFixture(t *testing.T, endpoint string, edit func(*manifest.Manifest)) checkedAPIKeyFixture {
	t.Helper()
	root := t.TempDir()
	bundle := filepath.Join(root, "native.plugin")
	require.NoError(t, os.CopyFS(bundle, os.DirFS("../../docs/provider-plugins/examples/responses-oauth.plugin")))
	data, err := os.ReadFile(filepath.Join(bundle, "manifest.json"))
	require.NoError(t, err)
	var declaration manifest.Manifest
	require.NoError(t, json.Unmarshal(data, &declaration))
	declaration.Compatibility.RequiredFeatures = append(declaration.Compatibility.RequiredFeatures, "operation.model-catalog-http")
	declaration.Capabilities.OAuth = nil
	declaration.Capabilities.Credentials = []manifest.Credential{{ID: "key", Kind: "api-key", Audience: []string{"api"}}}
	target, err := url.Parse(endpoint)
	require.NoError(t, err)
	for i := range declaration.Capabilities.Endpoints {
		if declaration.Capabilities.Endpoints[i].ID == "api" {
			declaration.Capabilities.Endpoints[i] = manifest.Endpoint{ID: "api", BaseURL: endpoint, AllowedSchemes: []string{target.Scheme}, AllowedHosts: []string{target.Hostname()}, Override: "same-origin", Credential: "key"}
		}
	}
	declaration.Capabilities.Operations = append(declaration.Capabilities.Operations, manifest.Operation{
		ID: "catalog", Kind: "model-catalog", Protocol: "generic-json", Transport: "http-json", Endpoint: "api", Method: "GET", Path: "/declared/catalog",
		Headers: []manifest.HeaderRule{{Operation: "set", Name: "X-Declared-Key", Value: &manifest.Template{Kind: "credential", Ref: "key"}}},
	})
	if edit != nil {
		edit(&declaration)
	}
	data, err = json.Marshal(declaration)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(bundle, "manifest.json"), data, 0600))
	values := map[string]string{"HOME": root, "USERPROFILE": root, "AI_CLI_DIR": filepath.Join(root, "accounts"), "CRUX_GLOBAL_CONFIG": filepath.Join(root, "config"), "CRUX_GLOBAL_DATA": filepath.Join(root, "data"), "CRUX_CACHE_DIR": filepath.Join(root, "cache"), "CRUX_PROVIDER_PROFILE": string(ProviderProfilePluginNative), "CRUX_PROVIDER_PLUGINS": "example-responses"}
	installTrustedProviderBundle(t, values["CRUX_GLOBAL_DATA"], values["CRUX_CACHE_DIR"], bundle)
	require.NoError(t, os.MkdirAll(values["CRUX_GLOBAL_CONFIG"], 0700))
	document := fmt.Sprintf(`{"providers":{"example-responses":{"plugin":{"id":"example.responses-oauth"},"base_url":%q,"extra_headers":{"X-Configured":"kept","X-Declared-Key":"shadowed"},"configuration":{"oauth_client_id":"synthetic-client"}}},"models":{"large":{"provider":"example-responses","model":"example-reasoner"},"small":{"provider":"example-responses","model":"example-small"}}}`, endpoint)
	require.NoError(t, os.WriteFile(filepath.Join(values["CRUX_GLOBAL_CONFIG"], "crux.json"), []byte(document), 0600))
	path := filepath.Join(values["CRUX_GLOBAL_DATA"], "crux.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"providers":{"example-responses":{"api_key":"synthetic-old"}}}`), 0600))
	store, err := LoadIsolated(root, filepath.Join(root, "workspace"), false, env.NewFromMap(values))
	require.NoError(t, err)
	owner, ok := store.RuntimeSnapshot().ProviderOwner("example-responses")
	require.True(t, ok)
	return checkedAPIKeyFixture{store: store, owner: owner, root: root, path: path}
}

func TestCheckedAPIKeyManifestCheckSave(t *testing.T) {
	var calls atomic.Int32
	var received string
	host := checkedAPIKeyHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.Equal(t, "/declared/catalog", r.URL.Path)
		require.Equal(t, "GET", r.Method)
		require.Equal(t, "kept", r.Header.Get("X-Configured"))
		require.Empty(t, r.Header.Get("Authorization"), "no implicit generic bearer header")
		received = r.Header.Get("X-Declared-Key")
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	f := newCheckedManifestFixture(t, host.URL, nil)
	counter, forbidden := filepath.Join(f.root, "counter"), filepath.Join(f.root, "must-not-run")
	literal := "synthetic-$(touch " + forbidden + ")-$UNRESOLVED"
	source := fmt.Sprintf("$(printf x >> '%s'; printf '%%s' '%s')", counter, literal)
	before := f.capture(t)
	prepared, err := f.store.PrepareCheckedAPIKey(t.Context(), before, f.owner, "provider.api_key", source)
	require.NoError(t, err)
	require.Equal(t, ConnectionProbeResult{Kind: ConnectionProbeHTTPResponse, Policy: ConnectionProbePolicyManifestHTTP200, HTTPStatus: 200}, prepared.ProbeResult())
	require.NoError(t, prepared.ProbeResult().Validate())
	require.Equal(t, literal, received)
	result, err := f.store.SaveCheckedAPIKey(t.Context(), ScopeGlobal, prepared)
	require.NoError(t, err)
	require.True(t, result.ConfigSaved)
	require.True(t, result.RuntimePublished)
	require.False(t, result.AccountsSaved)
	require.EqualValues(t, 1, calls.Load())
	data, err := os.ReadFile(counter)
	require.NoError(t, err)
	require.Equal(t, "x", string(data))
	require.NoFileExists(t, forbidden)
	data, err = os.ReadFile(f.path)
	require.NoError(t, err)
	require.Equal(t, source, gjson.GetBytes(data, "providers.example-responses.api_key").String())
	provider, ok := result.After.runtime.config.Providers.Get("example-responses")
	require.True(t, ok)
	actual, err := result.After.runtime.ResolveProviderAPIKey(provider)
	require.NoError(t, err)
	require.Equal(t, literal, actual)
	proposal, err := f.store.CollectRemoteRuntimeForAuthentication(t.Context(), result.After, 1, nil)
	require.NoError(t, err)
	require.Equal(t, literal, proposal.Credentials[0].APIKey)
	require.Equal(t, before.runtime.config.Models, result.After.runtime.config.Models)
	require.True(t, before.accounts.SameObservation(result.After.accounts))
}

func TestCheckedAPIKeyManifestFailuresNeverSave(t *testing.T) {
	for _, mode := range []string{"unauthorized", "invalid JSON", "oversized", "transform failure", "cyclic transformed response", "redirect denied", "ambiguous operation", "missing operation", "forbidden override"} {
		t.Run(mode, func(t *testing.T) {
			var calls, redirected atomic.Int32
			host := checkedAPIKeyHTTP(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path == "/redirected" {
					redirected.Add(1)
				}
				switch mode {
				case "unauthorized":
					w.WriteHeader(401)
				case "cyclic transformed response":
					_, _ = w.Write([]byte(`{"node":{}}`))
				case "invalid JSON":
					_, _ = w.Write([]byte("not JSON"))
				case "oversized":
					_, _ = w.Write(make([]byte, (1<<20)+1))
				case "redirect denied":
					http.Redirect(w, r, "/redirected", 307)
				default:
					_, _ = w.Write([]byte(`{}`))
				}
			})
			f := newCheckedManifestFixture(t, host.URL, func(m *manifest.Manifest) {
				catalog := &m.Capabilities.Operations[len(m.Capabilities.Operations)-1]
				switch mode {
				case "ambiguous operation":
					other := *catalog
					other.ID = "other-catalog"
					m.Capabilities.Operations = append(m.Capabilities.Operations, other)
				case "missing operation":
					m.Capabilities.Operations = m.Capabilities.Operations[:len(m.Capabilities.Operations)-1]
				case "forbidden override":
					for i := range m.Capabilities.Endpoints {
						if m.Capabilities.Endpoints[i].ID == "api" {
							m.Capabilities.Endpoints[i].Override = "forbidden"
							m.Capabilities.Endpoints[i].BaseURL += "/declared-base"
						}
					}
				case "cyclic transformed response":
					m.Capabilities.JSONTransforms["cycle"] = manifest.JSONPipeline{MaxOperations: 1, Operations: []manifest.JSONOperation{{Operation: "copy", From: "/node", Path: "/node/self"}}}
					catalog.ResponseTransform = "cycle"
				case "transform failure":
					m.Capabilities.JSONTransforms["check-response"] = manifest.JSONPipeline{MaxOperations: 1, Operations: []manifest.JSONOperation{{Operation: "copy", From: "/missing", Path: "/must-exist"}}}
					catalog.ResponseTransform = "check-response"
				}
			})
			beforeBytes, err := os.ReadFile(f.path)
			require.NoError(t, err)
			counter := filepath.Join(f.root, "source-counter")
			prepared, err := f.store.PrepareCheckedAPIKey(t.Context(), f.capture(t), f.owner, "provider.api_key", fmt.Sprintf("$(printf x >> '%s'; printf synthetic-new)", counter))
			require.Error(t, err)
			require.NoError(t, prepared.ProbeResult().Validate())
			_, err = f.store.SaveCheckedAPIKey(t.Context(), ScopeGlobal, prepared)
			require.Error(t, err)
			afterBytes, err := os.ReadFile(f.path)
			require.NoError(t, err)
			require.Equal(t, beforeBytes, afterBytes)
			require.Zero(t, redirected.Load())
			if mode == "ambiguous operation" || mode == "missing operation" {
				require.Zero(t, calls.Load())
				require.NoFileExists(t, counter)
			}
			if mode == "forbidden override" {
				require.Zero(t, calls.Load())
			}
		})
	}
}

func TestCheckedAPIKeyManifestDeclaredBindingAndRetry(t *testing.T) {
	var calls atomic.Int32
	host := checkedAPIKeyHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		require.Equal(t, "POST", r.Method)
		require.Equal(t, "/declared/catalog", r.URL.Path)
		require.Equal(t, "native-fixture/1.2.3", r.Header.Get("User-Agent"))
		require.Equal(t, "Bearer synthetic-new", r.Header.Get("Authorization"))
		require.Equal(t, "synthetic-client", r.Header.Get("X-Config"))
		require.Equal(t, "application/vnd.catalog+json", r.Header.Get("Accept"))
		require.Equal(t, "application/vnd.catalog-request+json", r.Header.Get("Content-Type"))
		data, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.JSONEq(t, `{"key":"synthetic-new","configuration":"synthetic-client"}`, string(data))
		if n == 1 {
			w.WriteHeader(503)
			return
		}
		_, _ = w.Write([]byte(`{"required":9007199254740993}`))
	})
	f := newCheckedManifestFixture(t, host.URL, func(m *manifest.Manifest) {
		m.Capabilities.ClientIdentities = map[string]manifest.ResolvedClientIdentity{"catalog": {Environment: "SYNTHETIC_CATALOG_VERSION", CacheKey: "native-catalog", FallbackVersion: "1.2.3", VersionPattern: `^[0-9]+\.[0-9]+\.[0-9]+$`, UserAgentFormat: "native-fixture/{version}", ProbeTimeoutMS: 1000, ProbeMaxBytes: 1024}}
		m.Capabilities.JSONTransforms["catalog-request"] = manifest.JSONPipeline{MaxOperations: 2, Operations: []manifest.JSONOperation{{Operation: "set", Path: "/key", Value: &manifest.Template{Kind: "credential", Ref: "key"}}, {Operation: "set", Path: "/configuration", Value: &manifest.Template{Kind: "config", Ref: "oauth_client_id"}}}}
		m.Capabilities.JSONTransforms["catalog-response"] = manifest.JSONPipeline{MaxOperations: 1, Operations: []manifest.JSONOperation{{Operation: "copy", From: "/required", Path: "/observed"}}}
		catalog := &m.Capabilities.Operations[len(m.Capabilities.Operations)-1]
		catalog.Method = "POST"
		catalog.ClientIdentity = "catalog"
		catalog.RequestTransform = "catalog-request"
		catalog.ResponseTransform = "catalog-response"
		catalog.Headers = append(catalog.Headers, manifest.HeaderRule{Operation: "set", Name: "Authorization", Value: &manifest.Template{Kind: "concat", Parts: []manifest.Template{{Kind: "literal", Value: "Bearer "}, {Kind: "credential", Ref: "key"}}}}, manifest.HeaderRule{Operation: "set", Name: "User-Agent", Value: &manifest.Template{Kind: "context", Ref: "client.user_agent"}}, manifest.HeaderRule{Operation: "set", Name: "X-Config", Value: &manifest.Template{Kind: "config", Ref: "oauth_client_id"}})
		catalog.Retry = &manifest.RetryPolicy{MaxAttempts: 2, Authentication: "never", ReplayRequirement: "idempotent", Statuses: []int{503}}
	})
	require.NoError(t, f.store.SetConfigField(ScopeGlobal, "providers.example-responses.extra_headers.accept", "application/vnd.catalog+json"))
	require.NoError(t, f.store.SetConfigField(ScopeGlobal, "providers.example-responses.extra_headers.content-type", "application/vnd.catalog-request+json"))
	t.Setenv("SYNTHETIC_CATALOG_VERSION", "9.9.9") // captured environment wins
	prepared, err := f.store.PrepareCheckedAPIKey(t.Context(), f.capture(t), f.owner, "provider.api_key", "synthetic-new")
	require.NoError(t, err)
	require.EqualValues(t, 2, calls.Load())
	require.True(t, prepared.ProbeResult().EnteredKeyInAuthorization)
	require.False(t, prepared.ProbeResult().AuthorizationOverridden)
}

func TestCheckedAPIKeyManifestAudienceAndAuthorization(t *testing.T) {
	for _, mode := range []string{"configured override", "equal literal override", "delete then append", "context without audience", "credential without audience", "config credential without audience"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			host := checkedAPIKeyHTTP(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				require.Equal(t, "Bearer synthetic-new", r.Header.Get("Authorization"))
				_, _ = w.Write([]byte(`{}`))
			})
			f := newCheckedManifestFixture(t, host.URL, func(m *manifest.Manifest) {
				catalog := &m.Capabilities.Operations[len(m.Capabilities.Operations)-1]
				key := manifest.Template{Kind: "credential", Ref: "key"}
				if mode == "config credential without audience" {
					m.Capabilities.Credentials = append(m.Capabilities.Credentials, manifest.Credential{ID: "secret", Kind: "api-key", ConfigProperty: "oauth_client_id", Audience: []string{"token"}})
					key = manifest.Template{Kind: "config", Ref: "oauth_client_id"}
				} else if strings.Contains(mode, "without audience") {
					m.Capabilities.Credentials[0].Audience = []string{"token"}
					m.Capabilities.Endpoints[2].Credential = ""
					if strings.HasPrefix(mode, "context") {
						key = manifest.Template{Kind: "context", Ref: "oauth.access_token"}
					}
				}
				catalog.Headers = []manifest.HeaderRule{{Operation: "set", Name: "Authorization", Value: &manifest.Template{Kind: "concat", Parts: []manifest.Template{{Kind: "literal", Value: "Bearer "}, key}}}}
				if mode == "equal literal override" {
					catalog.Headers = append(catalog.Headers, manifest.HeaderRule{Operation: "set", Name: "Authorization", Value: &manifest.Template{Kind: "literal", Value: "Bearer synthetic-new"}})
				}
				if mode == "configured override" {
					catalog.Headers[0].Operation = "set-if-absent"
				}
				if mode == "delete then append" {
					catalog.Headers[0].Operation = "append"
					catalog.Headers = append([]manifest.HeaderRule{{Operation: "delete", Name: "Authorization"}}, catalog.Headers...)
				}
			})
			if mode == "configured override" {
				require.NoError(t, f.store.SetConfigField(ScopeGlobal, "providers.example-responses.extra_headers.Authorization", "Bearer synthetic-new"))
			}
			prepared, err := f.store.PrepareCheckedAPIKey(t.Context(), f.capture(t), f.owner, "provider.api_key", "synthetic-new")
			if strings.Contains(mode, "without audience") {
				require.Error(t, err)
				require.Zero(t, calls.Load())
			} else if mode == "delete then append" {
				require.NoError(t, err)
				require.True(t, prepared.ProbeResult().EnteredKeyInAuthorization)
				require.False(t, prepared.ProbeResult().AuthorizationOverridden)
			} else {
				require.NoError(t, err)
				require.True(t, prepared.ProbeResult().AuthorizationOverridden)
				require.False(t, prepared.ProbeResult().EnteredKeyInAuthorization)
			}
		})
	}
}

func TestCheckedAPIKeyManifestCanceledOrChangedBeforeAcknowledgement(t *testing.T) {
	for _, mode := range []string{"cancel", "owner changed", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			host := checkedAPIKeyHTTP(t, func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				select {
				case <-release:
					_, _ = w.Write([]byte(`{}`))
				case <-r.Context().Done():
				}
			})
			f := newCheckedManifestFixture(t, host.URL, func(m *manifest.Manifest) {
				if mode == "timeout" {
					m.Capabilities.Operations[len(m.Capabilities.Operations)-1].Timeouts = &manifest.TimeoutHints{RequestSeconds: 1}
				}
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			type answer struct {
				p   CheckedAPIKeyPreparation
				err error
			}
			done := make(chan answer, 1)
			before := f.capture(t)
			go func() {
				p, err := f.store.PrepareCheckedAPIKey(ctx, before, f.owner, "provider.api_key", "synthetic-new")
				done <- answer{p, err}
			}()
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				t.Fatal("catalog request never arrived")
			}
			if mode == "cancel" {
				cancel()
			}
			if mode == "owner changed" {
				require.NoError(t, f.store.SetConfigField(ScopeGlobal, "providers.example-responses.disable", true))
				close(release)
			}
			select {
			case result := <-done:
				require.Error(t, result.err)
				require.NoError(t, result.p.ProbeResult().Validate())
				_, err := f.store.SaveCheckedAPIKey(t.Context(), ScopeGlobal, result.p)
				require.Error(t, err)
			case <-time.After(10 * time.Second):
				t.Fatal("check did not finish")
			}
			if mode != "owner changed" {
				close(release)
			}
		})
	}
}

func TestCheckedAPIKeyManifestAllowedAndForbiddenRedirects(t *testing.T) {
	for _, mode := range []string{"declared same host", "undeclared host"} {
		t.Run(mode, func(t *testing.T) {
			var final atomic.Int32
			host := checkedAPIKeyHTTP(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/declared/catalog" {
					destination := "/final"
					if mode == "undeclared host" {
						destination = "https://localhost:" + strings.Split(r.Host, ":")[1] + "/final"
					}
					http.Redirect(w, r, destination, 307)
					return
				}
				final.Add(1)
				require.Equal(t, "synthetic-new", r.Header.Get("X-Declared-Key"))
				_, _ = w.Write([]byte(`{}`))
			})
			f := newCheckedManifestFixture(t, host.URL, func(m *manifest.Manifest) {
				for i := range m.Capabilities.Endpoints {
					if m.Capabilities.Endpoints[i].ID == "api" {
						m.Capabilities.Endpoints[i].FollowRedirects = true
					}
				}
			})
			var forbiddenHops atomic.Int32
			original := http.DefaultClient.Transport
			http.DefaultClient.Transport = checkedCatalogTransportFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Hostname() == "localhost" {
					forbiddenHops.Add(1)
				}
				return original.RoundTrip(r)
			})
			defer func() { http.DefaultClient.Transport = original }()
			prepared, err := f.store.PrepareCheckedAPIKey(t.Context(), f.capture(t), f.owner, "provider.api_key", "synthetic-new")
			require.NoError(t, prepared.ProbeResult().Validate())
			if mode == "declared same host" {
				require.NoError(t, err)
				require.EqualValues(t, 1, final.Load())
			} else {
				require.Error(t, err)
				require.Zero(t, final.Load())
				require.Equal(t, 307, prepared.ProbeResult().HTTPStatus)
				require.Zero(t, forbiddenHops.Load(), "denied redirect must stop before transport, independent of TLS")
			}
		})
	}
}

type checkedCatalogTransportFunc func(*http.Request) (*http.Response, error)

func (f checkedCatalogTransportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCheckedAPIKeyManifestRetryCancellationRetainsResponseEvidence(t *testing.T) {
	var calls atomic.Int32
	host := checkedAPIKeyHTTP(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(503) })
	f := newCheckedManifestFixture(t, host.URL, func(m *manifest.Manifest) {
		m.Capabilities.Operations[len(m.Capabilities.Operations)-1].Retry = &manifest.RetryPolicy{MaxAttempts: 2, InitialDelayMS: 5000, Authentication: "never", ReplayRequirement: "idempotent", Statuses: []int{503}}
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	original := http.DefaultClient.Transport
	http.DefaultClient.Transport = checkedCatalogTransportFunc(func(r *http.Request) (*http.Response, error) {
		response, err := original.RoundTrip(r)
		cancel()
		return response, err
	})
	defer func() { http.DefaultClient.Transport = original }()
	prepared, err := f.store.PrepareCheckedAPIKey(ctx, f.capture(t), f.owner, "provider.api_key", "synthetic-new")
	require.ErrorIs(t, err, context.Canceled)
	require.EqualValues(t, 1, calls.Load())
	require.Equal(t, ConnectionProbeHTTPResponse, prepared.ProbeResult().Kind)
	require.Equal(t, 503, prepared.ProbeResult().HTTPStatus)
	require.NoError(t, prepared.ProbeResult().Validate())
}

func TestCheckedAPIKeyManifestTransformsCannotMutateSavedConfiguration(t *testing.T) {
	type observation struct{ body, header string }
	observed := make(chan observation, 1)
	host := checkedAPIKeyHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		observed <- observation{string(data), r.Header.Get("X-Nested")}
		_, _ = w.Write([]byte(`{}`))
	})
	f := newCheckedManifestFixture(t, host.URL, func(m *manifest.Manifest) {
		m.Configuration.Schema["properties"].(map[string]any)["nested"] = map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string"}}, "additionalProperties": false}
		for _, phase := range []string{"request", "response"} {
			m.Capabilities.JSONTransforms["catalog-"+phase] = manifest.JSONPipeline{MaxOperations: 2, Operations: []manifest.JSONOperation{{Operation: "set", Path: "/nested", Value: &manifest.Template{Kind: "config", Ref: "nested"}}, {Operation: "set", Path: "/nested/value", Value: &manifest.Template{Kind: "literal", Value: phase}}}}
		}
		catalog := &m.Capabilities.Operations[len(m.Capabilities.Operations)-1]
		catalog.Method = "POST"
		catalog.RequestTransform = "catalog-request"
		catalog.ResponseTransform = "catalog-response"
		catalog.Headers = append(catalog.Headers, manifest.HeaderRule{Operation: "set", Name: "X-Nested", Value: &manifest.Template{Kind: "config", Ref: "nested"}})
	})
	require.NoError(t, f.store.SetConfigField(ScopeGlobal, "providers.example-responses.configuration.nested", map[string]any{"value": "original"}))
	before := f.capture(t)
	prepared, err := f.store.PrepareCheckedAPIKey(t.Context(), before, f.owner, "provider.api_key", "synthetic-new")
	require.NoError(t, err)
	saved, err := f.store.SaveCheckedAPIKey(t.Context(), ScopeGlobal, prepared)
	require.NoError(t, err)
	provider, ok := saved.After.runtime.config.Providers.Get(f.owner.ProviderID)
	require.True(t, ok)
	require.Equal(t, map[string]any{"value": "original"}, provider.Configuration["nested"], "request/response transformations must not rewrite the provider definition installed by Save")
	request := <-observed
	require.JSONEq(t, `{"nested":{"value":"request"}}`, request.body)
	require.JSONEq(t, `{"value":"original"}`, request.header, "headers read the captured definition, not request-transform mutations")
	disk, err := os.ReadFile(f.path)
	require.NoError(t, err)
	require.Equal(t, "original", gjson.GetBytes(disk, "providers.example-responses.configuration.nested.value").String())
}

func TestCheckedAPIKeyManifestDeadlineIncludesQueuedOwnerCheck(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	host := checkedAPIKeyHTTP(t, func(w http.ResponseWriter, r *http.Request) { close(entered); <-release; _, _ = w.Write([]byte(`{}`)) })
	f := newCheckedManifestFixture(t, host.URL, nil)
	before := f.capture(t)
	done := make(chan error, 1)
	go func() {
		_, err := f.store.PrepareCheckedAPIKey(t.Context(), before, f.owner, "provider.api_key", "synthetic-new")
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("catalog did not start")
	}
	f.store.writeMu.Lock()
	defer f.store.writeMu.Unlock()
	close(release)
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(8 * time.Second):
		t.Fatal("owner recheck exceeded the complete catalog probe deadline while waiting for a writer")
	}
}
