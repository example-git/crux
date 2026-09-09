package proto

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func mutationProtocolFixture() (providerauth.SwitchRequest, ProviderAuthenticationMutationResponse) {
	owner := providerauth.Owner{ProviderID: "fixture", HasOAuth: true}
	target := providerauth.Target{WorkspaceID: "workspace", Owner: owner, Generation: providerauth.Generation{Epoch: strings.Repeat("a", 32), Sequence: 1}}
	request := providerauth.SwitchRequest{OperationID: strings.Repeat("b", 32), Target: target, AccountID: "selected"}
	current := target
	current.Generation.Sequence++
	selected := config.SelectedModel{Provider: "fixture", Model: "model", ProviderOptions: map[string]any{"literal.dotted": json.Number("9007199254740993"), "false": false, "zero": json.Number("0"), "empty": ""}}
	status := providerauth.Status{Owner: owner, Configured: true, AccountState: "in-sync", ActiveAccountID: "selected", Credentials: []providerauth.CredentialStatus{{Kind: "api-key", State: "configured"}, {Kind: "oauth", State: "present"}}}
	state := providerauth.AccountsState{Target: current, Status: status, Accounts: []providerauth.AccountSummary{{ID: "selected", DisplayName: "Selected", Active: true, CredentialState: "present"}}}
	model := &providerauth.OwnedModelState{Model: selected, Owner: &owner}
	catalogModels := []catalog.Model{{ID: "model", Name: "Model", ContextWindow: 8192, DefaultMaxTokens: 1024}}
	view := &AuthenticationWorkspaceView{ID: "workspace", Config: &config.Config{Providers: csync.NewMapFrom(map[string]config.ProviderConfig{"fixture": {ID: "fixture", Models: catalogModels}}), Models: map[config.SelectedModelType]config.SelectedModel{config.SelectedModelTypeLarge: selected, config.SelectedModelTypeSmall: selected}}, ProviderSurfaces: []AuthenticationProviderSurface{{ID: "fixture", Name: "Fixture", Owner: &owner, Available: true, Availability: "available", Models: catalogModels}}}
	return request, ProviderAuthenticationMutationResponse{Outcome: providerauth.MutationOutcome{OperationID: request.OperationID, Previous: target, Progress: providerauth.MutationProgress{AccountsSaved: true, ConfigSaved: true, RuntimePublished: true}, Change: &providerauth.Change{OperationID: request.OperationID, Previous: target, Current: state, Models: providerauth.ModelState{Large: model, Small: model}}}, Workspace: view}
}

func TestProviderAuthMutationStrictResponseAndNumbers(t *testing.T) {
	request, response := mutationProtocolFixture()
	require.NoError(t, response.ValidateSwitch(request))
	encoded, err := json.Marshal(response)
	require.NoError(t, err)
	decoded, err := DecodeProviderAuthSwitchResponse(encoded, request)
	require.NoError(t, err)
	require.Equal(t, json.Number("9007199254740993"), decoded.Workspace.Config.Models[config.SelectedModelTypeLarge].ProviderOptions["literal.dotted"])
	require.Equal(t, json.Number("9007199254740993"), decoded.Outcome.Change.Models.Large.Model.ProviderOptions["literal.dotted"])
	valid := string(encoded)
	for name, body := range map[string]string{
		"null": "null", "trailing": valid + "{}", "duplicate": strings.Replace(valid, `"superseded":false`, `"superseded":false,"superseded":false`, 1),
		"missing progress flag": strings.Replace(valid, `"account_refreshed":false,`, "", 1),
		"pointer alias":         strings.Replace(valid, `"current":`, `"Current":`, 1),
		"pointer unknown":       strings.Replace(valid, `"change":{`, `"change":{"unknown":true,`, 1),
		"private owner":         strings.Replace(valid, `"has_oauth":true`, `"has_oauth":true,"account_namespace":"private"`, 1),
		"null change":           strings.Replace(valid, `"change":{`, `"change":null,"ignored":{`, 1),
		"model mismatch":        strings.Replace(valid, `"config":{"models":`, `"config":{"models":`, 1) + " ",
		"wrong workspace":       strings.Replace(valid, `"id":"workspace"`, `"id":"elsewhere"`, 1),
		"unknown config":        strings.Replace(valid, `"config":{`, `"config":{"unexpected":true,`, 1),
		"alias provider":        strings.Replace(valid, `"providers":{"fixture":{`, `"providers":{"fixture":{"ID":"other",`, 1),
		"missing config":        strings.Replace(valid, `"config":{`, `"ignored":{`, 1),
		"null scoped map value": strings.Replace(valid, `"providers":{"fixture":{`, `"providers":{"fixture":null,"ignored":{`, 1),
		"oversized":             strings.Repeat(" ", MaxProviderAuthResponseBytes) + valid,
	} {
		if name == "model mismatch" {
			body = strings.Replace(valid, `"config":{"models":{"large":{"model":"model"`, `"config":{"models":{"large":{"model":"different"`, 1)
			require.NotEqual(t, valid, body)
		}
		t.Run(name, func(t *testing.T) {
			_, err := DecodeProviderAuthSwitchResponse([]byte(body), request)
			require.Error(t, err)
		})
	}
}

