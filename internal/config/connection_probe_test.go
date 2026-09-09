package config

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func connectionProbeProvider() ProviderConfig {
	return ProviderConfig{ID: "probe-fixture", APIKey: "synthetic-entered", Type: catalog.TypeOpenAICompat,
		Owner: &ProviderOwnerReference{Type: ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat}}
}

func connectionProbePreset(provider *ProviderConfig, id catalog.ProviderID) {
	provider.ID = string(id)
	provider.Owner = providerPresetOwnerReference()
	provider.Preset = &ProviderPresetReference{ID: "fixture.preset", Version: "1.0.0", Digest: "fixture-exact-digest"}
}

func TestConnectionProbeNonNetworkClassifications(t *testing.T) {
	var requests atomic.Int32
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests.Add(1); w.WriteHeader(http.StatusOK) }))
	defer host.Close()
	previous := http.DefaultClient
	http.DefaultClient = host.Client()
	defer func() { http.DefaultClient = previous }()
	for _, test := range []struct {
		name      string
		id        catalog.ProviderID
		key       string
		kind      ConnectionProbeKind
		policy    ConnectionProbePolicy
		errorText string
	}{
		{"minimax", catalog.ProviderMiniMax, "synthetic", ConnectionProbeNotProbed, ConnectionProbePolicyNone, ""},
		{"minimax china", catalog.ProviderMiniMaxChina, "synthetic", ConnectionProbeNotProbed, ConnectionProbePolicyNone, ""},
		{"alibaba format", catalog.ProviderAlibabaSingapore, "sk-synthetic", ConnectionProbeFormatOnly, ConnectionProbePolicySKPrefix, ""},
		{"alibaba rejected format", catalog.ProviderAlibabaSingapore, "wrong-format", ConnectionProbeFormatOnly, ConnectionProbePolicySKPrefix, "invalid API key format"},
		{"unsupported", "", "synthetic", ConnectionProbeUnsupported, ConnectionProbePolicyNone, "unsupported provider type"},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := connectionProbeProvider()
			provider.BaseURL, provider.APIKey = host.URL, test.key
			if test.id != "" {
				connectionProbePreset(&provider, test.id)
			} else {
				provider.Type = catalog.TypeAnthropic
			}
			result, err := provider.ProbeConnection(t.Context(), IdentityResolver(), func() error { return nil })
			require.Equal(t, ConnectionProbeResult{Kind: test.kind, Policy: test.policy}, result)
			require.NoError(t, result.Validate())
			if test.errorText == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, test.errorText)
			}
			compatibility := provider.TestConnection(t.Context(), IdentityResolver(), func() error { return nil })
			if test.errorText == "" {
				require.NoError(t, compatibility)
			} else {
				require.ErrorContains(t, compatibility, test.errorText)
			}
			require.Zero(t, requests.Load(), "no-probe/format/unsupported paths must not fall back to network")
		})
	}
}

