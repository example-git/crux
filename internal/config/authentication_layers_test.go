package config

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth"
	"github.com/stretchr/testify/require"
)

func authenticationLayerFixture(t *testing.T, names, content []string) (authenticationConfigInputs, []string) {
	t.Helper()
	require.Len(t, content, len(names))
	inputs := authenticationConfigInputs{valid: true}
	root := t.TempDir()
	for i, name := range names {
		path := filepath.Join(root, name)
		inputs.order = append(inputs.order, path)
		inputs.files = append(inputs.files, authenticationInputFile{path: path, data: []byte(content[i]), info: authenticationInputFileInfo{exists: content[i] != ""}})
	}
	return inputs, inputs.order
}

func authenticationLayerDesired(id, access string) ProviderConfig {
	return ProviderConfig{ID: id, APIKey: access, OAuthToken: &oauth.Token{AccessToken: access, ExpiresAt: 4102444800}, Owner: &ProviderOwnerReference{Type: ProviderOwnerCustom}}
}

func TestAuthenticationLayersEvaluateShellOnceAndUseCapturedEnvironment(t *testing.T) {
	inputs, paths := authenticationLayerFixture(t, []string{"base.json", "cruxrc", "scope.json"}, []string{
		`{"providers":{"exact.id":{"api_key":"old","owner":{"type":"custom"}}}}`,
		`printf x >> "$MARKER"
provider add exact.id --api-key "$CAPTURED_KEY" --base-url https://example.invalid/v1`, "",
	})
	marker := filepath.Join(filepath.Dir(paths[0]), "marker")
	t.Setenv("CAPTURED_KEY", "live-value-must-not-be-used")
	base := env.NewFromMap(map[string]string{"CAPTURED_KEY": "captured-key", "MARKER": marker})
	layers, err := evaluateAuthenticationLayers(t.Context(), inputs, base, nil, RuntimeOverrides{})
	require.NoError(t, err)
	want := authenticationLayerDesired("exact.id", "selected-key")
	for range 2 {
		edit, err := layers.stageCredentials(t.Context(), paths[2], "exact.id", &want)
		require.NoError(t, err)
		prior, ok := edit.before.Providers.Get("exact.id")
		require.True(t, ok)
		require.Equal(t, "captured-key", prior.APIKey)
		next, _ := edit.after.Providers.Get("exact.id")
		require.Equal(t, "selected-key", next.APIKey)
	}
	written, err := os.ReadFile(marker)
	require.NoError(t, err)
	require.Equal(t, "x", string(written), "both previews reuse the one shell evaluation")
	_, err = os.Stat(paths[2])
	require.ErrorIs(t, err, os.ErrNotExist, "preparation never writes the scope")
}

func TestAuthenticationLayersLiteralCredentialEditPreservesSiblings(t *testing.T) {
	inputs, paths := authenticationLayerFixture(t, []string{"scope.json"}, []string{`{
"providers":{"exact.id":{"api_key":"old","oauth":{"access_token":"old","refresh_token":"old-refresh","expires_in":10,"expires_at":100},"owner":{"type":"custom"},"disable":true,"tooling_instructions":"native","provider_options":{"large":9007199254740991}},"exact":{"id":"neighboring-id-untouched","api_key":"neighboring-key-untouched"},"other":{"api_key":"other-untouched"}},
"models":{"large":{"provider":"exact.id","model":"main","provider_options":{"budget":123}},"small":{"provider":"other","model":"title"}},
"foreign":{"number":9007199254740993,"spelling":1e3,"array":["a","b"]}}`})
	layers, err := evaluateAuthenticationLayers(t.Context(), inputs, env.NewFromMap(nil), nil, RuntimeOverrides{})
	require.NoError(t, err)
	want := authenticationLayerDesired("exact.id", "selected-key")
	edit, err := layers.stageCredentials(t.Context(), paths[0], "exact.id", &want)
	require.NoError(t, err)
	provider, _ := edit.after.Providers.Get("exact.id")
	require.True(t, provider.Disable)
	require.Equal(t, "native", provider.ToolingInstructions)
	require.Equal(t, want.OAuthToken, provider.OAuthToken)
	require.Equal(t, edit.before.Models, edit.after.Models)
	for _, keys := range [][]string{{"foreign"}, {"providers", "other"}, {"providers", "exact"}, {"providers", "exact.id", "provider_options"}} {
		before, err := runtimeControlReadField(inputs.files[0].data, keys)
		require.NoError(t, err)
		after, err := runtimeControlReadField(edit.data, keys)
		require.NoError(t, err)
		require.True(t, authenticationMetadataEqual(before.Value, after.Value), "%v must retain exact numeric values", keys)
	}
	logout, err := layers.stageCredentials(t.Context(), paths[0], "exact.id", nil)
	require.NoError(t, err)
	for _, field := range []string{"api_key", "oauth"} {
		value, err := runtimeControlReadField(logout.data, []string{"providers", "exact.id", field})
		require.NoError(t, err)
		require.False(t, value.Present, "logout deletes the scoped key")
	}
	loggedOut, _ := logout.after.Providers.Get("exact.id")
	require.True(t, loggedOut.Disable)
	require.Empty(t, loggedOut.APIKey)
	require.Nil(t, loggedOut.OAuthToken)
}

