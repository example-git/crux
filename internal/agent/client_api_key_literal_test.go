package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth/accounts"
	openairesponsestransport "github.com/example-git/crux/internal/providertransport/openairesponses"
	"github.com/stretchr/testify/require"
)

func TestClientResponsesExplicitAPIKeyCredentialHTTPS(t *testing.T) {
	for _, key := range []string{"HOME", "USERPROFILE", "AI_CLI_DIR"} {
		t.Setenv(key, t.TempDir())
	}
	marker := filepath.Join(t.TempDir(), "must-not-execute")
	literal := "synthetic-$(printf x > " + marker + ")-$UNSET"
	var mu sync.Mutex
	var headers []string
	var requests []map[string]any
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		mu.Lock()
		headers = append(headers, r.Header.Get("Authorization"))
		requests = append(requests, request)
		id := fmt.Sprintf("resp_%d", len(requests))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":%q,\"status\":\"in_progress\",\"output\":[]}}\n\n", id)
		_, _ = fmt.Fprint(w, "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"msg\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"in_progress\",\"content\":[]}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"item_id\":\"msg\",\"delta\":\"answer\"}\n\n")
		_, _ = fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"msg\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"answer\",\"annotations\":[]}]}}\n\n")
		_, _ = fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":%q,\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n", id)
	}))
	t.Cleanup(host.Close)
	oldTransport := http.DefaultTransport
	http.DefaultTransport = host.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	proposal := clientResponsesProposalWithAPIKey(t, host.URL, literal)
	root := t.TempDir()
	store, err := config.CompileRemoteRuntime(root, filepath.Join(root, "workspace"), false, proposal, strings.Repeat("a", 64), env.NewFromMap(map[string]string{"HOME": root}))
	require.NoError(t, err)
	owner := proposal.Credentials[0].Owner
	require.NoError(t, accounts.Save(t.Context(), owner.AccountNamespace, accounts.Entry{ID: "unrelated-account", AccessToken: literal}))
	coord := &coordinator{cfg: store, responsesContinuations: openairesponsestransport.NewContinuationStore()}
	model, _, err := coord.buildAgentModelsWithSnapshot(t.Context(), config.Agent{Model: config.SelectedModelTypeLarge}, false, store.RuntimeSnapshot())
	require.NoError(t, err, "the explicit API-key slot must not require a forwarded OAuth account")
	call := fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("one")}, Headers: map[string]string{"x-session-id": "same-session", "x-request-purpose": "conversation"}}
	for range 2 {
		stream, err := model.Model.Stream(t.Context(), call)
		require.NoError(t, err)
		for part := range stream {
			require.NoError(t, part.Error)
		}
		call.Prompt = append(call.Prompt, fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "answer"}}}, fantasy.NewUserMessage("next"))
	}
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"Bearer " + literal, "Bearer " + literal}, headers)
	require.Len(t, requests, 2)
	require.Equal(t, "resp_1", requests[1]["previous_response_id"])
	require.NoFileExists(t, marker)
}
