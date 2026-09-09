package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/shellconfig"
	"github.com/qjebbs/go-jsons"
)

// authenticationLayers is a private, evaluated copy of captured configuration
// sources. Preparing it may execute shell configuration; reading authentication
// status never does. Preparation must run outside account and config file locks.
// Reusing this value for both sides of an edit evaluates each shell source once.
type authenticationLayers struct {
	order    []string
	values   map[string][]byte
	overlays [][]byte
	pinned   map[SelectedModelType]SelectedModel
}

// authenticationCredentialEdit is preparation only, not evidence of a write or
// runtime publication. The fixed transaction must validate the original input
// observation again, then prepare and publish an exact runtime candidate.
type authenticationCredentialEdit struct {
	path          string
	data          []byte
	before, after *Config
}

func (authenticationLayers) MarshalJSON() ([]byte, error) {
	return nil, errors.New("authentication layers are private")
}
func (authenticationLayers) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[private authentication layers]"))
}
func (authenticationCredentialEdit) MarshalJSON() ([]byte, error) {
	return nil, errors.New("authentication credential edits are private")
}
func (authenticationCredentialEdit) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[private authentication credential edit]"))
}

// evaluateAuthenticationLayers reads only retained bytes and the supplied
// captured environment. It never discovers files, resolves API-key expressions,
// refreshes accounts, scans plugins, applies defaults, or mutates process state.
// The caller captures environment, ephemeral providers and pins under writeMu.
func evaluateAuthenticationLayers(ctx context.Context, inputs authenticationConfigInputs, base env.Env, ephemeral map[string]ProviderConfig, overrides RuntimeOverrides) (authenticationLayers, error) {
	if err := ctx.Err(); err != nil {
		return authenticationLayers{}, err
	}
	if !inputs.valid || base == nil {
		return authenticationLayers{}, errors.New("authentication layer preparation requires captured inputs and environment")
	}
	layers := authenticationLayers{order: slices.Clone(inputs.order), values: map[string][]byte{}}
	for _, path := range layers.order {
		if err := ctx.Err(); err != nil {
			return authenticationLayers{}, err
		}
		if _, evaluated := layers.values[path]; evaluated {
			continue
		}
		file, captured := inputs.file(path)
		if !captured {
			return authenticationLayers{}, errors.New("authentication configuration source was not captured")
		}
		data := file.data
		if !file.info.exists || len(data) == 0 {
			layers.values[path] = nil
			continue
		}
		if isShellConfig(path) {
			var err error
			data, err = shellconfig.LoadShellConfig(ctx, path, data, base.Env())
			if err != nil {
				if ctx.Err() != nil {
					return authenticationLayers{}, ctx.Err()
				}
				// Interpreter errors can contain source bytes, paths or secrets.
				return authenticationLayers{}, errors.New("authentication shell configuration could not be evaluated")
			}
		}
		if len(data) > 0 && !authenticationLayerObject(data) {
			return authenticationLayers{}, errors.New("authentication configuration source must be a JSON object")
		}
		layers.values[path] = bytes.Clone(data)
	}
	if len(ephemeral) > 0 {
		data, err := json.Marshal(struct {
			Providers map[string]ProviderConfig `json:"providers"`
		}{ephemeral})
		if err != nil {
			return authenticationLayers{}, errors.New("authentication ephemeral provider configuration could not be encoded")
		}
		layers.overlays = append(layers.overlays, data)
	}
	if len(overrides.Models) > 0 {
		layers.pinned = make(map[SelectedModelType]SelectedModel, len(overrides.Models))
		for kind, model := range overrides.Models {
			layers.pinned[kind] = cloneSelectedModel(model)
		}
	}
	return layers, ctx.Err()
}

func authenticationLayerObject(data []byte) bool {
	if !utf8.Valid(data) || !json.Valid(data) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return false
	}
	var value func(json.Token) bool
	value = func(token json.Token) bool {
		switch token {
		case json.Delim('{'):
			keys := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				name, ok := key.(string)
				if err != nil || !ok || keys[name] {
					return false
				}
				keys[name] = true
				next, err := decoder.Token()
				if err != nil || !value(next) {
					return false
				}
			}
			end, err := decoder.Token()
			return err == nil && end == json.Delim('}')
		case json.Delim('['):
			for decoder.More() {
				next, err := decoder.Token()
				if err != nil || !value(next) {
					return false
				}
			}
			end, err := decoder.Token()
			return err == nil && end == json.Delim(']')
		default:
			_, delimiter := token.(json.Delim)
			return !delimiter
		}
	}
	return value(start) && decoder.Decode(new(any)) == io.EOF
}

