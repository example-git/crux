package localaddon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/compatibility"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func TestPaginateCodexModels(t *testing.T) {
	models := []any{"a", "b", "c"}
	invalid := "bad"
	page, next, err := paginateCodexModels(nil, &invalid, nil)
	require.NoError(t, err)
	require.Empty(t, page)
	require.Nil(t, next)

	page, next, err = paginateCodexModels(models, nil, nil)
	require.NoError(t, err)
	require.Equal(t, models, page)
	require.Nil(t, next)

	zero := uint32(0)
	page, next, err = paginateCodexModels(models, nil, &zero)
	require.NoError(t, err)
	require.Equal(t, []any{"a"}, page)
	require.Equal(t, "1", next)

	cursor := "1"
	limit := uint32(1)
	page, next, err = paginateCodexModels(models, &cursor, &limit)
	require.NoError(t, err)
	require.Equal(t, []any{"b"}, page)
	require.Equal(t, "2", next)

	cursor = "3"
	page, next, err = paginateCodexModels(models, &cursor, nil)
	require.NoError(t, err)
	require.Empty(t, page)
	require.Nil(t, next)

	cursor = "4"
	_, _, err = paginateCodexModels(models, &cursor, nil)
	require.EqualError(t, err, "cursor 4 exceeds total models 3")
	_, _, err = paginateCodexModels(models, &invalid, nil)
	require.EqualError(t, err, "invalid cursor: bad")
}

func TestPaginateCodexLoadedThreads(t *testing.T) {
	invalid := "not-a-thread-id"
	page, next, err := paginateCodexLoadedThreads(nil, &invalid, nil)
	require.NoError(t, err)
	require.Empty(t, page)
	require.Nil(t, next)

	first := "01900000-0000-7000-8000-000000000001"
	missing := "01900000-0000-7000-8000-000000000002"
	second := "01900000-0000-7000-8000-000000000003"
	third := "01900000-0000-7000-8000-000000000005"
	threads := map[string]*codexThreadBinding{
		third:                                  {loaded: true},
		second:                                 {loaded: true},
		first:                                  {loaded: true},
		"01900000-0000-7000-8000-000000000004": {loaded: false},
	}

	page, next, err = paginateCodexLoadedThreads(threads, nil, nil)
	require.NoError(t, err)
	require.Equal(t, []string{first, second, third}, page)
	require.Nil(t, next)

	zero := uint32(0)
	page, next, err = paginateCodexLoadedThreads(threads, nil, &zero)
	require.NoError(t, err)
	require.Equal(t, []string{first}, page)
	require.Equal(t, first, *next)

	limit := uint32(1)
	page, next, err = paginateCodexLoadedThreads(threads, &first, &limit)
	require.NoError(t, err)
	require.Equal(t, []string{second}, page)
	require.Equal(t, second, *next)

	page, next, err = paginateCodexLoadedThreads(threads, &missing, &limit)
	require.NoError(t, err)
	require.Equal(t, []string{second}, page)
	require.Equal(t, second, *next)

	page, next, err = paginateCodexLoadedThreads(threads, &third, nil)
	require.NoError(t, err)
	require.Empty(t, page)
	require.Nil(t, next)

	_, _, err = paginateCodexLoadedThreads(threads, &invalid, nil)
	require.EqualError(t, err, "invalid cursor: not-a-thread-id")
}