func TestConnectionProbeHTTPSPolicyAndHeaderEvidence(t *testing.T) {
	for _, test := range []struct {
		name        string
		preset      catalog.ProviderID
		status      int
		override    string
		overrideSet bool
		wantError   bool
	}{
		{name: "ordinary models200", status: 200},
		{name: "ordinary models403", status: 403, wantError: true},
		{name: "opencode go models200", preset: catalog.ProviderOpenCodeGo, status: 200},
		{name: "zai non401", preset: catalog.ProviderZAI, status: 403},
		{name: "zai server error remains observation", preset: catalog.ProviderZAI, status: 500},
		{name: "zai nonstandard observed code", preset: catalog.ProviderZAI, status: 777},
		{name: "zai unauthorized", preset: catalog.ProviderZAI, status: 401, wantError: true},
		{name: "overriding authorization", status: 200, overrideSet: true, override: "Bearer synthetic-configured"},
		{name: "empty authorization override", status: 200, overrideSet: true},
		{name: "equal bytes do not prove input provenance", status: 200, overrideSet: true, override: "Bearer synthetic-entered"},
	} {
		t.Run(test.name, func(t *testing.T) {
			type observed struct{ path, method, authorization, configured string }
			requests := make(chan observed, 2)
			host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests <- observed{r.URL.Path, r.Method, r.Header.Get("Authorization"), r.Header.Get("X-Configured")}
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte("not a model catalog"))
			}))
			defer host.Close()
			previous := http.DefaultClient
			http.DefaultClient = host.Client()
			defer func() { http.DefaultClient = previous }()
			provider := connectionProbeProvider()
			provider.BaseURL = host.URL + "/configured/endpoint"
			provider.Configuration = map[string]any{"project": "retained-definition"}
			provider.ExtraHeaders = map[string]string{"X-Configured": "full-provider-header"}
			if test.overrideSet {
				provider.ExtraHeaders["aUtHoRiZaTiOn"] = test.override
			}
			if test.preset != "" {
				connectionProbePreset(&provider, test.preset)
			}
			if test.preset == catalog.ProviderOpenCodeGo {
				provider.BaseURL += "/go"
			}
			before, err := json.Marshal(provider)
			require.NoError(t, err)
			result, err := provider.ProbeConnection(t.Context(), IdentityResolver(), func() error { return nil })
			if test.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			policy := ConnectionProbePolicyHTTP200
			if test.preset == catalog.ProviderZAI {
				policy = ConnectionProbePolicyNon401
			}
			require.Equal(t, ConnectionProbeResult{Kind: ConnectionProbeHTTPResponse, Policy: policy, HTTPStatus: test.status, EnteredKeyInAuthorization: !test.overrideSet, AuthorizationOverridden: test.overrideSet}, result)
			require.NoError(t, result.Validate())
			authorization := "Bearer synthetic-entered"
			if test.overrideSet {
				authorization = test.override
			}
			require.Equal(t, observed{"/configured/endpoint/models", "GET", authorization, "full-provider-header"}, <-requests)
			after, err := json.Marshal(provider)
			require.NoError(t, err)
			require.Equal(t, string(before), string(after), "probing must not rewrite the supplied full definition")
			public, err := json.Marshal(result)
			require.NoError(t, err)
			require.NotContains(t, string(public), "synthetic")
			require.NotContains(t, string(public), host.URL)
			compatibility := provider.TestConnection(t.Context(), IdentityResolver(), func() error { return nil })
			if test.wantError {
				require.Error(t, compatibility)
			} else {
				require.NoError(t, compatibility)
			}
			require.Equal(t, observed{"/configured/endpoint/models", "GET", authorization, "full-provider-header"}, <-requests)
		})
	}
}