func (layers authenticationLayers) merged(path string, replacement []byte) (*Config, error) {
	return layers.mergedReplacements(map[string][]byte{path: replacement})
}

func (layers authenticationLayers) mergedReplacements(replacements map[string][]byte) (*Config, error) {
	values := make([][]byte, 0, len(layers.order)+len(layers.overlays))
	for _, source := range layers.order {
		data := layers.values[source]
		if replacement, changed := replacements[source]; changed {
			data = replacement
		}
		if len(data) > 0 {
			values = append(values, data)
		}
	}
	values = append(values, layers.overlays...)
	merged, err := jsons.Merge(values)
	if err != nil {
		return nil, errors.New("authentication configuration layers could not be merged")
	}
	if err := layers.checkPrecision(values, merged); err != nil {
		return nil, err
	}
	var cfg Config
	if json.Unmarshal(merged, &cfg) != nil {
		return nil, errors.New("authentication configuration layers could not be decoded")
	}
	if len(layers.pinned) > 0 && cfg.Models == nil {
		cfg.Models = map[SelectedModelType]SelectedModel{}
	}
	for kind, model := range layers.pinned {
		cfg.Models[kind] = cloneSelectedModel(model)
	}
	return &cfg, nil
}

// The ordinary loader's merge converts numbers through float64. Before/after
// equality alone cannot detect the same rounding on both sides. A number-aware
// reference merge detects that loss without changing the loader's semantics.
// Unknown document roots are not Config fields and remain raw in the file edit;
// pinned model records replace their raw slot completely after normal loading.
func (layers authenticationLayers) checkPrecision(values [][]byte, merged []byte) error {
	exact := jsons.NewMerger()
	decode := func(data []byte) (map[string]any, error) {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		var object map[string]any
		if err := decoder.Decode(&object); err != nil {
			return nil, err
		}
		return object, nil
	}
	if exact.RegisterLoader(jsons.FormatJSON, []string{".json"}, decode) != nil {
		return errors.New("authentication configuration precision check is unavailable")
	}
	reference, err := exact.Merge(values)
	if err != nil {
		return errors.New("authentication configuration precision could not be checked")
	}
	want, wantErr := decode(reference)
	got, gotErr := decode(merged)
	if wantErr != nil || gotErr != nil {
		return errors.New("authentication configuration precision could not be checked")
	}
	known := map[string]bool{}
	configType := reflect.TypeFor[Config]()
	for i := range configType.NumField() {
		field := configType.Field(i)
		key := strings.Split(field.Tag.Get("json"), ",")[0]
		if field.IsExported() && key != "-" {
			if key == "" {
				key = field.Name
			}
			known[strings.ToLower(key)] = true
		}
	}
	for _, object := range []map[string]any{want, got} {
		for key := range object {
			if !known[strings.ToLower(key)] {
				delete(object, key)
			}
		}
		if models, ok := object["models"].(map[string]any); ok {
			for kind := range layers.pinned {
				delete(models, string(kind))
			}
		}
	}
	if !runtimeControlJSONValuesEqual(want, got) {
		return errors.New("authentication configuration contains numeric values the current loader cannot preserve")
	}
	return nil
}

// stageCredentials changes only literal credential/ownership members in one
// captured JSON source. desired=nil deletes api_key and oauth from that scope.
// A non-nil desired is the exact owner-mapped OAuth provider, already prepared
// from the selected account. It is never a provider-wide replacement.
func (layers authenticationLayers) stageCredentials(ctx context.Context, path, providerID string, desired *ProviderConfig) (authenticationCredentialEdit, error) {
	return layers.stageCredentialsAtPaths(ctx, path, providerID, desired, []string{path})
}

