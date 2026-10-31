package localaddon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/example-git/crux/internal/config"
	validator "github.com/kaptinlin/jsonschema"
	"github.com/stretchr/testify/require"
)

func TestCodexLegacyAuthStatusTokenRestrictions(t *testing.T) {
	require.Equal(t, map[string]any{"authMethod": "apikey", "authToken": nil, "requiresOpenaiAuth": false}, codexLegacyAuthStatusResult())
}

func TestCodexPreparationGeneratedSchemas(t *testing.T) {
	out := t.TempDir()
	require.NoError(t, writeCodexJSONSchemas(out))
	require.NoError(t, writeCodexTypeScriptSchemas(out))
	for _, path := range []string{"GetAuthStatusParams.ts", "GetAuthStatusResponse.ts", "v2/GetAccountResponse.ts", "v2/GatewayOAuthReadResponse.ts", "v2/LoginAccountResponse.ts", "v2/LogoutAccountResponse.ts", "v2/ConfigReadResponse.ts"} {
		data, err := os.ReadFile(filepath.Join(out, path))
		require.NoError(t, err)
		require.NotEmpty(t, data)
	}
	for _, test := range []struct {
		path    string
		valid   []string
		invalid []string
	}{
		{path: "GetAuthStatusParams.json", valid: []string{`{}`, `{"includeToken":null,"refreshToken":null}`, `{"includeToken":true,"refreshToken":false}`}, invalid: []string{`[]`, `{"includeToken":"true"}`, `{"refreshToken":1}`}},
		{path: "GetAuthStatusResponse.json", valid: []string{`{"authMethod":"apikey","authToken":null,"requiresOpenaiAuth":false}`}, invalid: []string{`{}`, `{"authMethod":null,"authToken":null,"requiresOpenaiAuth":false}`, `{"authMethod":"apikey","authToken":"synthetic-token","requiresOpenaiAuth":false}`, `{"authMethod":"chatgpt","authToken":null,"requiresOpenaiAuth":true}`}},
		{path: "v2/GetAccountResponse.json", valid: []string{`{"account":{"type":"apiKey"},"requiresOpenaiAuth":false}`}, invalid: []string{`{"account":null,"requiresOpenaiAuth":false}`, `{"account":{"type":"apiKey"},"requiresOpenaiAuth":true}`}},
		{path: "v2/GatewayOAuthReadResponse.json", valid: []string{`{"providerId":"crux","providerName":"Crux","required":false,"status":null,"error":null}`}, invalid: []string{`{}`, `{"providerId":"provider","providerName":"Provider","required":false,"status":null,"error":null}`, `{"providerId":"crux","providerName":"Crux","required":true,"status":"succeeded","error":null}`}},
		{path: "v2/LoginAccountResponse.json", valid: []string{`{"type":"apiKey"}`}, invalid: []string{`{}`, `{"type":"chatgpt"}`}},
		{path: "v2/LogoutAccountResponse.json", valid: []string{`{}`}, invalid: []string{`null`, `{"token":"secret"}`}},
		{path: "ClientRequest.json", valid: []string{`{"id":1,"method":"getAuthStatus"}`, `{"id":1,"method":"getAuthStatus","params":null}`, `{"id":1,"method":"getAuthStatus","params":{"includeToken":null}}`, `{"id":1,"method":"config/read","params":{"includeLayers":false}}`, `{"id":"login","method":"account/login/start","params":{"type":"chatgpt"}}`, `{"id":"logout","method":"account/logout","params":null}`, `{"id":"gateway","method":"account/gatewayOAuth/read"}`, `{"id":"gateway","method":"account/gatewayOAuth/read","params":null}`}, invalid: []string{`{"id":1,"method":"getAuthStatus","params":{"refreshToken":1}}`}},
	} {
		t.Run(test.path, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(out, test.path))
			require.NoError(t, err)
			compiled, err := validator.NewCompiler().Compile(data)
			require.NoError(t, err)
			for _, value := range test.valid {
				require.True(t, compiled.ValidateJSON([]byte(value)).IsValid(), value)
			}
			for _, value := range test.invalid {
				require.False(t, compiled.ValidateJSON([]byte(value)).IsValid(), value)
			}
		})
	}
	data, err := os.ReadFile(filepath.Join(out, "v2/ConfigReadResponse.json"))
	require.NoError(t, err)
	compiled, err := validator.NewCompiler().Compile(data)
	require.NoError(t, err)
	for _, kind := range []string{"system", "user", "project", "workspace", "runtime"} {
		origin := config.ConfigReadOrigin{Kind: kind, Version: "sha256:synthetic"}
		if kind != "runtime" {
			origin.Path = filepath.Join(out, "crux.json")
		}
		view := config.ConfigReadView{Values: map[string]string{"models.large.model": "fixture", "memories.consolidation_model": "memory"}, Origins: map[string]config.ConfigReadOrigin{"models.large.model": origin, "memories.consolidation_model": origin}}
		body, err := json.Marshal(codexConfigViewResult(view))
		require.NoError(t, err)
		result := compiled.ValidateJSON(body)
		require.True(t, result.IsValid(), "%s: %+v", kind, result.Errors)
	}
	for _, body := range []string{`{}`, `{"config":{},"origins":{},"layers":[]}`, `{"config":{"api_key":"secret"},"origins":{}}`, `{"config":{},"origins":{"model":{"name":{"type":"user"},"version":"v"}}}`} {
		require.False(t, compiled.ValidateJSON([]byte(body)).IsValid(), body)
	}
	require.Contains(t, codexSupportedMethods(), "getAuthStatus")
	require.Contains(t, codexSupportedMethods(), "config/read")
	require.Contains(t, codexSupportedMethods(), "account/gatewayOAuth/read")
}