func TestConnectionProbeHTTPSOwnerAndCancellation(t *testing.T) {
	for _, mode := range []string{"owner changed before dispatch", "owner changed with response", "owner changed on redirect", "cancel during request", "cancel after response"} {
		t.Run(mode, func(t *testing.T) {
			var requests, redirected, validations atomic.Int32
			var changed atomic.Bool
			entered := make(chan struct{}, 1)
			host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Path == "/redirected" {
					redirected.Add(1)
					w.WriteHeader(200)
					return
				}
				if mode == "cancel during request" {
					entered <- struct{}{}
					<-r.Context().Done()
					return
				}
				changed.Store(true)
				if mode == "owner changed on redirect" {
					http.Redirect(w, r, "/redirected", http.StatusFound)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer host.Close()
			previous := http.DefaultClient
			http.DefaultClient = host.Client()
			defer func() { http.DefaultClient = previous }()
			provider := connectionProbeProvider()
			provider.BaseURL = host.URL
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			validate := func() error {
				if mode == "cancel after response" && changed.Load() {
					cancel()
					return nil
				}
				if mode == "owner changed before dispatch" && validations.Add(1) > 1 || changed.Load() {
					return errors.New("exact probe owner changed")
				}
				return nil
			}
			type completion struct {
				result ConnectionProbeResult
				err    error
			}
			done := make(chan completion, 1)
			go func() {
				result, err := provider.ProbeConnection(ctx, IdentityResolver(), validate)
				done <- completion{result, err}
			}()
			if mode == "cancel during request" {
				select {
				case <-entered:
				case <-time.After(10 * time.Second):
					t.Fatal("probe did not reach synthetic HTTPS")
				}
				cancel()
			}
			var completed completion
			select {
			case completed = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("probe did not finish")
			}
			require.NoError(t, completed.result.Validate())
			if mode == "cancel during request" {
				require.Equal(t, ConnectionProbeHTTPAttempt, completed.result.Kind)
				require.Zero(t, completed.result.HTTPStatus)
			}
			if strings.HasPrefix(mode, "cancel") {
				require.ErrorIs(t, completed.err, context.Canceled)
			} else {
				require.ErrorContains(t, completed.err, "exact probe owner changed")
			}
			if mode == "owner changed before dispatch" {
				require.Zero(t, requests.Load())
			} else {
				require.EqualValues(t, 1, requests.Load())
			}
			require.Zero(t, redirected.Load(), "an owner change must prevent a redirected request")
			if mode == "owner changed with response" || mode == "cancel after response" {
				require.Equal(t, ConnectionProbeHTTPResponse, completed.result.Kind)
				require.Equal(t, 200, completed.result.HTTPStatus)
			}
		})
	}
}

func TestConnectionProbeCancellationReachesCapturedResolver(t *testing.T) {
	for _, mode := range []string{"already cancelled", "during expression", "owner changes during no-probe expression"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			entered := make(chan struct{}, 1)
			var changed atomic.Bool
			resolver := NewShellVariableResolver(env.NewFromMap(map[string]string{"CAPTURED": "synthetic"}), WithExpander(func(expansion context.Context, value string, values []string) (string, error) {
				require.Equal(t, "$CAPTURED", value)
				require.Contains(t, values, "CAPTURED=synthetic")
				entered <- struct{}{}
				if mode == "owner changes during no-probe expression" {
					changed.Store(true)
					return "synthetic", nil
				}
				<-expansion.Done()
				return "", expansion.Err()
			}))
			provider := connectionProbeProvider()
			connectionProbePreset(&provider, catalog.ProviderMiniMax)
			provider.APIKey = "$CAPTURED"
			if mode == "already cancelled" {
				cancel()
			}
			done := make(chan error, 1)
			go func() {
				_, err := provider.ProbeConnection(ctx, resolver, func() error {
					if changed.Load() {
						return errors.New("owner changed during resolution")
					}
					return nil
				})
				done <- err
			}()
			if mode == "during expression" {
				select {
				case <-entered:
				case <-time.After(10 * time.Second):
					t.Fatal("resolver did not start")
				}
				cancel()
			}
			select {
			case err := <-done:
				if strings.HasPrefix(mode, "owner") {
					require.ErrorContains(t, err, "owner changed during resolution")
				} else {
					require.ErrorIs(t, err, context.Canceled)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("resolver did not stop")
			}
			if mode == "already cancelled" {
				require.Empty(t, entered)
			}
		})
	}
}

func TestConnectionProbeAttemptWithoutResponse(t *testing.T) {
	host := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("closed server received a request") }))
	previous := http.DefaultClient
	http.DefaultClient = host.Client()
	defer func() { http.DefaultClient = previous }()
	provider := connectionProbeProvider()
	provider.BaseURL = host.URL
	host.Close()
	result, err := provider.ProbeConnection(t.Context(), IdentityResolver(), func() error { return nil })
	require.Error(t, err)
	require.Equal(t, ConnectionProbeResult{Kind: ConnectionProbeHTTPAttempt, Policy: ConnectionProbePolicyHTTP200, EnteredKeyInAuthorization: true}, result)
	require.NoError(t, result.Validate())
	provider.BaseURL = ""
	result, err = provider.ProbeConnection(t.Context(), IdentityResolver(), func() error { return nil })
	require.ErrorContains(t, err, "missing an API endpoint")
	require.Equal(t, ConnectionProbeResult{Kind: ConnectionProbeNotProbed, Policy: ConnectionProbePolicyHTTP200}, result)
	require.NoError(t, result.Validate())
}

