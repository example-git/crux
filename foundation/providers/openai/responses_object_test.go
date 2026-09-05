package openai

import (
	"net/http"
	"net/http/httptest"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/stretchr/testify/require"
)

func TestResponsesGenerateObjectNoTextUsesNormalizedUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"gpt-test","output":[],"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":4},"output_tokens":3,"output_tokens_details":{"reasoning_tokens":2},"total_tokens":13}}`))
	}))
	defer server.Close()

	provider, err := New(WithAPIKey("test-key"), WithBaseURL(server.URL), WithUseResponsesAPI(), WithResponsesAPIFunc(func(string) bool { return true }))
	require.NoError(t, err)
	model, err := provider.LanguageModel(t.Context(), "gpt-test")
	require.NoError(t, err)

	_, err = model.GenerateObject(t.Context(), fantasy.ObjectCall{
		Prompt: fantasy.Prompt{fantasy.NewUserMessage("return an object")},
		Schema: fantasy.Schema{Type: "object", Properties: map[string]*fantasy.Schema{"answer": {Type: "string"}}, Required: []string{"answer"}},
	})
	var noObject *fantasy.NoObjectGeneratedError
	require.ErrorAs(t, err, &noObject)
	require.ErrorContains(t, noObject.ParseError, "no text content")
	require.Equal(t, int64(6), noObject.Usage.InputTokens)
	require.Equal(t, int64(3), noObject.Usage.OutputTokens)
	require.Equal(t, int64(13), noObject.Usage.TotalTokens)
	require.Equal(t, int64(2), noObject.Usage.ReasoningTokens)
	require.Equal(t, int64(4), noObject.Usage.CacheReadTokens)
}
