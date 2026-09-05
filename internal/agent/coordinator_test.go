package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/foundation/providers/openai"
	"github.com/example-git/crux/foundation/providers/openaicompat"
	"github.com/example-git/crux/internal/agent/notify"
	"github.com/example-git/crux/internal/automemory"
	"github.com/example-git/crux/internal/codebaseindex"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/imageattachment"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/oauth/codex"
	codexresponses "github.com/example-git/crux/internal/oauth/codex/responses"
	"github.com/example-git/crux/internal/oauth/gemini"
	"github.com/example-git/crux/internal/oauth/gemini/antigravity"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/providertransport"
	openairesponsestransport "github.com/example-git/crux/internal/providertransport/openairesponses"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/example-git/crux/internal/session"
	"github.com/example-git/crux/internal/skills"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func integratedRegistration(t *testing.T, providerID string) (providerregistry.Registration, bool) {
	t.Helper()
	registry, err := providerregistry.New(providerregistry.Integrated()...)
	require.NoError(t, err)
	return registry.Lookup(providerID)
}

// mockSessionAgent is a minimal mock for the SessionAgent interface.
type mockSessionAgent struct {
	model                Model
	smallModel           Model
	instructions         fantasy.Instructions
	tools                []fantasy.AgentTool
	disableAutoSummarize bool
	runtime              InstalledRuntime
	runFunc              func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error)
	summarizeRuntimeFunc func(InstalledRuntime) error
	cancelled            []string
}

func (m *mockSessionAgent) Run(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
	return m.runFunc(ctx, call)
}

func (m *mockSessionAgent) BeginAccepted(sessionID string) *AcceptedRun {
	return &AcceptedRun{sessionID: sessionID}
}

func (m *mockSessionAgent) Model() Model                                      { return m.model }
func (m *mockSessionAgent) SetModels(large, small Model)                      {}
func (m *mockSessionAgent) SetTools(tools, planModeTools []fantasy.AgentTool) {}
func (m *mockSessionAgent) SetInstructions(instructions fantasy.Instructions) {}
func (m *mockSessionAgent) SetSystemPrompt(systemPrompt string)               {}
func (m *mockSessionAgent) SetRuntime(runtime InstalledRuntime) {
	m.model = runtime.LargeModel
	m.smallModel = runtime.SmallModel
	m.instructions = runtime.Instructions
	m.tools = runtime.Tools
	m.disableAutoSummarize = runtime.DisableAutoSummarize
	m.runtime = runtime
}

func (m *mockSessionAgent) Runtime() InstalledRuntime {
	runtime := m.runtime
	if runtime.LargeModel.Model == nil {
		runtime.LargeModel = m.model
		runtime.SmallModel = m.smallModel
	}
	return runtime
}

func (m *mockSessionAgent) Cancel(sessionID string) {
	m.cancelled = append(m.cancelled, sessionID)
}
func (m *mockSessionAgent) CancelAll()                                        {}
func (m *mockSessionAgent) IsSessionBusy(sessionID string) bool               { return false }
func (m *mockSessionAgent) IsBusy() bool                                      { return false }
func (m *mockSessionAgent) QueuedPrompts(sessionID string) int                { return 0 }
func (m *mockSessionAgent) QueuedPromptsList(sessionID string) []QueuedPrompt { return nil }
func (m *mockSessionAgent) ClearQueue(sessionID string)                       {}
func (m *mockSessionAgent) Summarize(context.Context, string, fantasy.ProviderOptions, func(context.Context, *fantasy.ProviderError) error) error {
	return nil
}

func (m *mockSessionAgent) SummarizeWithRuntime(_ context.Context, _ string, _ fantasy.ProviderOptions, _ func(context.Context, *fantasy.ProviderError) error, runtime InstalledRuntime) error {
	if m.summarizeRuntimeFunc != nil {
		return m.summarizeRuntimeFunc(runtime)
	}
	return nil
}
func (m *mockSessionAgent) GenerateTitle(context.Context, string, string) {}
func (m *mockSessionAgent) GenerateMemory(context.Context, string, string, int64) (string, error) {
	return `{"memories":[]}`, nil
}

func (m *mockSessionAgent) SuggestPrompt(context.Context, string) (string, error) { return "", nil }

func TestInstructionPolicyUsesProviderConstruction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		construction providerregistry.Construction
		want         fantasy.InstructionPolicy
	}{
		{name: "Anthropic Messages", construction: providerregistry.ConstructionAnthropicMessages, want: fantasy.InstructionPolicyAnthropic},
		{name: "Codex", construction: providerregistry.ConstructionCodex, want: fantasy.InstructionPolicyCodex},
		{name: "generic", construction: providerregistry.ConstructionGenericJSON, want: fantasy.InstructionPolicyGeneric},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, test.want, instructionPolicyForConstruction(test.construction))
		})
	}
}

func TestFilterSkillSnapshotHidesOnlyConfiguredSkills(t *testing.T) {
	t.Parallel()

	imagegen := &skills.Skill{Name: "imagegen"}
	configSkill := &skills.Skill{Name: "crux-config"}
	snapshot := skills.Snapshot{
		AllSkills:    []*skills.Skill{imagegen, configSkill},
		ActiveSkills: []*skills.Skill{imagegen, configSkill},
	}

	filtered := filterSkillSnapshot(snapshot, []string{"imagegen"})
	require.Equal(t, []*skills.Skill{configSkill}, filtered.ActiveSkills)
	require.Equal(t, []*skills.Skill{imagegen, configSkill}, filtered.AllSkills)
	require.Equal(t, []*skills.Skill{imagegen, configSkill}, snapshot.ActiveSkills)

	unfiltered := filterSkillSnapshot(snapshot, nil)
	require.Equal(t, snapshot.ActiveSkills, unfiltered.ActiveSkills)
}

func TestDiscoverSkillSnapshotReloadsPathsContentAndDisabledState(t *testing.T) {
	workingDir := t.TempDir()
	firstRoot := t.TempDir()
	firstDir := filepath.Join(firstRoot, "reload-skill")
	require.NoError(t, os.MkdirAll(firstDir, 0o755))
	skillFile := filepath.Join(firstDir, skills.SkillFileName)
	require.NoError(t, os.WriteFile(skillFile, []byte("---\nname: reload-skill\ndescription: Reload test.\n---\nversion one\n"), 0o644))

	cfg := initTestConfig(t, workingDir)
	require.NoError(t, cfg.SetConfigField(config.ScopeWorkspace, "options.skills_paths", []string{firstRoot}))
	first := discoverSkillSnapshot(cfg)
	firstSkill := skillNamed(first.ActiveSkills, "reload-skill")
	require.NotNil(t, firstSkill)
	require.Equal(t, "version one", strings.TrimSpace(firstSkill.Instructions))

	require.NoError(t, os.WriteFile(skillFile, []byte("---\nname: reload-skill\ndescription: Reload test.\n---\nversion two\n"), 0o644))
	contentReload := discoverSkillSnapshot(cfg)
	contentSkill := skillNamed(contentReload.ActiveSkills, "reload-skill")
	require.NotNil(t, contentSkill)
	require.Equal(t, "version two", strings.TrimSpace(contentSkill.Instructions))

	require.NoError(t, cfg.SetConfigField(config.ScopeWorkspace, "options.disabled_skills", []string{"reload-skill"}))
	disabledReload := discoverSkillSnapshot(cfg)
	require.NotNil(t, skillNamed(disabledReload.AllSkills, "reload-skill"))
	require.Nil(t, skillNamed(disabledReload.ActiveSkills, "reload-skill"))

	secondRoot := t.TempDir()
	secondDir := filepath.Join(secondRoot, "added-skill")
	require.NoError(t, os.MkdirAll(secondDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(secondDir, skills.SkillFileName), []byte("---\nname: added-skill\ndescription: Added path.\n---\nadded\n"), 0o644))
	require.NoError(t, cfg.SetConfigField(config.ScopeWorkspace, "options.skills_paths", []string{firstRoot, secondRoot}))
	pathReload := discoverSkillSnapshot(cfg)
	require.NotNil(t, skillNamed(pathReload.ActiveSkills, "added-skill"))
	require.Contains(t, pathReload.ResolvedPaths, secondRoot)
}

func skillNamed(items []*skills.Skill, name string) *skills.Skill {
	for _, item := range items {
		if item.Name == name {
			return item
		}
	}
	return nil
}

// newTestCoordinator creates a minimal coordinator for unit testing runSubAgent.
func newTestCoordinator(t *testing.T, env fakeEnv, providerID string, providerCfg config.ProviderConfig) *coordinator {
	cfg := initTestConfig(t, env.workingDir)
	cfg.Config().Providers.Set(providerID, providerCfg)
	return &coordinator{
		cfg:      cfg,
		sessions: env.sessions,
		messages: env.messages,
	}
}

func TestOpenAICompatExtraBodyDoesNotApplyLegacyDefaultsToExactOwner(t *testing.T) {
	providerCfg := config.ProviderConfig{
		ID:        string(catalog.ProviderZAI),
		ExtraBody: map[string]any{"user": true},
	}
	legacy := openAICompatExtraBody(providerCfg)
	require.Equal(t, true, legacy["user"])
	require.Equal(t, true, legacy["tool_stream"])
	require.NotContains(t, providerCfg.ExtraBody, "tool_stream")

	for _, exactOwner := range []config.ProviderConfig{
		{ID: providerCfg.ID, ExtraBody: providerCfg.ExtraBody, Preset: &config.ProviderPresetReference{ID: "example.zai-preset", Version: "1"}},
		{ID: providerCfg.ID, ExtraBody: providerCfg.ExtraBody, Plugin: &config.ProviderPluginReference{ID: "example.zai-plugin", Version: "1"}},
	} {
		extraBody := openAICompatExtraBody(exactOwner)
		require.Equal(t, map[string]any{"user": true}, extraBody)
	}
}

