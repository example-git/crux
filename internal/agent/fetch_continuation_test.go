package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/agent/tools"
	"github.com/stretchr/testify/require"
)

type fetchContinuationModel struct {
	finishStreamModel
	input   string
	prompts []fantasy.Prompt
}

func (m *fetchContinuationModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.prompts = append(m.prompts, cloneFantasyMessages(call.Prompt))
	if len(m.prompts) > 1 {
		return (&finishStreamModel{text: "continued after fetch"}).Stream(ctx, call)
	}
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "fetch-result", ToolCallName: tools.FetchToolName, ToolCallInput: m.input}) {
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
	}, nil
}

func TestFetchFailureContinuesSessionTurn(t *testing.T) {
	for _, kind := range []string{"connection-refused", "request-timeout", "http-error", "success"} {
		t.Run(kind, func(t *testing.T) {
			env := testEnv(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch kind {
				case "request-timeout":
					<-r.Context().Done()
				case "http-error":
					w.WriteHeader(http.StatusInternalServerError)
				default:
					_, _ = fmt.Fprint(w, "fetched content")
				}
			}))
			defer server.Close()
			if kind == "connection-refused" {
				server.Close()
			}
			params := tools.FetchParams{URL: server.URL, Format: "text"}
			if kind == "request-timeout" {
				params.Timeout = 1
			}
			input, err := json.Marshal(params)
			require.NoError(t, err)
			model := &fetchContinuationModel{input: string(input)}
			tool := tools.NewFetchTool(env.permissions, env.workingDir, server.Client(), nil, nil)
			a := testSessionAgent(env, model, &finishStreamModel{text: "title"}, "system", tool)
			current, err := env.sessions.Create(t.Context(), "fetch continuation")
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			_, err = a.Run(ctx, SessionAgentCall{SessionID: current.ID, Prompt: "Fetch the page and continue after its result"})
			require.NoError(t, err)
			require.Len(t, model.prompts, 2)
			var followup fantasy.ToolResultOutputContent
			for _, msg := range model.prompts[1] {
				for _, part := range msg.Content {
					if result, ok := part.(fantasy.ToolResultPart); ok && result.ToolCallID == "fetch-result" {
						require.Nil(t, followup)
						followup = result.Output
					}
				}
			}
			require.NotNil(t, followup)
			stored, err := env.messages.List(t.Context(), current.ID)
			require.NoError(t, err)
			results := 0
			for _, msg := range stored {
				for _, result := range msg.ToolResults() {
					if result.ToolCallID == "fetch-result" {
						results++
						require.Equal(t, kind != "success", result.IsError)
						require.NotEmpty(t, result.Content)
						if result.IsError {
							output, ok := followup.(fantasy.ToolResultOutputContentError)
							require.True(t, ok)
							require.EqualError(t, output.Error, result.Content)
						} else {
							output, ok := followup.(fantasy.ToolResultOutputContentText)
							require.True(t, ok)
							require.Equal(t, result.Content, output.Text)
						}
					}
				}
			}
			require.Equal(t, 1, results)
			require.Contains(t, stored[len(stored)-1].Content().Text, "continued after fetch")
		})
	}
}
