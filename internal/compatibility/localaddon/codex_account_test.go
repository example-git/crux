package localaddon

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/compatibility"
	"github.com/stretchr/testify/require"
)

func TestCodexAccountRemainsLoggedInWithoutNativeBackend(t *testing.T) {
	previous := codexClientFactory
	calls := 0
	codexClientFactory = func(string) (*client.Client, error) {
		calls++
		return nil, errors.New("selected provider owner is unavailable")
	}
	t.Cleanup(func() { codexClientFactory = previous })
	workingDir := t.TempDir()
	input := io.NopCloser(strings.NewReader(`{"id":"initialize","method":"initialize","params":{"clientInfo":{"name":"desktop","version":"1"}}}
{"method":"initialized"}
{"id":"account-before","method":"account/read","params":{"refreshToken":true}}
{"id":"gateway-before","method":"account/gatewayOAuth/read"}
{"id":"auth-before","method":"getAuthStatus","params":{"includeToken":true,"refreshToken":true}}
{"id":"login","method":"account/login/start","params":{"type":"chatgpt"}}
{"id":"logout","method":"account/logout"}
{"id":"account-after-logout","method":"account/read"}
{"id":"gateway-after-logout","method":"account/gatewayOAuth/read","params":{}}
{"id":"unsupported","method":"unsupported/operation"}
{"id":"models","method":"model/list"}
{"id":"account-after-failure","method":"account/read"}
{"id":"gateway-after-failure","method":"account/gatewayOAuth/read","params":null}
{"id":"auth-after-failure","method":"getAuthStatus","params":null}
{"id":"invalid","method":"getAuthStatus","params":{"includeToken":"invalid"}}
`))
	var output bytes.Buffer
	request := compatibility.Request{
		Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir,
		Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: input}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true},
	}
	require.NoError(t, runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, input, &output), request))
	require.Equal(t, 1, calls)
	responses := decodeJSONLines(t, output.Bytes())
	require.Len(t, responses, 14)
	for _, response := range responses {
		switch id := response["id"].(string); id {
		case "account-before", "account-after-logout", "account-after-failure":
			require.Equal(t, codexAccountReadResult(), response["result"])
		case "gateway-before", "gateway-after-logout", "gateway-after-failure":
			require.Equal(t, codexGatewayOAuthReadResult(), response["result"])
		case "auth-before", "auth-after-failure":
			require.Equal(t, codexLegacyAuthStatusResult(), response["result"])
		case "login":
			require.Equal(t, map[string]any{"type": "apiKey"}, response["result"])
		case "logout":
			require.Equal(t, map[string]any{}, response["result"])
		case "models":
			require.Equal(t, float64(-32000), response["error"].(map[string]any)["code"])
			require.Contains(t, response["error"].(map[string]any)["message"], "selected provider owner is unavailable")
		case "unsupported":
			require.Equal(t, float64(-32601), response["error"].(map[string]any)["code"])
		case "invalid":
			require.Equal(t, float64(-32600), response["error"].(map[string]any)["code"])
		default:
			require.Equal(t, "initialize", id)
			require.NotContains(t, response, "error")
			userAgent := response["result"].(map[string]any)["userAgent"].(string)
			require.True(t, strings.HasPrefix(userAgent, "codex_cli_rs/9.9.9 ("), userAgent)
			require.NotContains(t, strings.ToLower(userAgent), "crux")
		}
	}
}