func TestAuthenticationLayersRejectInheritedAndHigherPriorityCredentials(t *testing.T) {
	for _, test := range []struct {
		name, base, higher string
		ephemeral          map[string]ProviderConfig
		switchOK           bool
	}{
		{name: "lower key", base: `{"providers":{"p":{"api_key":"lower"}}}`, higher: `{}`, switchOK: true},
		{name: "higher key", base: `{}`, higher: `{"providers":{"p":{"api_key":"higher"}}}`},
		{name: "higher OAuth", base: `{}`, higher: `{"providers":{"p":{"oauth":{"access_token":"higher","expires_in":1,"expires_at":2}}}}`},
		{name: "ephemeral key", base: `{}`, higher: `{}`, ephemeral: map[string]ProviderConfig{"p": {APIKey: "ephemeral"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			inputs, paths := authenticationLayerFixture(t, []string{"base.json", "scope.json", "higher.json"}, []string{test.base, `{"providers":{"p":{"api_key":"old","owner":{"type":"custom"}}}}`, test.higher})
			layers, err := evaluateAuthenticationLayers(t.Context(), inputs, env.NewFromMap(nil), test.ephemeral, RuntimeOverrides{})
			require.NoError(t, err)
			_, err = layers.stageCredentials(t.Context(), paths[1], "p", nil)
			require.ErrorContains(t, err, "logout is shadowed")
			want := authenticationLayerDesired("p", "selected")
			_, err = layers.stageCredentials(t.Context(), paths[1], "p", &want)
			if test.switchOK {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "switch is shadowed")
			}
		})
	}
}

func TestAuthenticationLayersClearLowerRefreshToken(t *testing.T) {
	inputs, paths := authenticationLayerFixture(t, []string{"lower.json", "scope.json"}, []string{`{"providers":{"p":{"api_key":"old","oauth":{"access_token":"old","refresh_token":"old-refresh","expires_at":1,"expires_in":1}}}}`, ""})
	layers, err := evaluateAuthenticationLayers(t.Context(), inputs, env.NewFromMap(nil), nil, RuntimeOverrides{})
	require.NoError(t, err)
	want := authenticationLayerDesired("p", "nonrefreshable-selected")
	edit, err := layers.stageCredentials(t.Context(), paths[1], "p", &want)
	require.NoError(t, err)
	provider, _ := edit.after.Providers.Get("p")
	require.Equal(t, want.OAuthToken, provider.OAuthToken)
	require.Empty(t, provider.OAuthToken.RefreshToken)
	require.Nil(t, provider.OAuthToken.Client)
}

func TestAuthenticationLayersRejectInheritedOAuthClient(t *testing.T) {
	inputs, paths := authenticationLayerFixture(t, []string{"lower.json", "scope.json"}, []string{`{"providers":{"p":{"oauth":{"access_token":"old","expires_at":1,"expires_in":1,"client":{"client_id":"old-client"}}}}}`, ""})
	layers, err := evaluateAuthenticationLayers(t.Context(), inputs, env.NewFromMap(nil), nil, RuntimeOverrides{})
	require.NoError(t, err)
	want := authenticationLayerDesired("p", "selected-without-client")
	_, err = layers.stageCredentials(t.Context(), paths[1], "p", &want)
	// The existing loader does not remove an object with null. Returning this
	// conflict is required; accepting a mixture of two accounts is not a switch.
	require.ErrorContains(t, err, "switch is shadowed")
}

func TestAuthenticationLayersClearLowerOptionalOAuthClientFields(t *testing.T) {
	inputs, paths := authenticationLayerFixture(t, []string{"lower.json", "scope.json"}, []string{`{"providers":{"p":{"oauth":{"access_token":"old","expires_at":1,"expires_in":1,"client":{"client_id":"old-client","client_secret":"old-secret","auth_url":"https://old.invalid/auth","token_url":"https://old.invalid/token","auth_style":2}}}}}`, ""})
	layers, err := evaluateAuthenticationLayers(t.Context(), inputs, env.NewFromMap(nil), nil, RuntimeOverrides{})
	require.NoError(t, err)
	want := authenticationLayerDesired("p", "selected")
	want.OAuthToken.Client = &oauth.OAuthClient{ClientID: "selected-client"}
	edit, err := layers.stageCredentials(t.Context(), paths[1], "p", &want)
	require.NoError(t, err)
	provider, _ := edit.after.Providers.Get("p")
	require.Equal(t, want.OAuthToken, provider.OAuthToken)
}

