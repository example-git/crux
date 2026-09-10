package config

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/oauth/gemini"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestSelectedRefreshUsesCapturedAccountDatabase(t *testing.T) {
	for _, mode := range []string{"captured-directory", "captured-home", "ambient-changed-during-exchange", "captured-absence"} {
		t.Run(mode, func(t *testing.T) {
			store, owner, original, values := selectedGeminiIdentityStore(t, "captured-project")
			database := filepath.Join(values["AI_CLI_DIR"], "accounts.json")
			if mode == "captured-home" {
				target := filepath.Join(values["HOME"], ".ai-cli", "accounts.json")
				require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o700))
				require.NoError(t, os.Rename(database, target))
				database = target
				delete(values, "AI_CLI_DIR")
			}
			if mode == "captured-absence" {
				delete(values, "AI_CLI_DIR")
				delete(values, "HOME")
				delete(values, "USERPROFILE")
			}
			store.baseEnvironment = env.NewFromMap(maps.Clone(values))
			store.effectiveEnvironment = cloneEnvironment(store.baseEnvironment)
			before, err := os.ReadFile(database)
			require.NoError(t, err)
			before, err = sjson.SetRawBytes(before, "foreignHarness", []byte(`{"number":1.0,"large":9007199254740993,"keep":"verbatim"}`))
			require.NoError(t, err)
			before, err = sjson.SetRawBytes(before, "accounts.other", []byte(`[{"id":"other","accessToken":"other-secret","vendor":{"number":2.0}}]`))
			require.NoError(t, err)
			before, err = sjson.SetRawBytes(before, "accounts.gemini.0.vendor", []byte(`{"number":3.0,"keep":true}`))
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(database, before, 0o600))
			admitted := store.RuntimeSnapshot()

			ambientRoots := []string{t.TempDir(), t.TempDir()}
			ambientBytes := make(map[string][]byte)
			for _, root := range ambientRoots {
				document, err := json.Marshal(map[string]any{"active": map[string]string{owner.AccountNamespace: original.ID}, "accounts": map[string][]accounts.Entry{owner.AccountNamespace: {original}}, "hostileMarker": root})
				require.NoError(t, err)
				path := filepath.Join(root, "accounts.json")
				require.NoError(t, os.WriteFile(path, document, 0o600))
				ambientBytes[path] = document
			}
			for key, value := range map[string]string{"AI_CLI_DIR": ambientRoots[0], "HOME": ambientRoots[0], "USERPROFILE": ambientRoots[0], "GEMINI_OAUTH_CLIENT_ID": "ambient-client", "GEMINI_OAUTH_CLIENT_SECRET": "ambient-secret", "GEMINI_PROJECT_ID": "ambient-project"} {
				t.Setenv(key, value)
			}
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.NotNil(t, r.TLS)
				assert.Equal(t, "/token", r.URL.Path)
				require.NoError(t, r.ParseForm())
				assert.Equal(t, "synthetic-client", r.PostForm.Get("client_id"))
				assert.Equal(t, "synthetic-secret", r.PostForm.Get("client_secret"))
				assert.Equal(t, original.RefreshToken, r.PostForm.Get("refresh_token"))
				if mode == "ambient-changed-during-exchange" {
					// The initial Setenv cleanup restores every process variable after this
					// test; only disposable fixture directories are selected here.
					for _, key := range []string{"AI_CLI_DIR", "HOME", "USERPROFILE"} {
						require.NoError(t, os.Setenv(key, ambientRoots[1]))
					}
				}
				_, _ = io.WriteString(w, `{"access_token":"captured-fresh-access","refresh_token":"captured-fresh-refresh","expires_in":3600}`)
			}))
			defer server.Close()
			endpoint, err := url.Parse(server.URL)
			require.NoError(t, err)
			priorClient, priorTransport := http.DefaultClient, http.DefaultTransport
			http.DefaultClient = &http.Client{Transport: selectedGeminiIdentityTransport(func(r *http.Request) (*http.Response, error) {
				assert.Equal(t, "gemini-ag-token.example.invalid", r.URL.Host)
				request := r.Clone(r.Context())
				address := *r.URL
				address.Scheme, address.Host = endpoint.Scheme, endpoint.Host
				request.URL = &address
				return server.Client().Transport.RoundTrip(request)
			})}
			http.DefaultTransport = http.DefaultClient.Transport
			defer func() { http.DefaultClient, http.DefaultTransport = priorClient, priorTransport }()
			fresh, err := store.RefreshSelectedOAuthAccountForRuntime(t.Context(), ScopeGlobal, owner, original, true, admitted)
			if mode == "captured-absence" {
				require.ErrorContains(t, err, "captured account home")
				require.Nil(t, fresh)
				require.Zero(t, calls.Load())
				after, err := os.ReadFile(database)
				require.NoError(t, err)
				require.Equal(t, before, after)
			} else {
				require.NoError(t, err)
				require.Equal(t, "captured-fresh-access", fresh.AccessToken)
				require.EqualValues(t, 1, calls.Load())
				state, err := accounts.CaptureStateAt(t.Context(), database, []string{owner.AccountNamespace})
				require.NoError(t, err)
				require.Equal(t, original.ID, state.ActiveID(owner.AccountNamespace))
				require.Len(t, state.Entries(owner.AccountNamespace), 1)
				require.Equal(t, accounts.CredentialID(*fresh), accounts.CredentialID(state.Entries(owner.AccountNamespace)[0]))
				configured, _ := store.Config().Providers.Get(gemini.ID)
				require.Equal(t, fresh.AccessToken, configured.APIKey)
				written, err := os.ReadFile(store.globalDataPath)
				require.NoError(t, err)
				require.Equal(t, fresh.RefreshToken, gjson.GetBytes(written, "providers.gemini-ag.oauth.refresh_token").String())
				after, err := os.ReadFile(database)
				require.NoError(t, err)
				for _, field := range []string{"foreignHarness", "accounts.other", "accounts.gemini.0.vendor"} {
					require.Equal(t, gjson.GetBytes(before, field).Raw, gjson.GetBytes(after, field).Raw, "unrelated harness bytes must survive refresh")
				}
				// Reusing the admitted token may adopt its already recorded rotation; it
				// cannot exchange the consumed token again or redirect to ambient storage.
				adopted, err := store.RefreshSelectedOAuthAccountForRuntime(t.Context(), ScopeGlobal, owner, original, true, admitted)
				require.NoError(t, err)
				require.Equal(t, accounts.CredentialID(*fresh), accounts.CredentialID(*adopted))
				require.EqualValues(t, 1, calls.Load())
			}
			for path, before := range ambientBytes {
				after, err := os.ReadFile(path)
				require.NoError(t, err)
				require.Equal(t, before, after, "ambient database must remain byte-identical")
				entries, err := os.ReadDir(filepath.Dir(path))
				require.NoError(t, err)
				require.Len(t, entries, 1, "captured operation must not create ambient locks or files")
				require.Equal(t, "accounts.json", entries[0].Name())
			}
		})
	}
}

func TestSelectedRefreshCapturedDatabaseCancellationCreatesNoAmbientState(t *testing.T) {
	store, owner, entry, _ := selectedGeminiIdentityStore(t, "captured-project")
	admitted := store.RuntimeSnapshot()
	ambient := filepath.Join(t.TempDir(), "uncreated")
	t.Setenv("AI_CLI_DIR", ambient)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := store.RefreshSelectedOAuthAccountForRuntime(ctx, ScopeGlobal, owner, entry, true, admitted)
	require.ErrorIs(t, err, context.Canceled)
	_, err = os.Stat(ambient)
	require.True(t, os.IsNotExist(err))
}