func TestPaginateCodexThreadsSortsAndValidatesRequests(t *testing.T) {
	entries := []codexThreadListEntry{
		{thread: "old", id: "a", createdAt: 1, updatedAt: 30},
		{thread: "new", id: "c", createdAt: 3, updatedAt: 10},
		{thread: "middle", id: "b", createdAt: 2, updatedAt: 20},
		{thread: "same-time", id: "d", createdAt: 2, updatedAt: 25},
	}
	limit := uint32(2)
	page, next, backwards, err := paginateCodexThreads(entries, nil, &limit, "", "")
	require.NoError(t, err)
	require.Equal(t, []any{"new", "same-time"}, page)
	require.NotNil(t, next)
	require.NotNil(t, backwards)

	cursor := next.(string)
	page, next, backwards, err = paginateCodexThreads(entries, &cursor, &limit, "created_at", "desc")
	require.NoError(t, err)
	require.Equal(t, []any{"middle", "old"}, page)
	require.Nil(t, next)
	require.NotNil(t, backwards)

	cursor = backwards.(string)
	page, next, backwards, err = paginateCodexThreads(entries, &cursor, &limit, "created_at", "asc")
	require.NoError(t, err)
	require.Equal(t, []any{"middle", "same-time"}, page)
	require.NotNil(t, next)
	require.NotNil(t, backwards)

	page, next, backwards, err = paginateCodexThreads(entries, nil, nil, "updated_at", "asc")
	require.NoError(t, err)
	require.Equal(t, []any{"new", "middle", "same-time", "old"}, page)
	require.Nil(t, next)
	require.NotNil(t, backwards)

	zero := uint32(0)
	page, next, backwards, err = paginateCodexThreads(entries, nil, &zero, "", "")
	require.NoError(t, err)
	require.Equal(t, []any{"new"}, page)
	require.NotNil(t, next)
	require.NotNil(t, backwards)

	many := make([]codexThreadListEntry, 101)
	for i := range many {
		many[i] = codexThreadListEntry{thread: i, id: "thread", createdAt: int64(i)}
	}
	page, next, backwards, err = paginateCodexThreads(many, nil, nil, "", "")
	require.NoError(t, err)
	require.Len(t, page, 25)
	require.NotNil(t, next)
	require.NotNil(t, backwards)
	aboveMaximum := uint32(101)
	page, next, backwards, err = paginateCodexThreads(many, nil, &aboveMaximum, "", "")
	require.NoError(t, err)
	require.Len(t, page, 100)
	require.NotNil(t, next)
	require.NotNil(t, backwards)

	_, _, _, err = paginateCodexThreads(entries, nil, nil, "recency_at", "")
	require.EqualError(t, err, `thread/list sortKey "recency_at" is not supported`)
	_, _, _, err = paginateCodexThreads(entries, nil, nil, "", "sideways")
	require.EqualError(t, err, `invalid thread/list sortDirection "sideways"`)
	invalid := "bad"
	_, _, _, err = paginateCodexThreads(entries, &invalid, nil, "", "")
	require.EqualError(t, err, "invalid cursor: bad")

	_, next, _, err = paginateCodexThreads(entries, nil, &limit, "", "")
	require.NoError(t, err)
	cursor = next.(string)
	_, _, _, err = paginateCodexThreads(entries, &cursor, nil, "updated_at", "desc")
	require.EqualError(t, err, "cursor sort options do not match request")
}

func TestCodexConfigReadPreservesOptionalConsolidationModel(t *testing.T) {
	view := config.ConfigReadView{Values: map[string]string{"models.large.provider": "openai", "models.large.model": "gpt-5"}, Origins: map[string]config.ConfigReadOrigin{}}
	without := codexConfigViewResult(view)
	require.NotContains(t, without["config"], "memories")
	require.NotContains(t, without, "layers")
	for _, model := range []string{"gpt-memory", ""} {
		view.Values["memories.consolidation_model"] = model
		with := codexConfigViewResult(view)
		require.Equal(t, model, with["config"].(map[string]any)["memories"].(map[string]any)["consolidation_model"])
		require.NotContains(t, with, "layers")
	}
}

func TestCodexNativeModelSelectionPublishesOwnerForDefaultAndRepeatedSelections(t *testing.T) {
	const workspaceID = "workspace-1"
	candidate := config.SelectedModel{Provider: "anthropic", Model: "claude-sonnet"}
	current := config.SelectedModel{Provider: "openai", Model: "gpt-5"}
	owner := providerregistry.RegistrationOwner{
		ProviderID:   candidate.Provider,
		Construction: providerregistry.ConstructionOpenAICompat,
	}
	currentOwner := providerregistry.RegistrationOwner{
		ProviderID:   current.Provider,
		Construction: providerregistry.ConstructionOpenAICompat,
	}

	var mu sync.Mutex
	overrideCalls := 0
	updateCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/workspaces/" + workspaceID + "/config/model-overrides":
			var got proto.ModelOverridesRequest
			require.NoError(t, json.NewDecoder(request.Body).Decode(&got))
			require.NotNil(t, got.State.Large)
			require.Contains(t, []config.OwnedSelectedModel{
				{Model: candidate, Owner: owner},
				{Model: current, Owner: currentOwner},
			}, *got.State.Large)
			mu.Lock()
			overrideCalls++
			mu.Unlock()
			require.NoError(t, json.NewEncoder(writer).Encode(got.State))
		case "/v1/workspaces/" + workspaceID + "/config/model":
			http.Error(writer, "persistent model update is forbidden", http.StatusConflict)
		case "/v1/workspaces/" + workspaceID + "/agent/update":
			var got proto.AgentUpdateRequest
			require.NoError(t, json.NewDecoder(request.Body).Decode(&got))
			require.NotNil(t, got.State.Large)
			require.Contains(t, []config.OwnedSelectedModel{
				{Model: candidate, Owner: owner},
				{Model: current, Owner: currentOwner},
			}, *got.State.Large)
			mu.Lock()
			updateCalls++
			call := updateCalls
			mu.Unlock()
			if call == 1 {
				http.Error(writer, "update rejected", http.StatusConflict)
				return
			}
			writer.WriteHeader(http.StatusOK)
		default:
			http.Error(writer, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()
	serverURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	api, err := client.NewClient(t.TempDir(), "tcp", serverURL.Host)
	require.NoError(t, err)

	workspace := &codexNativeWorkspace{client: api, value: &proto.Workspace{ID: workspaceID}}
	bridge := &codexNativeBridge{
		ctx: context.Background(),
		models: map[*codexNativeWorkspace]map[string]config.SelectedModel{
			workspace: {"anthropic/claude-sonnet": candidate},
		},
		modelOwners: map[*codexNativeWorkspace]map[string]providerregistry.RegistrationOwner{
			workspace: {candidate.Provider: owner, current.Provider: currentOwner},
		},
		selectedModel: map[*codexNativeWorkspace]config.SelectedModel{workspace: current},
	}

	_, err = bridge.selectModel(t.Context(), workspace, "anthropic/claude-sonnet", "")
	require.Error(t, err)
	require.Equal(t, current, bridge.selectedModel[workspace])

	selected, err := bridge.selectModel(t.Context(), workspace, "anthropic/claude-sonnet", "")
	require.NoError(t, err)
	require.Equal(t, candidate, selected)
	require.Equal(t, candidate, bridge.selectedModel[workspace])

	selected, err = bridge.selectModel(t.Context(), workspace, "anthropic/claude-sonnet", "")
	require.NoError(t, err)
	require.Equal(t, candidate, selected)

	selected, err = bridge.selectModel(t.Context(), workspace, "", "")
	require.NoError(t, err)
	require.Equal(t, candidate, selected)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 5, overrideCalls)
	require.Equal(t, 5, updateCalls)
}