func TestManifestCompactionUsesExactRegistrationPolicy(t *testing.T) {
	t.Parallel()

	declared := &manifest.CompactionPolicy{
		Mode:                "local-summary",
		RetainedTokenBudget: 30_000,
		PreserveToolPairs:   true,
	}
	registration := providerregistry.Registration{
		Operation: &providertransport.Operation{Compaction: declared},
	}
	policy, retry, err := manifestCompaction(registration, true)
	require.NoError(t, err)
	require.Equal(t, declared, policy)
	require.NotSame(t, declared, policy)
	require.Nil(t, retry)

	policy, retry, err = manifestCompaction(registration, false)
	require.NoError(t, err)
	require.Nil(t, policy)
	require.Nil(t, retry)

	registration.Operation.Compaction = &manifest.CompactionPolicy{Mode: "none"}
	policy, retry, err = manifestCompaction(registration, true)
	require.NoError(t, err)
	require.Equal(t, "none", policy.Mode)
	require.Nil(t, retry)

	remoteRetry := manifest.RetryPolicy{
		MaxAttempts:       3,
		Statuses:          []int{503},
		Codes:             []string{"temporarily_unavailable"},
		TransportErrors:   true,
		UnexpectedEOF:     true,
		Authentication:    "refresh-once",
		ReplayRequirement: "before-first-event",
	}
	registration.Operation.Compaction = &manifest.CompactionPolicy{Mode: "remote-operation", Operation: "remote-compact"}
	registration.Operations = map[string]*providertransport.Operation{
		"remote-compact": {Retry: remoteRetry},
	}
	policy, retry, err = manifestCompaction(registration, true)
	require.NoError(t, err)
	require.Equal(t, registration.Operation.Compaction, policy)
	require.Equal(t, remoteRetry, *retry)
	retry.Statuses[0] = 429
	retry.Codes[0] = "changed"
	require.Equal(t, []int{503}, registration.Operations["remote-compact"].Retry.Statuses)
	require.Equal(t, []string{"temporarily_unavailable"}, registration.Operations["remote-compact"].Retry.Codes)

	delete(registration.Operations, "remote-compact")
	_, _, err = manifestCompaction(registration, true)
	require.ErrorContains(t, err, "remote operation")

	registration.Operation.Compaction = &manifest.CompactionPolicy{Mode: "unsupported"}
	_, _, err = manifestCompaction(registration, true)
	require.ErrorContains(t, err, "unsupported")
}

func TestBuildOpenAIProviderAppliesCompiledOperationPolicy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		require.Equal(t, "/v1/responses", request.URL.Path)
		body, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		var document map[string]any
		require.NoError(t, json.Unmarshal(body, &document))
		require.Equal(t, true, document["policy_applied"])
		require.Equal(t, "max", document["reasoning"].(map[string]any)["effort"])
		response.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(response).Encode(map[string]any{
			"id": "response-1", "object": "response", "model": "model-one", "status": "completed",
			"output": []any{map[string]any{
				"type": "message", "id": "message-1", "role": "assistant", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": "generated", "annotations": []any{}}},
			}},
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
		}))
	}))
	defer server.Close()
	cfg := initTestConfig(t, t.TempDir())
	cfg.Config().Options.AnalysisEffort = "max"
	coord := &coordinator{cfg: cfg}
	registration := providerregistry.Registration{
		ProviderID: "synthetic", Construction: providerregistry.ConstructionOpenAIResponses,
		RuntimeControls: []manifest.RuntimeControl{{ID: "effort", Type: "enum", Values: []string{"low", "max"}, Default: "low", Scope: "model", RequestPath: "/reasoning/effort"}},
		Operation: &providertransport.Operation{
			ID: "inference", Key: providertransport.Key{Protocol: string(providerregistry.ConstructionOpenAIResponses), Transport: "sse"},
			Endpoint: manifest.Endpoint{BaseURL: server.URL}, Method: http.MethodPost, Path: "/v1/responses",
			RequestTransform: &manifest.JSONPipeline{MaxOperations: 1, Operations: []manifest.JSONOperation{{
				Operation: "set", Path: "/policy_applied", Value: &manifest.Template{Kind: "literal", Value: true},
			}}},
		},
	}
	provider, err := coord.buildOpenaiProvider(false, cfg.Config().Options, registration, server.URL, "key", nil, providertransport.TemplateValues{}, func() error { return nil })
	require.NoError(t, err)
	model, err := provider.LanguageModel(t.Context(), "model-one")
	require.NoError(t, err)
	result, err := model.Generate(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hello")}})
	require.NoError(t, err)
	require.Equal(t, "generated", result.Content.Text())
}

func TestBuildAgentModelsRetainsNativeResponsesContinuationAcrossRebuilds(t *testing.T) {
	const (
		providerID = "synthetic-responses"
		modelID    = "response-model"
	)
	var mu sync.Mutex
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		var document map[string]any
		require.NoError(t, json.Unmarshal(body, &document))
		mu.Lock()
		requests = append(requests, document)
		responseID := fmt.Sprintf("resp_%d", len(requests))
		mu.Unlock()
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(response, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":%q,\"status\":\"in_progress\",\"output\":[]}}\n\n", responseID)
		_, _ = fmt.Fprintf(response, "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"message\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"in_progress\",\"content\":[]}}\n\n")
		_, _ = fmt.Fprintf(response, "event: response.content_part.added\ndata: {\"type\":\"response.content_part.added\",\"output_index\":0,\"content_index\":0,\"item_id\":\"message\",\"part\":{\"type\":\"output_text\",\"text\":\"\"}}\n\n")
		_, _ = fmt.Fprintf(response, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"item_id\":\"message\",\"delta\":\"answer\"}\n\n")
		_, _ = fmt.Fprintf(response, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"message\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"answer\",\"annotations\":[]}]}}\n\n")
		_, _ = fmt.Fprintf(response, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":%q,\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n", responseID)
	}))
	defer server.Close()

	continuation := &manifest.ContinuationPolicy{
		Mode:                 "previous-response",
		ResponseIDPointer:    "/id",
		RequestField:         "previous_response_id",
		RequiredStableFields: []string{"model", "instructions", "tools"},
		AppendOnlyHistory:    true,
		Store:                "required",
		Fallback:             "full-replay",
	}
	registration := providerregistry.Registration{
		ProviderID:   providerID,
		Construction: providerregistry.ConstructionOpenAIResponses,
		Manifest:     &manifest.Manifest{ID: "synthetic.responses.plugin", Version: "1.0.0"},
		Operation: &providertransport.Operation{
			ID: "inference",
			Key: providertransport.Key{
				Protocol:  string(providerregistry.ConstructionOpenAIResponses),
				Transport: "sse",
			},
			Endpoint:     manifest.Endpoint{BaseURL: server.URL},
			Method:       http.MethodPost,
			Path:         "/v1/responses",
			Retry:        manifest.RetryPolicy{MaxAttempts: 1, Authentication: "never", ReplayRequirement: "before-first-event"},
			Continuation: continuation,
		},
	}
	provider := config.ProviderConfig{
		ID:      providerID,
		APIKey:  "credential-one",
		BaseURL: server.URL,
		Owner: &config.ProviderOwnerReference{
			Type:         config.ProviderOwnerPlugin,
			Construction: providerregistry.ConstructionOpenAIResponses,
		},
		Plugin: &config.ProviderPluginReference{ID: registration.Manifest.ID, Version: registration.Manifest.Version},
		Models: []catalog.Model{{ID: modelID, DefaultMaxTokens: 1024}},
	}
	cfg := &config.Config{
		Options:   &config.Options{},
		Providers: csync.NewMapFrom(map[string]config.ProviderConfig{providerID: provider}),
		Models: map[config.SelectedModelType]config.SelectedModel{
			config.SelectedModelTypeLarge: {Provider: providerID, Model: modelID},
			config.SelectedModelTypeSmall: {Provider: providerID, Model: modelID},
		},
	}
	store := config.NewTestStoreWithRegistrations(cfg, registration)
	coord := &coordinator{cfg: store, responsesContinuations: openairesponsestransport.NewContinuationStore()}
	build := func() Model {
		large, _, err := coord.buildAgentModelsWithSnapshot(t.Context(), config.Agent{Model: config.SelectedModelTypeLarge}, false, store.RuntimeSnapshot())
		require.NoError(t, err)
		return large
	}
	consume := func(model Model, call fantasy.Call) {
		stream, err := model.Model.Stream(t.Context(), call)
		require.NoError(t, err)
		for part := range stream {
			require.NotEqual(t, fantasy.StreamPartTypeError, part.Type)
		}
	}

	const sessionID = "session"
	first := fantasy.Call{
		Prompt:  fantasy.Prompt{fantasy.NewUserMessage("one")},
		Headers: map[string]string{"x-session-id": session.HashID(sessionID), "x-request-purpose": "conversation"},
	}
	consume(build(), first)
	followup := first
	followup.Prompt = append(append(fantasy.Prompt{}, first.Prompt...),
		fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "answer"}}},
		fantasy.Message{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "two"}}},
	)
	consume(build(), followup)

	coord.ResetSession(sessionID)
	consume(build(), followup)

	provider.APIKey = "credential-two"
	cfg.Providers.Set(providerID, provider)
	consume(build(), followup)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, requests, 4)
	require.NotContains(t, requests[0], "previous_response_id")
	require.Equal(t, "resp_1", requests[1]["previous_response_id"])
	require.NotContains(t, requests[2], "previous_response_id")
	require.NotContains(t, requests[3], "previous_response_id")
}

func TestNativeResponsesContinuationOwnerIsolatesEndpointOwnerAndAccount(t *testing.T) {
	t.Setenv("AI_CLI_DIR", t.TempDir())
	registration := providerregistry.Registration{
		ProviderID:       "provider",
		AccountNamespace: "synthetic-responses-account",
		Construction:     providerregistry.ConstructionOpenAIResponses,
		Manifest:         &manifest.Manifest{ID: "plugin.one", Version: "1.0.0"},
	}
	base := nativeResponsesContinuationOwner(config.RuntimeSnapshot{}, registration, registration.ProviderID, "https://one.example.test", "credential-one")
	require.NotEmpty(t, base)
	require.NotEqual(t, base, nativeResponsesContinuationOwner(config.RuntimeSnapshot{}, registration, registration.ProviderID, "https://two.example.test", "credential-one"))
	require.NotEqual(t, base, nativeResponsesContinuationOwner(config.RuntimeSnapshot{}, registration, registration.ProviderID, "https://one.example.test", "credential-two"))
	registration.Manifest = &manifest.Manifest{ID: "plugin.two", Version: "2.0.0"}
	require.NotEqual(t, base, nativeResponsesContinuationOwner(config.RuntimeSnapshot{}, registration, registration.ProviderID, "https://one.example.test", "credential-one"))

	registration.Manifest = &manifest.Manifest{ID: "plugin.one", Version: "1.0.0"}
	require.NoError(t, accounts.Save(t.Context(), registration.AccountNamespace, accounts.Entry{ID: "stable-account", AccessToken: "credential-one"}))
	stableBeforeRotation := nativeResponsesContinuationOwner(config.RuntimeSnapshot{}, registration, registration.ProviderID, "https://one.example.test", "credential-one")
	require.NoError(t, accounts.Save(t.Context(), registration.AccountNamespace, accounts.Entry{ID: "stable-account", AccessToken: "credential-two"}))
	stableAfterRotation := nativeResponsesContinuationOwner(config.RuntimeSnapshot{}, registration, registration.ProviderID, "https://one.example.test", "credential-two")
	require.Equal(t, stableBeforeRotation, stableAfterRotation)
	require.NotEqual(t, stableAfterRotation, nativeResponsesContinuationOwner(config.RuntimeSnapshot{}, registration, registration.ProviderID, "https://one.example.test", "credential-one"))
}