func TestAuthenticationLayersModelPinsReplaceRecords(t *testing.T) {
	inputs, paths := authenticationLayerFixture(t, []string{"scope.json"}, []string{`{"providers":{"p":{"api_key":"old"}},"models":{"large":{"provider":"p","model":"base","provider_options":{"stale":1}},"small":{"provider":"p","model":"small","think":true}}}`})
	pin := SelectedModel{Provider: "p", Model: "pinned", ProviderOptions: map[string]any{"kept": json.Number("9007199254740993")}}
	layers, err := evaluateAuthenticationLayers(t.Context(), inputs, env.NewFromMap(nil), nil, RuntimeOverrides{Models: map[SelectedModelType]SelectedModel{SelectedModelTypeLarge: pin}})
	require.NoError(t, err)
	pin.ProviderOptions["kept"] = "caller edit"
	edit, err := layers.stageCredentials(t.Context(), paths[0], "p", nil)
	require.NoError(t, err)
	require.Equal(t, "pinned", edit.after.Models[SelectedModelTypeLarge].Model)
	require.Equal(t, map[string]any{"kept": json.Number("9007199254740993")}, edit.after.Models[SelectedModelTypeLarge].ProviderOptions)
	require.Equal(t, edit.before.Models, edit.after.Models)
}

func TestAuthenticationLayersRejectUnpinnedNumericLoss(t *testing.T) {
	for _, source := range []string{
		`{"providers":{"p":{"api_key":"old","provider_options":{"counter":9007199254740993}}}}`,
		`{"providers":{"p":{"api_key":"old"}},"models":{"small":{"provider":"p","model":"small","provider_options":{"counter":9007199254740993}}}}`,
	} {
		inputs, paths := authenticationLayerFixture(t, []string{"scope.json"}, []string{source})
		layers, err := evaluateAuthenticationLayers(t.Context(), inputs, env.NewFromMap(nil), nil, RuntimeOverrides{})
		require.NoError(t, err)
		_, err = layers.stageCredentials(t.Context(), paths[0], "p", nil)
		require.ErrorContains(t, err, "numeric values the current loader cannot preserve")
		_, err = os.Stat(paths[0])
		require.ErrorIs(t, err, os.ErrNotExist)
	}
}

func TestAuthenticationLayersPrivacyCancellationAndInvalidInput(t *testing.T) {
	inputs, paths := authenticationLayerFixture(t, []string{"scope.json"}, []string{`{"providers":{"p":{"api_key":"synthetic-private-key"}}}`})
	layers, err := evaluateAuthenticationLayers(t.Context(), inputs, env.NewFromMap(nil), nil, RuntimeOverrides{})
	require.NoError(t, err)
	edit, err := layers.stageCredentials(t.Context(), paths[0], "p", nil)
	require.NoError(t, err)
	for _, value := range []any{layers, edit} {
		_, err := json.Marshal(value)
		require.Error(t, err)
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
			text := fmt.Sprintf(verb, value)
			require.NotContains(t, text, "synthetic-private-key")
			require.NotContains(t, text, paths[0])
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = evaluateAuthenticationLayers(ctx, inputs, env.NewFromMap(nil), nil, RuntimeOverrides{})
	require.ErrorIs(t, err, context.Canceled)
	_, err = layers.stageCredentials(ctx, paths[0], "p", nil)
	require.ErrorIs(t, err, context.Canceled)
	_, err = layers.stageCredentials(t.Context(), filepath.Join(t.TempDir(), "foreign.json"), "p", nil)
	require.ErrorContains(t, err, "captured writable JSON scope")
	inputs.files[0].data = []byte(`{"providers": "synthetic-private-invalid"}`)
	layers, err = evaluateAuthenticationLayers(t.Context(), inputs, env.NewFromMap(nil), nil, RuntimeOverrides{})
	require.NoError(t, err)
	_, err = layers.stageCredentials(t.Context(), paths[0], "p", nil)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "synthetic-private-invalid")
}

func TestAuthenticationLayersRejectAmbiguousSourceObjects(t *testing.T) {
	for _, source := range []string{
		`null`, `[]`, `{"providers":{},"providers":{"p":{"api_key":"second"}}}`,
		`{"providers":{"p":{"api_key":"first","api_key":"second"}}}`,
		`{"foreign":[{"same":1,"same":2}]}`,
		"{\"invalid\":\"\xff\"}",
	} {
		inputs, _ := authenticationLayerFixture(t, []string{"scope.json"}, []string{source})
		_, err := evaluateAuthenticationLayers(t.Context(), inputs, env.NewFromMap(nil), nil, RuntimeOverrides{})
		require.ErrorContains(t, err, "JSON object")
		require.NotContains(t, err.Error(), source)
	}
}