func TestConnectionProbeResultValidation(t *testing.T) {
	for _, result := range []ConnectionProbeResult{
		{Kind: ConnectionProbeNotProbed, Policy: ConnectionProbePolicyNone},
		{Kind: ConnectionProbeNotProbed, Policy: ConnectionProbePolicyHTTP200},
		{Kind: ConnectionProbeNotProbed, Policy: ConnectionProbePolicyNon401},
		{Kind: ConnectionProbeFormatOnly, Policy: ConnectionProbePolicySKPrefix},
		{Kind: ConnectionProbeUnsupported, Policy: ConnectionProbePolicyNone},
		{Kind: ConnectionProbeHTTPAttempt, Policy: ConnectionProbePolicyHTTP200},
		{Kind: ConnectionProbeHTTPAttempt, Policy: ConnectionProbePolicyNon401, AuthorizationOverridden: true},
		{Kind: ConnectionProbeHTTPResponse, Policy: ConnectionProbePolicyHTTP200, HTTPStatus: 100},
		{Kind: ConnectionProbeHTTPResponse, Policy: ConnectionProbePolicyHTTP200, HTTPStatus: 401, EnteredKeyInAuthorization: true},
		{Kind: ConnectionProbeHTTPResponse, Policy: ConnectionProbePolicyNon401, HTTPStatus: 999, AuthorizationOverridden: true},
	} {
		require.NoError(t, result.Validate(), "%+v", result)
	}
	for name, result := range map[string]ConnectionProbeResult{
		"empty":                     {},
		"unknown kind":              {Kind: "verified", Policy: ConnectionProbePolicyNone},
		"unknown policy":            {Kind: ConnectionProbeNotProbed, Policy: "inference-authorized"},
		"unperformed format":        {Kind: ConnectionProbeNotProbed, Policy: ConnectionProbePolicySKPrefix},
		"format HTTP policy":        {Kind: ConnectionProbeFormatOnly, Policy: ConnectionProbePolicyHTTP200},
		"unsupported HTTP policy":   {Kind: ConnectionProbeUnsupported, Policy: ConnectionProbePolicyHTTP200},
		"unattempted headers":       {Kind: ConnectionProbeNotProbed, Policy: ConnectionProbePolicyNone, EnteredKeyInAuthorization: true},
		"format headers":            {Kind: ConnectionProbeFormatOnly, Policy: ConnectionProbePolicySKPrefix, AuthorizationOverridden: true},
		"unsupported status":        {Kind: ConnectionProbeUnsupported, Policy: ConnectionProbePolicyNone, HTTPStatus: 200},
		"attempted format":          {Kind: ConnectionProbeHTTPAttempt, Policy: ConnectionProbePolicySKPrefix},
		"attempt with response":     {Kind: ConnectionProbeHTTPAttempt, Policy: ConnectionProbePolicyHTTP200, HTTPStatus: 200},
		"response without status":   {Kind: ConnectionProbeHTTPResponse, Policy: ConnectionProbePolicyHTTP200},
		"response status too small": {Kind: ConnectionProbeHTTPResponse, Policy: ConnectionProbePolicyHTTP200, HTTPStatus: 99},
		"response status too large": {Kind: ConnectionProbeHTTPResponse, Policy: ConnectionProbePolicyHTTP200, HTTPStatus: 1000},
		"response without policy":   {Kind: ConnectionProbeHTTPResponse, Policy: ConnectionProbePolicyNone, HTTPStatus: 200},
		"contradictory headers":     {Kind: ConnectionProbeHTTPResponse, Policy: ConnectionProbePolicyHTTP200, HTTPStatus: 200, EnteredKeyInAuthorization: true, AuthorizationOverridden: true},
	} {
		t.Run(name, func(t *testing.T) { require.Error(t, result.Validate()) })
	}
}