func TestBuiltProviderRejectsSameIDOwnerReplacementBeforeInferenceDispatch(t *testing.T) {
	const providerID = "inference-owner-test"
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"text":"unexpected"}`))
	}))
	defer server.Close()

	registration := func(pluginID, version string) providerregistry.Registration {
		registration, err := providerregistry.FromManifest(manifest.Manifest{
			ID:      pluginID,
			Version: version,
			Provider: manifest.Provider{
				ID:   providerID,
				Name: "Inference Owner Test",
			},
			Capabilities: manifest.Capabilities{
				Endpoints: []manifest.Endpoint{{ID: "api", BaseURL: server.URL}},
				Operations: []manifest.Operation{{
					ID:        "inference",
					Kind:      "inference",
					Protocol:  string(providerregistry.ConstructionGenericJSON),
					Transport: "http-json",
					Endpoint:  "api",
					Method:    http.MethodPost,
					Path:      "/generate",
					Retry:     &manifest.RetryPolicy{MaxAttempts: 1, Authentication: "never", ReplayRequirement: "never"},
				}},
			},
		})
		require.NoError(t, err)
		return registration
	}
	providerConfig := func(owner providerregistry.Registration) config.ProviderConfig {
		return config.ProviderConfig{
			ID:      providerID,
			BaseURL: server.URL,
			Owner: &config.ProviderOwnerReference{
				Type:         config.ProviderOwnerPlugin,
				Construction: owner.Construction,
			},
			Plugin: &config.ProviderPluginReference{ID: owner.Manifest.ID, Version: owner.Manifest.Version},
		}
	}

	ownerA := registration("plugin.owner-a", "1.0.0")
	providerA := providerConfig(ownerA)
	storeA := config.NewTestStoreWithRegistrations(&config.Config{
		Options:   &config.Options{},
		Providers: csync.NewMapFrom(map[string]config.ProviderConfig{providerID: providerA}),
	}, ownerA)
	coord := &coordinator{cfg: storeA}
	built, err := coord.buildProvider(storeA.RuntimeSnapshot(), providerA, config.SelectedModel{Provider: providerID, Model: "model-one"}, false)
	require.NoError(t, err)
	model, err := built.LanguageModel(t.Context(), "model-one")
	require.NoError(t, err)

	ownerB := registration("plugin.owner-b", "2.0.0")
	providerB := providerConfig(ownerB)
	coord.cfg = config.NewTestStoreWithRegistrations(&config.Config{
		Options:   &config.Options{},
		Providers: csync.NewMapFrom(map[string]config.ProviderConfig{providerID: providerB}),
	}, ownerB)

	response, err := model.Generate(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hello")}})
	require.Nil(t, response)
	require.ErrorContains(t, err, "active owner for provider inference-owner-test changed")
	require.Zero(t, requests.Load())
}

func TestBuildProviderRejectsPassedPluginOwnerMismatch(t *testing.T) {
	base := initTestConfig(t, t.TempDir())
	registration := providerregistry.Registration{
		ProviderID:           "construction-owner-test",
		Construction:         providerregistry.ConstructionGenericJSON,
		CompatibilityAdapter: providerregistry.ConstructionOpenAIResponses,
		Manifest:             &manifest.Manifest{ID: "synthetic.plugin", Version: "1.0.0"},
	}
	persisted := config.ProviderConfig{
		ID:      registration.ProviderID,
		Type:    catalog.TypeOpenAICompat,
		BaseURL: "https://api.example.test/v1",
		Owner: &config.ProviderOwnerReference{
			Type:                 config.ProviderOwnerPlugin,
			Construction:         registration.Construction,
			CompatibilityAdapter: registration.CompatibilityAdapter,
		},
		Plugin: &config.ProviderPluginReference{ID: registration.Manifest.ID, Version: registration.Manifest.Version},
	}
	base.Config().Providers.Set(registration.ProviderID, persisted)
	store := config.NewTestStoreWithRegistrations(base.Config(), registration)
	coord := &coordinator{cfg: store}

	for _, test := range []struct {
		name     string
		provider config.ProviderConfig
		error    string
	}{
		{name: "missing owner", provider: func() config.ProviderConfig {
			provider := persisted
			provider.Owner = nil
			return provider
		}(), error: "passed owner does not match its persisted owner"},
		{name: "wrong owner type", provider: func() config.ProviderConfig {
			provider := persisted
			provider.Owner = &config.ProviderOwnerReference{Type: config.ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat}
			return provider
		}(), error: "passed owner does not match its persisted owner"},
		{name: "wrong construction", provider: func() config.ProviderConfig {
			provider := persisted
			provider.Owner = &config.ProviderOwnerReference{Type: config.ProviderOwnerPlugin, Construction: providerregistry.ConstructionAnthropicMessages, CompatibilityAdapter: registration.CompatibilityAdapter}
			return provider
		}(), error: "passed owner does not match its persisted owner"},
		{name: "wrong compatibility adapter", provider: func() config.ProviderConfig {
			provider := persisted
			provider.Owner = &config.ProviderOwnerReference{Type: config.ProviderOwnerPlugin, Construction: registration.Construction, CompatibilityAdapter: providerregistry.ConstructionGeminiContent}
			return provider
		}(), error: "passed owner does not match its persisted owner"},
		{name: "wrong plugin ID", provider: func() config.ProviderConfig {
			provider := persisted
			provider.Plugin = &config.ProviderPluginReference{ID: "wrong.plugin", Version: registration.Manifest.Version}
			return provider
		}(), error: "passed owner does not match its persisted owner"},
		{name: "stale plugin version", provider: func() config.ProviderConfig {
			provider := persisted
			provider.Plugin = &config.ProviderPluginReference{ID: registration.Manifest.ID, Version: registration.Manifest.Version + ".stale"}
			return provider
		}(), error: "passed owner does not match its persisted owner"},
		{name: "conflicting provider ID", provider: func() config.ProviderConfig {
			provider := persisted
			provider.ID = "other"
			return provider
		}(), error: "conflicts with passed provider ID"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := coord.buildProvider(store.RuntimeSnapshot(), test.provider, config.SelectedModel{Provider: registration.ProviderID}, false)
			require.ErrorContains(t, err, test.error)
		})
	}
}

func TestBuildProviderRejectsMissingPresetEvenWhenProviderIDIsRegistered(t *testing.T) {
	base := initTestConfig(t, t.TempDir())
	providerID := "synthetic-preset"
	providerCfg := config.ProviderConfig{
		ID:      providerID,
		Name:    "Retained Preset",
		Type:    catalog.TypeOpenAICompat,
		APIKey:  "key",
		BaseURL: "https://api.example.test/v1",
		Owner:   &config.ProviderOwnerReference{Type: config.ProviderOwnerPreset, Construction: providerregistry.ConstructionOpenAICompat},
		Preset:  &config.ProviderPresetReference{ID: "missing.preset", Version: "1.0.0", Digest: "missing-digest"},
		Models:  []catalog.Model{{ID: "retained-model"}},
	}
	base.Config().Providers.Set(providerID, providerCfg)
	registration := providerregistry.Registration{
		ProviderID:   providerID,
		Construction: providerregistry.ConstructionGenericJSON,
		Manifest:     &manifest.Manifest{ID: "same-id.plugin", Version: "1.0.0"},
	}
	store := config.NewTestStoreWithRegistrations(base.Config(), registration)
	coord := &coordinator{cfg: store}

	passed := providerCfg
	passed.Preset = &config.ProviderPresetReference{ID: providerCfg.Preset.ID, Version: providerCfg.Preset.Version, Digest: "other-digest"}
	_, err := coord.buildProvider(store.RuntimeSnapshot(), passed, config.SelectedModel{Provider: providerID, Model: "retained-model"}, false)
	require.ErrorContains(t, err, "passed owner does not match its persisted owner")

	_, err = coord.buildProvider(store.RuntimeSnapshot(), providerCfg, config.SelectedModel{Provider: providerID, Model: "retained-model"}, false)
	require.ErrorContains(t, err, "preset missing.preset version 1.0.0 with its persisted digest is not its active exact owner")
}

func TestBuildProviderRejectsMarkerlessReservedOwners(t *testing.T) {
	for _, providerID := range []string{
		string(catalog.ProviderCopilot),
		codex.ID,
		gemini.ID,
		string(catalog.ProviderDeepSeek),
	} {
		t.Run(providerID, func(t *testing.T) {
			base := initTestConfig(t, t.TempDir())
			providerCfg := config.ProviderConfig{
				ID:      providerID,
				Type:    catalog.TypeOpenAICompat,
				BaseURL: "https://api.example.test/v1",
			}
			base.Config().Providers.Set(providerID, providerCfg)
			store := config.NewTestStore(base.Config())
			coord := &coordinator{cfg: store}

			_, err := coord.buildProvider(store.RuntimeSnapshot(), providerCfg, config.SelectedModel{Provider: providerID}, false)
			require.ErrorContains(t, err, "incomplete owner reference")
		})
	}
}

func TestBuildProviderRejectsUnavailableExactPluginOwner(t *testing.T) {
	base := initTestConfig(t, t.TempDir())
	providerID := "missing-plugin-provider"
	providerCfg := config.ProviderConfig{
		ID:      providerID,
		Type:    catalog.TypeOpenAICompat,
		BaseURL: "https://api.example.test/v1",
		Owner:   &config.ProviderOwnerReference{Type: config.ProviderOwnerPlugin, Construction: providerregistry.ConstructionGenericJSON},
		Plugin:  &config.ProviderPluginReference{ID: "missing.plugin", Version: "1.0.0"},
	}
	base.Config().Providers.Set(providerID, providerCfg)
	store := config.NewTestStoreWithRegistrations(base.Config())
	coord := &coordinator{cfg: store}

	_, err := coord.buildProvider(store.RuntimeSnapshot(), providerCfg, config.SelectedModel{Provider: providerID}, false)
	require.ErrorContains(t, err, "plugin missing.plugin version 1.0.0 is not its active exact owner")
}

func TestBuildProviderAllowsExactCustomOwnerAgainstSameIDPlugin(t *testing.T) {
	base := initTestConfig(t, t.TempDir())
	providerID := "custom-provider"
	providerCfg := config.ProviderConfig{
		ID:      providerID,
		Type:    catalog.TypeOpenAICompat,
		BaseURL: "https://api.example.test/v1",
		Owner:   &config.ProviderOwnerReference{Type: config.ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat},
	}
	base.Config().Providers.Set(providerID, providerCfg)
	registration := providerregistry.Registration{
		ProviderID:   providerID,
		Construction: providerregistry.ConstructionGenericJSON,
		Manifest:     &manifest.Manifest{ID: "same-id.plugin", Version: "1.0.0"},
	}
	store := config.NewTestStoreWithRegistrations(base.Config(), registration)
	coord := &coordinator{cfg: store}

	provider, err := coord.buildProvider(store.RuntimeSnapshot(), providerCfg, config.SelectedModel{Provider: providerID}, false)
	require.NoError(t, err)
	require.NotNil(t, provider)
}

// newMockAgent creates a mockSessionAgent with the given provider and run function.
func newMockAgent(providerID string, maxTokens int64, snapshot config.RuntimeSnapshot, runFunc func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error)) *mockSessionAgent {
	return &mockSessionAgent{
		model: Model{
			CatalogModel: catalog.Model{
				DefaultMaxTokens: maxTokens,
			},
			ModelCfg: config.SelectedModel{
				Provider: providerID,
			},
		},
		runtime: InstalledRuntime{Snapshot: snapshot},
		runFunc: runFunc,
	}
}

// agentResultWithText creates a minimal AgentResult with the given text response.
func agentResultWithText(text string) *fantasy.AgentResult {
	return &fantasy.AgentResult{
		Response: fantasy.Response{
			Content: fantasy.ResponseContent{
				fantasy.TextContent{Text: text},
			},
		},
	}
}

func TestCoordinatorSummarizeUsesCapturedRuntime(t *testing.T) {
	const providerID = "test-provider"
	environment := testEnv(t)
	coord := newTestCoordinator(t, environment, providerID, config.ProviderConfig{ID: providerID})
	cfg := coord.cfg.Config()
	cfg.Providers.Set(providerID, config.ProviderConfig{ID: providerID})
	coord.cfg = config.NewTestStore(cfg)
	snapshot := coord.cfg.RuntimeSnapshot()
	captured := InstalledRuntime{
		LargeModel: Model{ModelCfg: config.SelectedModel{Provider: providerID, Model: "captured-model"}},
		Snapshot:   snapshot,
	}
	called := false
	agent := &mockSessionAgent{
		model:   captured.LargeModel,
		runtime: captured,
		summarizeRuntimeFunc: func(runtime InstalledRuntime) error {
			called = true
			require.Equal(t, captured.LargeModel.ModelCfg, runtime.LargeModel.ModelCfg)
			require.Same(t, captured.Snapshot.Config(), runtime.Snapshot.Config())
			return nil
		},
	}
	coord.currentAgent = agent

	require.NoError(t, coord.Summarize(t.Context(), "session"))
	require.True(t, called)
}

func TestScheduleCodebaseIndexReconcileIsAsyncAndCoalesced(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	coord := &coordinator{
		reconcileCodebaseIndexFn: func(context.Context) (codebaseindex.StoreStatus, error) {
			started <- struct{}{}
			<-release
			return codebaseindex.StoreStatus{}, nil
		},
	}

	returned := make(chan struct{})
	go func() {
		coord.scheduleCodebaseIndexReconcile()
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("scheduler waited for index reconciliation")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("index reconciliation did not start")
	}

	coord.scheduleCodebaseIndexReconcile()
	select {
	case <-started:
		t.Fatal("concurrent index reconciliation was not coalesced")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	require.Eventually(t, func() bool {
		coord.codebaseIndexReconcileMu.Lock()
		defer coord.codebaseIndexReconcileMu.Unlock()
		return !coord.codebaseIndexReconciling
	}, time.Second, 10*time.Millisecond)

	coord.scheduleCodebaseIndexReconcile()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("index reconciliation could not be scheduled after completion")
	}
}

func TestCodebaseIndexLifecycleReconcilesPeriodicallyAndStops(t *testing.T) {
	started := make(chan struct{}, 8)
	coord := &coordinator{
		reconcileCodebaseIndexFn: func(context.Context) (codebaseindex.StoreStatus, error) {
			started <- struct{}{}
			return codebaseindex.StoreStatus{}, nil
		},
	}
	ctx, cancel := context.WithCancel(t.Context())
	coord.startCodebaseIndexLifecycle(ctx, 10*time.Millisecond)
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("periodic index reconciliation did not run")
		}
	}
	cancel()
	coord.stopCodebaseIndexLifecycle(t.Context())
	for {
		select {
		case <-started:
			continue
		default:
			goto drained
		}
	}

drained:
	select {
	case <-started:
		t.Fatal("index reconciliation continued after lifecycle stop")
	case <-time.After(30 * time.Millisecond):
	}
}

func TestCodebaseIndexSearchReconciliationIsThrottled(t *testing.T) {
	started := make(chan struct{}, 4)
	coord := &coordinator{
		reconcileCodebaseIndexFn: func(context.Context) (codebaseindex.StoreStatus, error) {
			started <- struct{}{}
			return codebaseindex.StoreStatus{}, nil
		},
	}
	coord.startCodebaseIndexLifecycle(t.Context(), time.Hour)
	t.Cleanup(func() { coord.stopCodebaseIndexLifecycle(t.Context()) })
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("startup index reconciliation did not run")
	}
	require.Eventually(t, func() bool {
		coord.codebaseIndexReconcileMu.Lock()
		defer coord.codebaseIndexReconcileMu.Unlock()
		return !coord.codebaseIndexReconciling
	}, time.Second, 10*time.Millisecond)

	coord.requestCodebaseIndexReconcileAfter(0)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("search-triggered index reconciliation did not run")
	}
	require.Eventually(t, func() bool {
		coord.codebaseIndexReconcileMu.Lock()
		defer coord.codebaseIndexReconcileMu.Unlock()
		return !coord.codebaseIndexReconciling
	}, time.Second, 10*time.Millisecond)
	coord.requestCodebaseIndexReconcileAfter(time.Hour)
	select {
	case <-started:
		t.Fatal("search-triggered index reconciliation ignored throttle")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestCoordinatorLoadsBoundedMemoryInputsFromPersistedState(t *testing.T) {
	env := testEnv(t)
	parent, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	_, err = env.sessions.CreateTaskSession(t.Context(), "tool-call", parent.ID, "Child")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), parent.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "remember focused tests"}},
	})
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), parent.ID, message.CreateMessageParams{
		Role:  message.Tool,
		Parts: []message.ContentPart{message.TextContent{Text: "tool noise"}},
	})
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), parent.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "acknowledged"}},
	})
	require.NoError(t, err)
	coord := &coordinator{sessions: env.sessions, messages: env.messages}

	turns, err := coord.loadMemoryTranscript(t.Context(), parent.ID)
	require.NoError(t, err)
	require.Equal(t, []automemory.Turn{
		{Role: "user", Text: "remember focused tests"},
		{Role: "assistant", Text: "acknowledged"},
	}, turns)
	sessions, err := coord.loadMemorySessions(t.Context())
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.Equal(t, parent.ID, sessions[0].ID)
}

func TestRunSubAgent(t *testing.T) {
	const providerID = "test-provider"
	providerCfg := config.ProviderConfig{ID: providerID}

	t.Run("missing agent fails before session creation", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		_, err := coord.runSubAgent(t.Context(), subAgentParams{
			SessionID:      "parent",
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.EqualError(t, err, "sub-agent is unavailable")
		sessions, listErr := env.sessions.List(t.Context())
		require.NoError(t, listErr)
		require.Empty(t, sessions)
	})

	t.Run("missing runtime snapshot fails before session creation", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)
		runCalled := false
		agent := newMockAgent(providerID, 4096, config.RuntimeSnapshot{}, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			runCalled = true
			return agentResultWithText("unexpected"), nil
		})

		_, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      "parent",
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.EqualError(t, err, "sub-agent runtime snapshot is unavailable")
		require.False(t, runCalled)
		sessions, listErr := env.sessions.List(t.Context())
		require.NoError(t, listErr)
		require.Empty(t, sessions)
	})

	t.Run("happy path", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, coord.cfg.RuntimeSnapshot(), func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			assert.Equal(t, "do something", call.Prompt)
			assert.Equal(t, "<delegated_task>\ndo something\n</delegated_task>", call.TurnInstructions)
			assert.Equal(t, int64(4096), call.MaxOutputTokens)
			require.NotNil(t, call.runtime)
			assert.Equal(t, providerID, call.runtime.LargeModel.ModelCfg.Provider)
			return agentResultWithText("done"), nil
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "do something",
			SessionTitle:   "Test Session",
		})
		require.NoError(t, err)
		assert.Equal(t, "done", resp.Content)
		assert.False(t, resp.IsError)
	})

	t.Run("cost update failure preserves output", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		agent := newMockAgent(providerID, 4096, coord.cfg.RuntimeSnapshot(), func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			return agentResultWithText("output before cost failure"), nil
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      "missing-parent-session",
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.NoError(t, err)
		assert.False(t, resp.IsError)
		assert.Equal(t, "output before cost failure", resp.Content)
	})

	t.Run("response with text returns it", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, coord.cfg.RuntimeSnapshot(), func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			return agentResultWithText("the answer"), nil
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.NoError(t, err)
		assert.False(t, resp.IsError)
		assert.Equal(t, "the answer", resp.Content)
	})

	t.Run("nil result returns error response", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, coord.cfg.RuntimeSnapshot(), func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			return nil, nil
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.NoError(t, err)
		assert.True(t, resp.IsError)
		assert.Equal(t, "Sub-agent completed but produced no text output.", resp.Content)
	})

	t.Run("empty result returns error response", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, coord.cfg.RuntimeSnapshot(), func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			return &fantasy.AgentResult{}, nil
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.NoError(t, err)
		assert.True(t, resp.IsError)
		assert.Equal(t, "Sub-agent completed but produced no text output.", resp.Content)
	})

	t.Run("ModelCfg.MaxTokens overrides default", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := &mockSessionAgent{
			model: Model{
				CatalogModel: catalog.Model{
					DefaultMaxTokens: 4096,
				},
				ModelCfg: config.SelectedModel{
					Provider:  providerID,
					MaxTokens: 8192,
				},
			},
			runtime: InstalledRuntime{Snapshot: coord.cfg.RuntimeSnapshot()},
			runFunc: func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
				assert.Equal(t, int64(8192), call.MaxOutputTokens)
				return agentResultWithText("ok"), nil
			},
		}

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.NoError(t, err)
		assert.Equal(t, "ok", resp.Content)
	})

	t.Run("session creation failure with canceled context", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, coord.cfg.RuntimeSnapshot(), nil)

		// Use a canceled context to trigger CreateTaskSession failure.
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err = coord.runSubAgent(ctx, subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.Error(t, err)
	})

	t.Run("provider not configured", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		// Agent references a provider that doesn't exist in config.
		agent := newMockAgent("unknown-provider", 4096, coord.cfg.RuntimeSnapshot(), nil)

		_, err = coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "model provider not configured")
	})

	t.Run("agent run error returns error response", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, coord.cfg.RuntimeSnapshot(), func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			return nil, errors.New("provider request failed")
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		// runSubAgent returns (errorResponse, nil) when agent.Run fails — not a Go error.
		require.NoError(t, err)
		assert.True(t, resp.IsError)
		assert.Equal(t, "Failed to generate response: provider request failed", resp.Content)
	})

	t.Run("session setup callback is invoked", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		var setupCalledWith string
		agent := newMockAgent(providerID, 4096, coord.cfg.RuntimeSnapshot(), func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			return agentResultWithText("ok"), nil
		})

		_, err = coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
			SessionSetup: func(sessionID string) {
				setupCalledWith = sessionID
			},
		})
		require.NoError(t, err)
		assert.NotEmpty(t, setupCalledWith, "SessionSetup should have been called")
	})

	t.Run("cost propagation to parent session", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, coord.cfg.RuntimeSnapshot(), func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			// Simulate the agent incurring cost by updating the child session.
			childSession, err := env.sessions.Get(ctx, call.SessionID)
			if err != nil {
				return nil, err
			}
			childSession.Cost = 0.05
			_, err = env.sessions.Save(ctx, childSession)
			if err != nil {
				return nil, err
			}
			return agentResultWithText("ok"), nil
		})

		_, err = coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.NoError(t, err)

		updated, err := env.sessions.Get(t.Context(), parentSession.ID)
		require.NoError(t, err)
		assert.InDelta(t, 0.05, updated.Cost, 1e-9)
	})
}

func TestUpdateParentSessionCost(t *testing.T) {
	t.Run("accumulates cost correctly", func(t *testing.T) {
		env := testEnv(t)
		cfg := initTestConfig(t, env.workingDir)
		coord := &coordinator{cfg: cfg, sessions: env.sessions}

		parent, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		child, err := env.sessions.CreateTaskSession(t.Context(), "tool-1", parent.ID, "Child")
		require.NoError(t, err)

		// Set child cost.
		child.Cost = 0.10
		_, err = env.sessions.Save(t.Context(), child)
		require.NoError(t, err)

		err = coord.updateParentSessionCost(t.Context(), child.ID, parent.ID, 0)
		require.NoError(t, err)

		updated, err := env.sessions.Get(t.Context(), parent.ID)
		require.NoError(t, err)
		assert.InDelta(t, 0.10, updated.Cost, 1e-9)
	})

	t.Run("accumulates multiple child costs", func(t *testing.T) {
		env := testEnv(t)
		cfg := initTestConfig(t, env.workingDir)
		coord := &coordinator{cfg: cfg, sessions: env.sessions}

		parent, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		child1, err := env.sessions.CreateTaskSession(t.Context(), "tool-1", parent.ID, "Child1")
		require.NoError(t, err)
		child1.Cost = 0.05
		_, err = env.sessions.Save(t.Context(), child1)
		require.NoError(t, err)

		child2, err := env.sessions.CreateTaskSession(t.Context(), "tool-2", parent.ID, "Child2")
		require.NoError(t, err)
		child2.Cost = 0.03
		_, err = env.sessions.Save(t.Context(), child2)
		require.NoError(t, err)

		err = coord.updateParentSessionCost(t.Context(), child1.ID, parent.ID, 0)
		require.NoError(t, err)
		err = coord.updateParentSessionCost(t.Context(), child2.ID, parent.ID, 0)
		require.NoError(t, err)

		updated, err := env.sessions.Get(t.Context(), parent.ID)
		require.NoError(t, err)
		assert.InDelta(t, 0.08, updated.Cost, 1e-9)
	})

	t.Run("child session not found", func(t *testing.T) {
		env := testEnv(t)
		cfg := initTestConfig(t, env.workingDir)
		coord := &coordinator{cfg: cfg, sessions: env.sessions}

		parent, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		err = coord.updateParentSessionCost(t.Context(), "non-existent", parent.ID, 0)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "get child session")
	})

	t.Run("parent session not found", func(t *testing.T) {
		env := testEnv(t)
		cfg := initTestConfig(t, env.workingDir)
		coord := &coordinator{cfg: cfg, sessions: env.sessions}

		parent, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)
		child, err := env.sessions.CreateTaskSession(t.Context(), "tool-1", parent.ID, "Child")
		require.NoError(t, err)

		err = coord.updateParentSessionCost(t.Context(), child.ID, "non-existent", 0)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "get parent session")
	})

	t.Run("adds only cost incurred by a continued child turn", func(t *testing.T) {
		env := testEnv(t)
		cfg := initTestConfig(t, env.workingDir)
		coord := &coordinator{cfg: cfg, sessions: env.sessions}

		parent, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)
		child, err := env.sessions.CreateTaskSession(t.Context(), "tool-1", parent.ID, "Child")
		require.NoError(t, err)
		child.Cost = 0.16
		_, err = env.sessions.Save(t.Context(), child)
		require.NoError(t, err)

		err = coord.updateParentSessionCost(t.Context(), child.ID, parent.ID, 0.1)
		require.NoError(t, err)

		updated, err := env.sessions.Get(t.Context(), parent.ID)
		require.NoError(t, err)
		assert.InDelta(t, 0.06, updated.Cost, 1e-9)
	})

	t.Run("zero cost handled correctly", func(t *testing.T) {
		env := testEnv(t)
		cfg := initTestConfig(t, env.workingDir)
		coord := &coordinator{cfg: cfg, sessions: env.sessions}

		parent, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)
		child, err := env.sessions.CreateTaskSession(t.Context(), "tool-1", parent.ID, "Child")
		require.NoError(t, err)

		err = coord.updateParentSessionCost(t.Context(), child.ID, parent.ID, 0)
		require.NoError(t, err)

		updated, err := env.sessions.Get(t.Context(), parent.ID)
		require.NoError(t, err)
		assert.InDelta(t, 0.0, updated.Cost, 1e-9)
	})
}

func TestApplyCodexRuntimeOptions(t *testing.T) {
	configured := &config.Options{ResponseVerbosity: "high", AnalysisEffort: "max"}

	registration, registered := integratedRegistration(t, codex.ID)
	for _, modelID := range []string{"gpt-5.6-sol", "gpt-6-astra"} {
		t.Run(modelID, func(t *testing.T) {
			options := applyRegisteredRuntimeOptions(modelID, configured, registration, registered, fantasy.ProviderOptions{})
			parsed, ok := options[codexresponses.Name].(*codexresponses.ProviderOptions)
			require.True(t, ok)
			assert.Equal(t, "high", parsed.ResponseVerbosity)
			assert.Equal(t, "max", parsed.ReasoningEffort)
			assert.False(t, parsed.DisableReasoning)
		})
	}
}

func TestApplyCodexRuntimeOptionsIsRestrictedToCodexGPT56(t *testing.T) {
	configured := &config.Options{ResponseVerbosity: "high", AnalysisEffort: "max"}

	codexRegistration, codexRegistered := integratedRegistration(t, codex.ID)
	for _, tc := range []struct {
		name         string
		modelID      string
		registration providerregistry.Registration
		registered   bool
	}{
		{name: "unregistered provider", modelID: "gpt-5.6-sol"},
		{name: "other model", modelID: "gpt-5.5-codex", registration: codexRegistration, registered: codexRegistered},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options := applyRegisteredRuntimeOptions(tc.modelID, configured, tc.registration, tc.registered, fantasy.ProviderOptions{})
			assert.Empty(t, options)
		})
	}
}

func TestPresetOwnerDoesNotInheritSameIDAgentBehavior(t *testing.T) {
	for _, test := range []struct {
		name   string
		preset *config.ProviderPresetReference
		plugin *config.ProviderPluginReference
	}{
		{name: "preset", preset: &config.ProviderPresetReference{ID: "example.codex-preset", Version: "1"}},
		{name: "plugin", plugin: &config.ProviderPluginReference{ID: "example.codex-plugin", Version: "1"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			providerCfg := config.ProviderConfig{
				ID:     codex.ID,
				Type:   openaicompat.Name,
				Preset: test.preset,
				Plugin: test.plugin,
			}
			configured := &config.Options{ResponseVerbosity: "high", AnalysisEffort: "max"}
			registration, registered := providerregistry.Registration{}, false
			runtimeOptions := applyRegisteredRuntimeOptions("gpt-5.6-sol", configured, registration, registered, fantasy.ProviderOptions{})
			require.Empty(t, runtimeOptions)

			require.Nil(t, registration.Reasoning)
			require.Equal(t, fantasy.InstructionPolicyGeneric, instructionPolicyForConstruction(registration.Construction))

			cfg := initTestConfig(t, t.TempDir())
			cfg.Config().Providers.Set(codex.ID, providerCfg)
			imagegen := &skills.Skill{Name: "imagegen"}
			snapshot := skills.Snapshot{AllSkills: []*skills.Skill{imagegen}, ActiveSkills: []*skills.Skill{imagegen}}
			require.Equal(t, snapshot, providerSkillSnapshot(snapshot, cfg.Config(), codex.ID))

			model := Model{
				CatalogModel: catalog.Model{ID: "gpt-5.6-sol", CanReason: true, ReasoningLevels: []string{"high"}},
				ModelCfg:     config.SelectedModel{Provider: codex.ID, ReasoningEffort: "high"},
			}
			providerOptions, optionsErr := getProviderOptions(model, providerCfg, registration)
			require.NoError(t, optionsErr)
			require.NotContains(t, providerOptions, codexresponses.Name)
			require.Contains(t, providerOptions, openaicompat.Name)
		})
	}
}

func TestSameIDBehaviorConsumersUseOnlyExactOwner(t *testing.T) {
	const providerID = "same-id-provider"
	registration := func(pluginID, hiddenSkill string, status int, title string) providerregistry.Registration {
		errorMappings := []manifest.ErrorMapping{{
			Class: "authentication", Statuses: []int{status}, Title: title,
		}}
		return providerregistry.Registration{
			ProviderID:   providerID,
			Construction: providerregistry.ConstructionGenericJSON,
			Manifest: &manifest.Manifest{
				ID: pluginID, Version: "1.0.0",
				Capabilities: manifest.Capabilities{Errors: errorMappings},
			},
			Instructions: &providerregistry.InstructionCapability{HiddenSkills: []string{hiddenSkill}},
			Errors:       errorMappings,
		}
	}
	configuration := func(persisted, active providerregistry.Registration) (*config.ConfigStore, config.ProviderConfig) {
		provider := config.ProviderConfig{
			ID: providerID,
			Owner: &config.ProviderOwnerReference{
				Type:         config.ProviderOwnerPlugin,
				Construction: persisted.Construction,
			},
			Plugin: &config.ProviderPluginReference{ID: persisted.Manifest.ID, Version: persisted.Manifest.Version},
		}
		cfg := &config.Config{Providers: csync.NewMapFrom(map[string]config.ProviderConfig{providerID: provider})}
		return config.NewTestStoreWithRegistrations(cfg, active), provider
	}

	ownerA := registration("plugin.owner-a", "owner-a-hidden", http.StatusForbidden, "Owner A error")
	ownerB := registration("plugin.owner-b", "owner-b-hidden", http.StatusTooManyRequests, "Owner B error")
	ownerASkill := &skills.Skill{Name: "owner-a-hidden"}
	ownerBSkill := &skills.Skill{Name: "owner-b-hidden"}
	catalogSnapshot := skills.Snapshot{
		AllSkills:    []*skills.Skill{ownerASkill, ownerBSkill},
		ActiveSkills: []*skills.Skill{ownerASkill, ownerBSkill},
	}

	mismatched, mismatchedProvider := configuration(ownerA, ownerB)
	require.Equal(t, catalogSnapshot, providerSkillSnapshot(catalogSnapshot, mismatched.Config(), providerID))
	_, ok := mismatched.RuntimeSnapshot().ProviderBehaviorRegistration(providerID, mismatchedProvider)
	require.False(t, ok)

	exact, exactProvider := configuration(ownerB, ownerB)
	filtered := providerSkillSnapshot(catalogSnapshot, exact.Config(), providerID)
	require.Equal(t, []*skills.Skill{ownerASkill}, filtered.ActiveSkills)
	behavior, ok := exact.RuntimeSnapshot().ProviderBehaviorRegistration(providerID, exactProvider)
	require.True(t, ok)
	require.Equal(t, ownerB.Owner(), behavior.Owner())

	ownerAError := &fantasy.ProviderError{StatusCode: http.StatusForbidden}
	ownerAModel := mapLanguageModelErrors(errorMappingModel{generateErr: ownerAError}, behavior)
	_, err := ownerAModel.Generate(t.Context(), fantasy.Call{})
	require.Error(t, err)
	require.False(t, ownerAError.AuthError)
	require.Empty(t, ownerAError.Title)

	ownerBError := &fantasy.ProviderError{StatusCode: http.StatusTooManyRequests}
	ownerBModel := mapLanguageModelErrors(errorMappingModel{generateErr: ownerBError}, behavior)
	_, err = ownerBModel.Generate(t.Context(), fantasy.Call{})
	require.Error(t, err)
	require.True(t, ownerBError.AuthError)
	require.Equal(t, "Owner B error", ownerBError.Title)
}

func TestGetProviderOptionsCarriesSelectedNativeResponsesControl(t *testing.T) {
	registration, err := providerregistry.FromManifest(manifest.Manifest{
		Provider: manifest.Provider{ID: "synthetic", Name: "Synthetic"},
		Capabilities: manifest.Capabilities{
			Endpoints: []manifest.Endpoint{{ID: "api", BaseURL: "https://example.invalid"}},
			Operations: []manifest.Operation{{
				ID: "inference", Kind: "inference", Protocol: string(providerregistry.ConstructionOpenAIResponses), Transport: "sse", Endpoint: "api", Path: "/v1/responses",
				Retry: &manifest.RetryPolicy{MaxAttempts: 1, Authentication: "never", ReplayRequirement: "never"},
			}},
			RuntimeControls: []manifest.RuntimeControl{{ID: "vendor.mode", Label: "Vendor mode", Type: "string", Scope: "model", RequestPath: "/vendor/mode"}},
		},
	})
	require.NoError(t, err)

	model := Model{
		CatalogModel: catalog.Model{ID: "gpt-test"},
		ModelCfg: config.SelectedModel{
			Provider:        "synthetic",
			ProviderOptions: map[string]any{"vendor.mode": "selected"},
		},
	}
	providerCfg := config.ProviderConfig{ID: "synthetic", ProviderOptions: map[string]any{"vendor.mode": "provider"}}
	options, optionsErr := getProviderOptions(model, providerCfg, registration)
	require.NoError(t, optionsErr)
	native, ok := options[openai.Name].(*openai.ResponsesProviderOptions)
	require.True(t, ok)
	require.Equal(t, "selected", native.RuntimeControls["/vendor/mode"])
}

func TestGetProviderOptionsRejectsInvalidNativeResponsesOptions(t *testing.T) {
	model := Model{
		CatalogModel: catalog.Model{ID: "gpt-test"},
		ModelCfg: config.SelectedModel{
			Provider:        "synthetic",
			ProviderOptions: map[string]any{"max_tool_calls": "invalid"},
		},
	}
	registration := providerregistry.Registration{
		ProviderID:   "synthetic",
		Construction: providerregistry.ConstructionOpenAIResponses,
	}

	options, err := getProviderOptions(model, config.ProviderConfig{ID: "synthetic"}, registration)
	require.Nil(t, options)
	require.ErrorContains(t, err, "parse OpenAI Responses provider options")
	require.ErrorContains(t, err, "max_tool_calls")
}

func TestGetProviderOptionsRejectsInvalidNativeResponsesOptionsWithManifestRuntimeControl(t *testing.T) {
	registration, err := providerregistry.FromManifest(manifest.Manifest{
		Provider: manifest.Provider{ID: "synthetic", Name: "Synthetic"},
		Capabilities: manifest.Capabilities{
			Endpoints: []manifest.Endpoint{{ID: "api", BaseURL: "https://example.invalid"}},
			Operations: []manifest.Operation{{
				ID: "inference", Kind: "inference", Protocol: string(providerregistry.ConstructionOpenAIResponses), Transport: "sse", Endpoint: "api", Path: "/v1/responses",
				Retry: &manifest.RetryPolicy{MaxAttempts: 1, Authentication: "never", ReplayRequirement: "never"},
			}},
			RuntimeControls: []manifest.RuntimeControl{{ID: "vendor.mode", Label: "Vendor mode", Type: "string", Scope: "model", RequestPath: "/vendor/mode"}},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, registration.Reasoning)

	model := Model{
		CatalogModel: catalog.Model{ID: "gpt-test"},
		ModelCfg: config.SelectedModel{
			Provider: "synthetic",
			ProviderOptions: map[string]any{
				"vendor.mode":    "selected",
				"max_tool_calls": "invalid",
			},
		},
	}
	options, optionsErr := getProviderOptions(model, config.ProviderConfig{ID: "synthetic"}, registration)
	require.Nil(t, options)
	require.ErrorContains(t, optionsErr, "parse OpenAI Responses provider options")
	require.ErrorContains(t, optionsErr, "max_tool_calls")
}

func TestGetProviderOptionsRejectsInvalidIntegratedGeminiOptions(t *testing.T) {
	registration, ok := integratedRegistration(t, gemini.ID)
	require.True(t, ok)

	model := Model{
		CatalogModel: catalog.Model{ID: "gemini-test"},
		ModelCfg: config.SelectedModel{
			Provider:        gemini.ID,
			ProviderOptions: map[string]any{"thinking_config": "invalid"},
		},
	}
	options, err := getProviderOptions(model, config.ProviderConfig{ID: gemini.ID}, registration)
	require.Nil(t, options)
	require.ErrorContains(t, err, "parse Gemini provider options")
	require.ErrorContains(t, err, "thinking_config")
}

func TestGetProviderOptionsReasoningEffort(t *testing.T) {
	model := Model{
		CatalogModel: catalog.Model{
			ID:              "deepseek-chat",
			CanReason:       true,
			ReasoningLevels: []string{"high"},
		},
		ModelCfg: config.SelectedModel{Provider: "deepseek", ReasoningEffort: "high"},
	}

	opts, optionsErr := getProviderOptions(model, config.ProviderConfig{ID: "deepseek", Type: catalog.TypeOpenAICompat}, providerregistry.Registration{})
	require.NoError(t, optionsErr)
	raw, ok := opts[openaicompat.Name]
	require.True(t, ok)
	parsed, ok := raw.(*openaicompat.ProviderOptions)
	require.True(t, ok)
	require.NotNil(t, parsed.ReasoningEffort)
	assert.Equal(t, "high", string(*parsed.ReasoningEffort))
}

func TestPresetOwnerDoesNotInheritLegacyProviderIDRequestShaping(t *testing.T) {
	model := Model{
		CatalogModel: catalog.Model{
			ID:              "glm-5.2",
			CanReason:       true,
			ReasoningLevels: []string{"high"},
		},
		ModelCfg: config.SelectedModel{
			Provider:        string(catalog.ProviderZAI),
			ReasoningEffort: "high",
		},
	}
	providerCfg := config.ProviderConfig{
		ID:     string(catalog.ProviderZAI),
		Type:   openaicompat.Name,
		Preset: &config.ProviderPresetReference{ID: "example.zai-preset", Version: "1"},
	}

	opts, optionsErr := getProviderOptions(model, providerCfg, providerregistry.Registration{})
	require.NoError(t, optionsErr)
	parsed, ok := opts[openaicompat.Name].(*openaicompat.ProviderOptions)
	require.True(t, ok)
	require.NotNil(t, parsed.ReasoningEffort)
	assert.Equal(t, "high", string(*parsed.ReasoningEffort))
	require.NotContains(t, parsed.ExtraBody, "thinking")
}

func TestGeminiProviderDoesNotConfigureThinkingForGPTOSS(t *testing.T) {
	model := Model{
		CatalogModel: catalog.Model{
			ID: "gpt-oss-120b-medium",
		},
		ModelCfg: config.SelectedModel{
			Provider: gemini.ID,
		},
	}

	registration, registered := integratedRegistration(t, gemini.ID)
	require.True(t, registered)
	opts, optionsErr := getProviderOptions(model, config.ProviderConfig{ID: gemini.ID}, registration)
	require.NoError(t, optionsErr)
	parsed, ok := opts[antigravity.Name].(*antigravity.ProviderOptions)
	require.True(t, ok)
	assert.Nil(t, parsed.ThinkingConfig)
}

func TestIsUnauthorized(t *testing.T) {
	t.Run("nil error", func(t *testing.T) {
		assert.False(t, isUnauthorized(nil))
	})

	t.Run("non-provider error", func(t *testing.T) {
		assert.False(t, isUnauthorized(errors.New("something broke")))
	})

	t.Run("provider error with 401", func(t *testing.T) {
		err := &fantasy.ProviderError{StatusCode: http.StatusUnauthorized, Message: "unauthorized"}
		assert.True(t, isUnauthorized(err))
	})

	t.Run("provider error with non-401", func(t *testing.T) {
		err := &fantasy.ProviderError{StatusCode: http.StatusForbidden, Message: "forbidden"}
		assert.False(t, isUnauthorized(err))
	})

	t.Run("wrapped provider error with 401", func(t *testing.T) {
		inner := &fantasy.ProviderError{StatusCode: http.StatusUnauthorized, Message: "expired"}
		err := fmt.Errorf("request failed: %w", inner)
		assert.True(t, isUnauthorized(err))
	})
}

func TestGetProviderOptionsReasoningEffortFallback(t *testing.T) {
	model := Model{
		CatalogModel: catalog.Model{
			ID:              "glm-5.2",
			CanReason:       true,
			ReasoningLevels: []string{"high", "max"},
		},
		ModelCfg: config.SelectedModel{
			Provider: "zai",
		},
	}
	providerCfg := config.ProviderConfig{
		ID:   string(catalog.ProviderZAI),
		Type: openaicompat.Name,
	}

	opts, optionsErr := getProviderOptions(model, providerCfg, providerregistry.Registration{})
	require.NoError(t, optionsErr)

	raw, ok := opts[openaicompat.Name]
	require.True(t, ok)
	parsed, ok := raw.(*openaicompat.ProviderOptions)
	require.True(t, ok)
	require.NotNil(t, parsed.ReasoningEffort)
	assert.Equal(t, "high", string(*parsed.ReasoningEffort))

	thinking, ok := parsed.ExtraBody["thinking"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "enabled", thinking["type"])
}

func TestOAuthModelCatalogsEnableReasoningByDefault(t *testing.T) {
	tests := []struct {
		name   string
		models []catalog.Model
		levels []string
		effort string
	}{
		{name: codex.ID, models: codex.Models(), levels: []string{"low", "medium", "high", "xhigh"}, effort: "medium"},
		{name: gemini.ID, models: gemini.Models(), levels: []string{"LOW", "MEDIUM", "HIGH"}, effort: "MEDIUM"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.NotEmpty(t, tc.models)
			for _, model := range tc.models {
				if model.ID == "gpt-oss-120b-medium" {
					assert.False(t, model.CanReason, model.ID)
					assert.Empty(t, model.ReasoningLevels, model.ID)
					assert.Empty(t, model.DefaultReasoningEffort, model.ID)
					continue
				}
				assert.True(t, model.CanReason, model.ID)
				assert.Equal(t, tc.levels, model.ReasoningLevels, model.ID)
				assert.Equal(t, tc.effort, model.DefaultReasoningEffort, model.ID)
			}
		})
	}
}

func TestAuthRefreshCallbackRejectsReplacementOwnerGeneration(t *testing.T) {
	registration := providerregistry.Registration{
		ProviderID:       "refresh-owner-test",
		AccountNamespace: "plugin.one.accounts",
		Construction:     providerregistry.ConstructionGenericJSON,
		OAuth:            &providerregistry.OAuthCapability{},
		Manifest:         &manifest.Manifest{ID: "plugin.one", Version: "1.0.0"},
	}
	_, err := providerregistry.New(registration)
	require.NoError(t, err)
	provider := config.ProviderConfig{
		ID:             registration.ProviderID,
		APIKey:         "old-generation-key",
		APIKeyTemplate: "$OLD_GENERATION_KEY",
		Owner:          &config.ProviderOwnerReference{Type: config.ProviderOwnerPlugin, Construction: registration.Construction},
		Plugin:         &config.ProviderPluginReference{ID: registration.Manifest.ID, Version: registration.Manifest.Version},
	}
	oldStore := config.NewTestStoreWithRegistrations(&config.Config{
		Providers: csync.NewMapFrom(map[string]config.ProviderConfig{registration.ProviderID: provider}),
	}, registration)
	coord := &coordinator{cfg: oldStore}
	snapshot := oldStore.RuntimeSnapshot()
	_, active := snapshot.ProviderRegistrationFor(registration.ProviderID, provider)
	require.True(t, active)
	_, active = snapshot.ProviderOwnerFor(registration.ProviderID, provider)
	require.True(t, active)
	callback := coord.makeAuthRefreshCallback(snapshot, provider)
	require.NotNil(t, callback)

	replacement := registration
	replacement.AccountNamespace = "plugin.two.accounts"
	replacement.Manifest = &manifest.Manifest{ID: "plugin.two", Version: "2.0.0"}
	replacementProvider := provider
	replacementProvider.APIKey = "new-generation-key"
	replacementProvider.APIKeyTemplate = "$NEW_GENERATION_KEY"
	replacementProvider.Plugin = &config.ProviderPluginReference{ID: replacement.Manifest.ID, Version: replacement.Manifest.Version}
	coord.cfg = config.NewTestStoreWithRegistrations(&config.Config{
		Providers: csync.NewMapFrom(map[string]config.ProviderConfig{replacement.ProviderID: replacementProvider}),
	}, replacement)

	err = callback(t.Context(), &fantasy.ProviderError{StatusCode: http.StatusUnauthorized})
	require.ErrorContains(t, err, "changed")
	actual, ok := coord.cfg.Config().Providers.Get(replacement.ProviderID)
	require.True(t, ok)
	require.Equal(t, replacementProvider, actual)
}

func TestReAuthenticateNotificationPreservesCapturedOwner(t *testing.T) {
	registration := providerregistry.Registration{
		ProviderID:       "reauth-owner-test",
		AccountNamespace: "plugin.reauth.accounts",
		Construction:     providerregistry.ConstructionGenericJSON,
		OAuth: &providerregistry.OAuthCapability{
			Adapter: providerregistry.LoginBrowser,
			FlowID:  "plugin.reauth.flow",
			Refresh: func(context.Context, string) (*oauth.Token, error) {
				return nil, &oauth.TokenExchangeError{StatusCode: http.StatusBadRequest, Body: `{"error":"invalid_grant"}`}
			},
		},
		Manifest: &manifest.Manifest{ID: "plugin.reauth", Version: "1.0.0"},
	}
	provider := config.ProviderConfig{
		ID:         registration.ProviderID,
		OAuthToken: &oauth.Token{AccessToken: "expired-access", RefreshToken: "revoked-refresh"},
		Owner: &config.ProviderOwnerReference{
			Type:         config.ProviderOwnerPlugin,
			Construction: registration.Construction,
		},
		Plugin: &config.ProviderPluginReference{ID: registration.Manifest.ID, Version: registration.Manifest.Version},
	}
	store := config.NewTestStoreWithRegistrations(&config.Config{
		Providers: csync.NewMapFrom(map[string]config.ProviderConfig{provider.ID: provider}),
	}, registration)
	broker := pubsub.NewBroker[notify.Notification]()
	t.Cleanup(broker.Shutdown)
	events := broker.Subscribe(t.Context())
	coord := &coordinator{cfg: store, notify: broker}
	owner := registration.Owner()
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		result <- coord.retryAfterUnauthorized(ctx, store.RuntimeSnapshot(), owner, provider)
	}()

	select {
	case event := <-events:
		require.Equal(t, pubsub.CreatedEvent, event.Type)
		require.Equal(t, notify.TypeReAuthenticate, event.Payload.Type)
		require.Equal(t, owner.ProviderID, event.Payload.ProviderID)
		require.Equal(t, owner, event.Payload.Owner)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reauthentication notification")
	}
	cancel()
	store.SignalAuthComplete(owner)
	require.ErrorIs(t, <-result, context.Canceled)
}

func TestIsUnsupportedReasoningMessage(t *testing.T) {
	for _, message := range []string{
		"unsupported reasoning metadata",
		"thinking is not supported for this model",
		"invalid reasoning effort",
		"thinking error",
	} {
		assert.True(t, isUnsupportedReasoningMessage(message), message)
	}
	for _, message := range []string{
		"reasoning completed",
		"request failed",
		"unsupported image metadata",
	} {
		assert.False(t, isUnsupportedReasoningMessage(message), message)
	}
}

func TestBuildAgentModelsPinsRuntimeToEachOAuthProvider(t *testing.T) {
	env := testEnv(t)
	largeCatalog := codex.Models()[0]
	largeCatalog.CanReason = true
	largeCatalog.ReasoningLevels = []string{"low", "medium", "high", "xhigh"}
	largeCatalog.DefaultReasoningEffort = "medium"
	smallCatalog := gemini.Models()[0]
	smallCatalog.CanReason = true
	smallCatalog.ReasoningLevels = []string{"LOW", "MEDIUM", "HIGH"}
	smallCatalog.DefaultReasoningEffort = "MEDIUM"
	future := time.Now().Add(time.Hour).Unix()
	largeProvider := config.ProviderConfig{
		ID:                 codex.ID,
		Owner:              &config.ProviderOwnerReference{Type: config.ProviderOwnerCore, Construction: providerregistry.ConstructionCodex},
		BaseURL:            "wss://codex.example.test/responses",
		APIKey:             "codex-token",
		OAuthToken:         &oauth.Token{AccessToken: "codex-token", RefreshToken: "codex-refresh", ExpiresAt: future},
		SystemPromptPrefix: "codex-prefix",
		Models:             []catalog.Model{largeCatalog},
	}
	smallProvider := config.ProviderConfig{
		ID:                 gemini.ID,
		Owner:              &config.ProviderOwnerReference{Type: config.ProviderOwnerCore, Construction: providerregistry.ConstructionGeminiAntigravity},
		BaseURL:            "https://gemini.example.test",
		APIKey:             "gemini-token",
		OAuthToken:         &oauth.Token{AccessToken: "gemini-token", RefreshToken: "gemini-refresh", ExpiresAt: future},
		SystemPromptPrefix: "gemini-prefix",
		Models:             []catalog.Model{smallCatalog},
	}
	coord := newTestCoordinator(t, env, codex.ID, largeProvider)
	coord.cfg.Config().Providers.Set(gemini.ID, smallProvider)
	coord.cfg.Config().Models[config.SelectedModelTypeLarge] = config.SelectedModel{
		Provider: codex.ID,
		Model:    largeCatalog.ID,
	}
	coord.cfg.Config().Models[config.SelectedModelTypeSmall] = config.SelectedModel{
		Provider: gemini.ID,
		Model:    smallCatalog.ID,
	}
	codexRegistration, codexRegistered := integratedRegistration(t, codex.ID)
	require.True(t, codexRegistered)
	geminiRegistration, geminiRegistered := integratedRegistration(t, gemini.ID)
	require.True(t, geminiRegistered)
	coord.cfg = config.NewTestStoreWithRegistrations(coord.cfg.Config(), codexRegistration, geminiRegistration)

	large, small, err := coord.buildAgentModels(t.Context(), config.Agent{Model: config.SelectedModelTypeLarge}, false)
	require.NoError(t, err)
	require.Equal(t, codex.ID, large.ModelCfg.Provider)
	require.Equal(t, "codex-prefix", large.SystemPromptPrefix)
	require.NotNil(t, large.OnAuthRefresh)
	expectedCodexImagePolicy, ok := imageattachment.PolicyFromDeclaration(codexRegistration.Images)
	require.True(t, ok)
	require.Equal(t, &expectedCodexImagePolicy, large.ImagePolicy)
	require.Contains(t, large.ProviderOptions, codexresponses.Name)
	require.NotContains(t, large.ProviderOptions, antigravity.Name)
	require.Equal(t, gemini.ID, small.ModelCfg.Provider)
	require.Equal(t, "gemini-prefix", small.SystemPromptPrefix)
	require.NotNil(t, small.OnAuthRefresh)
	expectedGeminiImagePolicy, ok := imageattachment.PolicyFromDeclaration(geminiRegistration.Images)
	require.True(t, ok)
	require.Equal(t, &expectedGeminiImagePolicy, small.ImagePolicy)
	require.Contains(t, small.ProviderOptions, antigravity.Name)
	require.NotContains(t, small.ProviderOptions, codexresponses.Name)

	customCatalog := catalog.Model{ID: "organization/custom/model", DefaultMaxTokens: 8192}
	customProvider := config.ProviderConfig{
		ID:                 "custom-provider",
		Owner:              &config.ProviderOwnerReference{Type: config.ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat},
		Type:               openaicompat.Name,
		BaseURL:            "https://custom.example.test/v1",
		APIKey:             "custom-token",
		OAuthToken:         &oauth.Token{AccessToken: "custom-token", RefreshToken: "custom-refresh", ExpiresAt: future},
		SystemPromptPrefix: "custom-prefix",
		Models:             []catalog.Model{customCatalog},
	}
	coord.cfg.Config().Providers.Set(customProvider.ID, customProvider)
	customSelection := config.SelectedModel{Provider: customProvider.ID, Model: customCatalog.ID}
	custom, customSmall, err := coord.buildAgentModels(t.Context(), config.Agent{
		PrimaryModelOverride: &customSelection,
	}, true)
	require.NoError(t, err)
	require.Equal(t, customProvider.ID, custom.ModelCfg.Provider)
	require.Equal(t, customCatalog.ID, custom.ModelCfg.Model)
	require.Equal(t, "custom-prefix", custom.SystemPromptPrefix)
	require.NotNil(t, custom.OnAuthRefresh)
	require.Nil(t, custom.ImagePolicy)
	require.Contains(t, custom.ProviderOptions, openaicompat.Name)
	require.NotContains(t, custom.ProviderOptions, codexresponses.Name)
	require.Equal(t, gemini.ID, customSmall.ModelCfg.Provider)
	require.Equal(t, "gemini-prefix", customSmall.SystemPromptPrefix)
	require.NotNil(t, customSmall.OnAuthRefresh)
	require.Contains(t, customSmall.ProviderOptions, antigravity.Name)

	unavailableProvider := customProvider
	unavailableProvider.ID = "unavailable-provider"
	unavailableProvider.Owner = &config.ProviderOwnerReference{Type: config.ProviderOwnerPlugin, Construction: providerregistry.ConstructionGenericJSON}
	unavailableProvider.Plugin = &config.ProviderPluginReference{ID: "missing.plugin", Version: "1"}
	coord.cfg.Config().Providers.Set(unavailableProvider.ID, unavailableProvider)
	unavailableSelection := config.SelectedModel{Provider: unavailableProvider.ID, Model: customCatalog.ID}
	_, _, err = coord.buildAgentModels(t.Context(), config.Agent{PrimaryModelOverride: &unavailableSelection}, true)
	require.ErrorContains(t, err, "is not available")
}

func TestOAuthReasoningOptionsDisablesCanonicalAdapters(t *testing.T) {
	cfg := initTestConfig(t, t.TempDir())
	cfg.Config().Providers.Set(codex.ID, config.ProviderConfig{ID: codex.ID})
	cfg.Config().Providers.Set(gemini.ID, config.ProviderConfig{ID: gemini.ID})
	coord := &coordinator{cfg: cfg}
	codexRegistration, codexRegistered := integratedRegistration(t, codex.ID)
	geminiRegistration, geminiRegistered := integratedRegistration(t, gemini.ID)

	codexOriginal := &codexresponses.ProviderOptions{ReasoningEffort: "high"}
	coord.disableOAuthReasoningForRegistration(codex.ID, "codex-model", codexRegistration, codexRegistered, "reasoning not supported")
	codexResult := coord.oauthReasoningOptionsForRegistration(codex.ID, "codex-model", codexRegistration, codexRegistered, fantasy.ProviderOptions{codexresponses.Name: codexOriginal})
	codexDisabled := codexResult[codexresponses.Name].(*codexresponses.ProviderOptions)
	assert.True(t, codexDisabled.DisableReasoning)
	assert.Empty(t, codexDisabled.ReasoningEffort)
	assert.False(t, codexOriginal.DisableReasoning)
	assert.Equal(t, "high", codexOriginal.ReasoningEffort)

	includeThoughts := true
	geminiOriginal := &antigravity.ProviderOptions{ThinkingConfig: &antigravity.ThinkingConfig{IncludeThoughts: &includeThoughts}}
	coord.disableOAuthReasoningForRegistration(gemini.ID, "gemini-model", geminiRegistration, geminiRegistered, "thinking error")
	geminiResult := coord.oauthReasoningOptionsForRegistration(gemini.ID, "gemini-model", geminiRegistration, geminiRegistered, fantasy.ProviderOptions{antigravity.Name: geminiOriginal})
	geminiDisabled := geminiResult[antigravity.Name].(*antigravity.ProviderOptions)
	assert.Nil(t, geminiDisabled.ThinkingConfig)
	assert.NotNil(t, geminiOriginal.ThinkingConfig)

	otherModelOptions := fantasy.ProviderOptions{codexresponses.Name: codexOriginal}
	otherModelResult := coord.oauthReasoningOptionsForRegistration(codex.ID, "another-model", codexRegistration, codexRegistered, otherModelOptions)
	assert.Same(t, codexOriginal, otherModelResult[codexresponses.Name])

	// Learned fallback is intentionally coordinator-local. A restarted
	// coordinator retries the configured reasoning policy instead of inheriting
	// stale process state from a previous provider instance.
	restarted := &coordinator{cfg: cfg}
	restartedResult := restarted.oauthReasoningOptionsForRegistration(codex.ID, "codex-model", codexRegistration, codexRegistered, fantasy.ProviderOptions{codexresponses.Name: codexOriginal})
	assert.Same(t, codexOriginal, restartedResult[codexresponses.Name])
}

func TestRunSubAgentDisablesReasoningAfterProviderWarning(t *testing.T) {
	env := testEnv(t)
	providerCfg := config.ProviderConfig{
		ID:    codex.ID,
		Owner: &config.ProviderOwnerReference{Type: config.ProviderOwnerCore, Construction: providerregistry.ConstructionCodex},
	}
	coord := newTestCoordinator(t, env, codex.ID, providerCfg)
	registration, registered := integratedRegistration(t, codex.ID)
	require.True(t, registered)
	coord.cfg = config.NewTestStoreWithRegistrations(coord.cfg.Config(), registration)
	parentSession, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)

	agent := newMockAgent(codex.ID, 4096, coord.cfg.RuntimeSnapshot(), func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		require.NotNil(t, call.OnProviderWarning)
		call.OnProviderWarning(fantasy.CallWarning{Message: "unsupported reasoning metadata"})
		return agentResultWithText("done"), nil
	})
	agent.model.ModelCfg.Model = "gpt-test"

	_, err = coord.runSubAgent(t.Context(), subAgentParams{
		Agent:          agent,
		SessionID:      parentSession.ID,
		AgentMessageID: "msg-1",
		ToolCallID:     "call-1",
		Prompt:         "test",
		SessionTitle:   "Test",
	})
	require.NoError(t, err)
	assert.True(t, coord.reasoningDisabled[oauthReasoningKey(codex.ID, "gpt-test")])
}