// stageCredentialsAtPaths represents one physical rename observed through its
// prevalidated aliases. It never creates additional physical writes.
func (layers authenticationLayers) stageCredentialsAtPaths(ctx context.Context, path, providerID string, desired *ProviderConfig, writtenPaths []string) (authenticationCredentialEdit, error) {
	if err := ctx.Err(); err != nil {
		return authenticationCredentialEdit{}, err
	}
	if !filepath.IsAbs(path) || providerID == "" || isShellConfig(path) || !slices.Contains(layers.order, path) {
		return authenticationCredentialEdit{}, errors.New("authentication edit requires its captured writable JSON scope and exact provider")
	}
	before, err := layers.merged("", nil)
	if err != nil {
		return authenticationCredentialEdit{}, err
	}
	data := layers.values[path]
	if len(data) == 0 {
		data = []byte("{}")
	}
	fields := []string{"api_key", "oauth"}
	var values map[string]any
	if desired != nil {
		if desired.ID != providerID || desired.OAuthToken == nil || desired.Owner == nil {
			return authenticationCredentialEdit{}, errors.New("authentication switch requires its exact OAuth provider and owner")
		}
		values = authenticationCredentialValues(desired)
		fields = slices.Sorted(maps.Keys(values))
	}
	for _, field := range fields {
		var value json.RawMessage
		if desired != nil {
			value, err = json.Marshal(values[field])
			if err != nil {
				return authenticationCredentialEdit{}, errors.New("authentication credential mapping could not be encoded")
			}
		}
		data, err = runtimeControlChangeField(data, []string{"providers", providerID, field}, value, desired == nil)
		if err != nil {
			return authenticationCredentialEdit{}, errors.New("authentication scoped configuration must contain objects")
		}
	}
	if !slices.Contains(writtenPaths, path) {
		return authenticationCredentialEdit{}, errors.New("authentication scope aliases omit the written path")
	}
	replacements := make(map[string][]byte, len(writtenPaths))
	for _, written := range writtenPaths {
		replacements[written] = data
	}
	after, err := layers.mergedReplacements(replacements)
	if err != nil {
		return authenticationCredentialEdit{}, err
	}
	var provider ProviderConfig
	if after.Providers != nil {
		provider, _ = after.Providers.Get(providerID)
	}
	if desired == nil {
		if provider.APIKey != "" || provider.APIKeyTemplate != "" || provider.OAuthToken != nil {
			return authenticationCredentialEdit{}, errors.New("authentication logout is shadowed by credentials in another configuration layer")
		}
	} else {
		expected, encodeErr := json.Marshal(desired.OAuthToken)
		actual, actualErr := json.Marshal(provider.OAuthToken)
		if encodeErr != nil || actualErr != nil || provider.APIKey != desired.APIKey || !bytes.Equal(expected, actual) {
			return authenticationCredentialEdit{}, errors.New("authentication switch is shadowed by another configuration layer")
		}
		for _, field := range []struct{ expected, actual any }{{desired.Owner, provider.Owner}, {desired.Plugin, provider.Plugin}, {desired.Preset, provider.Preset}} {
			expected, _ := json.Marshal(field.expected)
			actual, _ := json.Marshal(field.actual)
			if !bytes.Equal(expected, actual) {
				return authenticationCredentialEdit{}, errors.New("authentication ownership is shadowed by another configuration layer")
			}
		}
	}
	return authenticationCredentialEdit{path: path, data: bytes.Clone(data), before: before, after: after}, ctx.Err()
}

// authenticationCredentialValues is the shared exact scoped-write receipt.
// Empty optional OAuth/client fields intentionally mask inherited sibling
// values during the ordinary loader's object merge.
func authenticationCredentialValues(desired *ProviderConfig) map[string]any {
	if desired == nil {
		return map[string]any{"api_key": nil, "oauth": nil}
	}
	token := desired.OAuthToken
	var client any
	if token.Client != nil {
		client = map[string]any{
			"client_id": token.Client.ClientID, "client_secret": token.Client.ClientSecret,
			"auth_url": token.Client.AuthURL, "token_url": token.Client.TokenURL,
			"auth_style": token.Client.AuthStyle,
		}
	}
	values := map[string]any{"api_key": desired.APIKey, "oauth": map[string]any{
		"access_token": token.AccessToken, "refresh_token": token.RefreshToken,
		"expires_in": token.ExpiresIn, "expires_at": token.ExpiresAt, "client": client,
	}, "owner": desired.Owner}
	if desired.Plugin != nil {
		values["plugin"] = desired.Plugin
	}
	if desired.Preset != nil {
		values["preset"] = desired.Preset
	}
	return values
}