func TestProviderAuthMutationEnvelopePartialAndHistorical(t *testing.T) {
	request, response := mutationProtocolFixture()
	response.Workspace = nil
	response.Outcome.Change = nil
	response.Error = NewProviderAuthenticationError(providerauth.ErrMutation)
	encoded, err := json.Marshal(response)
	require.NoError(t, err)
	partial, err := DecodeProviderAuthSwitchResponse(encoded, request)
	require.NoError(t, err)
	require.True(t, partial.Outcome.Progress.RuntimePublished)
	require.ErrorIs(t, partial.Error, providerauth.ErrMutation)
	require.Nil(t, partial.Workspace)
	partial.Error = nil
	require.Error(t, partial.ValidateSwitch(request))
	_, response = mutationProtocolFixture()
	response.Outcome.Superseded = true
	require.Error(t, response.ValidateSwitch(request))
	response.Workspace = nil
	require.NoError(t, response.ValidateSwitch(request))
	response.Error = &ProviderAuthenticationError{Code: "mutation_failed", Message: "private-token"}
	require.Error(t, response.ValidateSwitch(request))
}

func TestProviderAuthSurfaceProjectionCompleteness(t *testing.T) {
	original, public := reflect.TypeFor[providerregistry.Surface](), reflect.TypeFor[AuthenticationProviderSurface]()
	require.Equal(t, original.NumField(), public.NumField(), "new provider metadata needs deliberate auth projection")
	for i := range original.NumField() {
		expected := original.Field(i)
		actual, ok := public.FieldByName(expected.Name)
		require.True(t, ok, expected.Name)
		require.Equal(t, expected.Tag.Get("json"), actual.Tag.Get("json"))
		if expected.Name == "Owner" {
			require.Equal(t, reflect.TypeFor[*providerauth.Owner](), actual.Type)
		} else {
			require.Equal(t, expected.Type, actual.Type)
		}
	}
	var inspect func(reflect.Type)
	inspected := map[reflect.Type]bool{}
	inspect = func(typ reflect.Type) {
		if inspected[typ] {
			return
		}
		inspected[typ] = true
		require.NotEqual(t, reflect.TypeFor[providerregistry.RegistrationOwner](), typ, "public surface cannot embed private owner")
		switch typ.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
			inspect(typ.Elem())
		case reflect.Struct:
			for i := range typ.NumField() {
				if typ.Field(i).IsExported() {
					inspect(typ.Field(i).Type)
				}
			}
		}
	}
	inspect(public)
}

func TestProviderAuthMutationStrictRequests(t *testing.T) {
	request, _ := mutationProtocolFixture()
	data, err := json.Marshal(request)
	require.NoError(t, err)
	decoded, err := DecodeProviderAuthSwitchRequest(data)
	require.NoError(t, err)
	require.Equal(t, request, decoded)
	valid := string(data)
	for _, body := range []string{`{}`, `null`, valid + `{}`, strings.Replace(valid, `"operation_id"`, `"Operation_ID"`, 1), strings.Replace(valid, `"has_oauth":true`, `"has_oauth":null`, 1), strings.Replace(valid, `"account_id":"selected"`, `"account_id":"selected","account_id":"other"`, 1), strings.Replace(valid, `"target":{`, `"target":{"account_namespace":"private",`, 1)} {
		_, err := DecodeProviderAuthSwitchRequest([]byte(body))
		require.Error(t, err)
	}
}