func TestCodexAppServerRetainsThreadModels(t *testing.T) {
	type submission struct {
		session, provider, model string
	}
	submissions := make(chan submission, 8)
	workingDir := nativeAPIFixture(t, "done", func(session, provider, model string, _ proto.AgentMessage) {
		submissions <- submission{session, provider, model}
	})
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	t.Cleanup(func() {
		_ = inputWriter.Close()
		_ = outputReader.Close()
	})
	request := compatibility.Request{
		Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir,
		Prompt:  compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader},
		Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true},
	}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	read := func() map[string]any {
		var response map[string]any
		require.NoError(t, decoder.Decode(&response))
		return response
	}
	send := func(method string, params map[string]any) {
		require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": method, "params": params}))
	}
	send("initialize", map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}})
	require.NotNil(t, read()["result"])
	send("model/list", map[string]any{})
	models := read()["result"].(map[string]any)["data"].([]any)
	aliases := make(map[string]string)
	for _, raw := range models {
		model := raw.(map[string]any)
		require.Equal(t, model["id"], model["model"])
		require.Equal(t, model["id"], model["displayName"])
		aliases[model["id"].(string)] = model["model"].(string)
	}
	require.Len(t, aliases, 3)
	start := func(model string) string {
		send("thread/start", map[string]any{"cwd": workingDir, "model": model})
		response := read()
		require.Nil(t, response["error"])
		result := response["result"].(map[string]any)
		require.Equal(t, model, result["model"])
		require.Equal(t, model, result["thread"].(map[string]any)["model"])
		id := result["thread"].(map[string]any)["id"].(string)
		require.Equal(t, "thread/started", read()["method"])
		return id
	}
	first := start(aliases["OPENAI-GPT-5"])
	second := start(aliases["ANTHROPIC-CLAUDE-SONNET"])
	send("thread/resume", map[string]any{"threadId": second, "model": aliases["ANTHROPIC-CLAUDE-SONNET"], "modelProvider": "openai"})
	require.Contains(t, read()["error"].(map[string]any)["message"], "the public provider is crux")
	send("thread/resume", map[string]any{"threadId": second, "model": aliases["ANTHROPIC-CLAUDE-SONNET"], "modelProvider": "crux"})
	resumed := read()
	require.Nil(t, resumed["error"])
	require.Equal(t, aliases["ANTHROPIC-CLAUDE-SONNET"], resumed["result"].(map[string]any)["model"])
	send("thread/read", map[string]any{"threadId": first, "model": "openai/gpt-4.1"})
	readResponse := read()
	require.Nil(t, readResponse["error"])
	require.Equal(t, first, readResponse["result"].(map[string]any)["thread"].(map[string]any)["id"])
	turn := func(thread, model string, expected submission) {
		params := map[string]any{"threadId": thread, "input": []any{map[string]any{"type": "text", "text": "hello"}}}
		if model != "" {
			params["model"] = model
		}
		send("turn/start", params)
		for {
			response := read()
			require.Nil(t, response["error"])
			if response["method"] == "turn/completed" {
				require.Equal(t, "completed", response["params"].(map[string]any)["turn"].(map[string]any)["status"])
				break
			}
		}
		require.Equal(t, expected, <-submissions)
	}
	turn(first, "", submission{"thread-1", "openai", "gpt-5"})
	turn(second, "", submission{"thread-2", "anthropic", "claude-sonnet"})
	turn(first, aliases["OPENAI-GPT-4-1"], submission{"thread-1", "openai", "gpt-4.1"})
	turn(second, "", submission{"thread-2", "anthropic", "claude-sonnet"})
	send("turn/start", map[string]any{"threadId": first, "model": "missing-model"})
	require.NotNil(t, read()["error"])
	turn(first, "", submission{"thread-1", "openai", "gpt-4.1"})
	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}
