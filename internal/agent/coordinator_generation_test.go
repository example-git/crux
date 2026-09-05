package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/session"
	"github.com/example-git/crux/internal/skills"
	"github.com/stretchr/testify/require"
)

func TestBuildAgentModelsUsesOneGenerationDuringReload(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	dataDir := filepath.Join(root, "data")
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	t.Setenv("CRUX_GLOBAL_CONFIG", configDir)
	t.Setenv("CRUX_GLOBAL_DATA", filepath.Join(root, "global-data"))
	t.Setenv("CRUX_CACHE_DIR", filepath.Join(root, "cache"))
	t.Setenv("AI_CLI_DIR", filepath.Join(root, "accounts"))
	configPath := filepath.Join(configDir, "crux.json")
	generation := func(id string) []byte {
		return []byte(fmt.Sprintf(`{"options":{"disable_default_providers":true},"providers":{"provider-%[1]s":{"type":"openai-compat","base_url":"https://%[1]s.example.invalid/v1","api_key":"key-%[1]s","models":[{"id":"model-%[1]s","name":"Model %[1]s"}]}},"models":{"large":{"provider":"provider-%[1]s","model":"model-%[1]s"},"small":{"provider":"provider-%[1]s","model":"model-%[1]s"}}}`, id))
	}
	require.NoError(t, os.WriteFile(configPath, generation("a"), 0o600))
	store, err := config.Load(root, dataDir, false)
	require.NoError(t, err)
	coord := &coordinator{cfg: store}
	agentCfg := config.Agent{Model: config.SelectedModelTypeLarge}
	errors := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for index := 0; index < 40; index++ {
			id := "a"
			if index%2 == 0 {
				id = "b"
			}
			if writeErr := os.WriteFile(configPath, generation(id), 0o600); writeErr != nil {
				errors <- writeErr
				return
			}
			if reloadErr := store.ReloadFromDisk(t.Context()); reloadErr != nil {
				errors <- reloadErr
				return
			}
			runtime.Gosched()
		}
	}()
	for index := 0; index < 400; index++ {
		primary, small, buildErr := coord.buildAgentModels(t.Context(), agentCfg, false)
		require.NoError(t, buildErr)
		require.Equal(t, primary.ModelCfg.Provider, small.ModelCfg.Provider)
		require.Equal(t, primary.ModelCfg.Model, small.ModelCfg.Model)
		validA := primary.ModelCfg.Provider == "provider-a" && primary.ModelCfg.Model == "model-a"
		validB := primary.ModelCfg.Provider == "provider-b" && primary.ModelCfg.Model == "model-b"
		require.True(t, validA || validB)
		runtime.Gosched()
	}
	<-done
	select {
	case goroutineErr := <-errors:
		require.NoError(t, goroutineErr)
	default:
	}
}

func TestReloadPublishesActiveRuntimeWithConfigurationGeneration(t *testing.T) {
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "true")
	environment := testEnv(t)
	oldSkills := filepath.Join(environment.workingDir, "old-skills")
	newSkills := filepath.Join(environment.workingDir, "new-skills")
	for path, name := range map[string]string{oldSkills: "old-generation-skill", newSkills: "new-generation-skill"} {
		directory := filepath.Join(path, name)
		require.NoError(t, os.MkdirAll(directory, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(directory, skills.SkillFileName), []byte("---\nname: "+name+"\ndescription: Generation marker.\n---\nmarker\n"), 0o600))
	}
	configPath := filepath.Join(environment.workingDir, "crux.json")
	generation := func(id, skillsPath string, disableJQ, disableAutoSummarize bool) []byte {
		disabledTools := "[]"
		if disableJQ {
			disabledTools = `["jq"]`
		}
		return []byte(fmt.Sprintf(`{"env":{"T3_9_RUNTIME_GENERATION":"%[1]s"},"options":{"disable_default_providers":true,"disable_auto_summarize":%[2]t,"disabled_tools":%[3]s,"skills_paths":[%[4]q]},"providers":{"provider-%[1]s":{"type":"openai-compat","base_url":"https://%[1]s.example.invalid/v1","api_key":"key-%[1]s","system_prompt_prefix":"prefix-%[1]s","models":[{"id":"model-%[1]s","name":"Model %[1]s"}]}},"models":{"large":{"provider":"provider-%[1]s","model":"model-%[1]s"},"small":{"provider":"provider-%[1]s","model":"model-%[1]s"}}}`, id, disableAutoSummarize, disabledTools, skillsPath))
	}
	require.NoError(t, os.WriteFile(configPath, generation("old", oldSkills, true, true), 0o600))
	store := initTestConfig(t, environment.workingDir)
	current := &mockSessionAgent{}
	coord := &coordinator{
		cfg:          store,
		sessions:     environment.sessions,
		messages:     environment.messages,
		permissions:  environment.permissions,
		history:      environment.history,
		filetracker:  *environment.filetracker,
		currentAgent: current,
		skillTracker: skills.NewTracker(nil),
	}
	require.NoError(t, coord.UpdateModels(t.Context()))
	store.SetRuntimeGenerationPreparer(coord.prepareRuntimeGeneration)
	require.NoError(t, os.WriteFile(configPath, generation("new", newSkills, false, false), 0o600))

	require.NoError(t, store.ReloadFromDisk(t.Context()))
	require.Equal(t, "provider-new", current.model.ModelCfg.Provider)
	require.Equal(t, "model-new", current.model.ModelCfg.Model)
	require.Equal(t, "provider-new", current.smallModel.ModelCfg.Provider)
	require.Equal(t, "prefix-new", current.model.SystemPromptPrefix)
	require.False(t, current.disableAutoSummarize)
	require.Contains(t, current.instructions.String(), "new-generation-skill")
	require.NotContains(t, current.instructions.String(), "old-generation-skill")
	var toolNames []string
	for _, tool := range current.tools {
		toolNames = append(toolNames, tool.Info().Name)
	}
	require.Contains(t, toolNames, tools.JQToolName)
	installed := current.Runtime()
	require.Same(t, store.Config(), installed.Snapshot.Config())
	require.Equal(t, "provider-new", installed.Snapshot.Config().Models[config.SelectedModelTypeLarge].Provider)
	resolvedGeneration, err := installed.Snapshot.Resolve("$T3_9_RUNTIME_GENERATION")
	require.NoError(t, err)
	require.Equal(t, "new", resolvedGeneration)
	require.Equal(t, "new", os.Getenv("T3_9_RUNTIME_GENERATION"))
	created, err := environment.sessions.Create(t.Context(), "generation")
	require.NoError(t, err)
	lifecycle, err := installed.SystemPromptBuilder(t.Context(), created, installed.LargeModel)
	require.NoError(t, err)
	require.Contains(t, lifecycle.String(), "new-generation-skill")
	require.NotContains(t, lifecycle.String(), "old-generation-skill")

	publishedConfig := store.Config()
	publishedRuntime := current.Runtime()
	store.SetRuntimeGenerationPreparer(func(ctx context.Context, snapshot config.RuntimeSnapshot) (config.RuntimeGenerationCandidate, error) {
		candidate, err := coord.prepareRuntimeGeneration(ctx, snapshot)
		if err != nil {
			return config.RuntimeGenerationCandidate{}, err
		}
		candidate.Abort()
		return config.RuntimeGenerationCandidate{}, fmt.Errorf("runtime preparation blocked")
	})
	require.NoError(t, os.WriteFile(configPath, generation("blocked", oldSkills, true, true), 0o600))
	err = store.ReloadFromDisk(t.Context())
	require.ErrorContains(t, err, "runtime preparation blocked")
	require.Same(t, publishedConfig, store.Config())
	retained := current.Runtime()
	require.Same(t, publishedRuntime.Snapshot.Config(), retained.Snapshot.Config())
	require.Equal(t, publishedRuntime.LargeModel.ModelCfg, retained.LargeModel.ModelCfg)
	require.Equal(t, publishedRuntime.SmallModel.ModelCfg, retained.SmallModel.ModelCfg)
	require.Equal(t, publishedRuntime.Instructions.String(), retained.Instructions.String())
	require.Equal(t, publishedRuntime.DisableAutoSummarize, retained.DisableAutoSummarize)
	require.Equal(t, "new", os.Getenv("T3_9_RUNTIME_GENERATION"))
	resolvedGeneration, err = retained.Snapshot.Resolve("$T3_9_RUNTIME_GENERATION")
	require.NoError(t, err)
	require.Equal(t, "new", resolvedGeneration)
}

func TestReloadAndUpdateModelsCannotPublishStaleRuntime(t *testing.T) {
	environment := testEnv(t)
	configPath := filepath.Join(environment.workingDir, "crux.json")
	generation := func(id string) []byte {
		return []byte(fmt.Sprintf(`{"options":{"disable_default_providers":true},"providers":{"provider-%[1]s":{"type":"openai-compat","base_url":"https://%[1]s.example.invalid/v1","api_key":"key-%[1]s","system_prompt_prefix":"prefix-%[1]s","models":[{"id":"model-%[1]s","name":"Model %[1]s"}]}},"models":{"large":{"provider":"provider-%[1]s","model":"model-%[1]s"},"small":{"provider":"provider-%[1]s","model":"model-%[1]s"}}}`, id))
	}
	require.NoError(t, os.WriteFile(configPath, generation("a"), 0o600))
	store := initTestConfig(t, environment.workingDir)
	current := testSessionAgent(environment, nil, nil, "initial")
	coord := &coordinator{
		cfg:          store,
		sessions:     environment.sessions,
		messages:     environment.messages,
		permissions:  environment.permissions,
		history:      environment.history,
		filetracker:  *environment.filetracker,
		currentAgent: current,
		skillTracker: skills.NewTracker(nil),
	}
	require.NoError(t, coord.UpdateModels(t.Context()))
	store.SetRuntimeGenerationPreparer(coord.prepareRuntimeGeneration)

	errors := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for index := 0; index < 20; index++ {
			id := "a"
			if index%2 == 0 {
				id = "b"
			}
			if err := os.WriteFile(configPath, generation(id), 0o600); err != nil {
				errors <- err
				return
			}
			if err := store.ReloadFromDisk(t.Context()); err != nil {
				errors <- err
				return
			}
			runtime.Gosched()
		}
	}()
	for index := 0; index < 100; index++ {
		if err := coord.UpdateModels(t.Context()); err != nil {
			require.ErrorContains(t, err, "agent model generation changed before runtime publication")
			continue
		}
		installed := current.Runtime()
		selected := installed.Snapshot.Config().Models[config.SelectedModelTypeLarge]
		require.Equal(t, selected.Provider, installed.LargeModel.ModelCfg.Provider)
		require.Equal(t, selected.Model, installed.LargeModel.ModelCfg.Model)
		require.Equal(t, installed.LargeModel.ModelCfg, installed.SmallModel.ModelCfg)
		require.Equal(t, "prefix-"+strings.TrimPrefix(selected.Provider, "provider-"), installed.LargeModel.SystemPromptPrefix)
		runtime.Gosched()
	}
	<-done
	select {
	case err := <-errors:
		require.NoError(t, err)
	default:
	}
	require.NoError(t, coord.UpdateModels(t.Context()))
	require.Same(t, store.Config(), current.Runtime().Snapshot.Config())
}

func TestSetRuntimePublishesOneAtomicTuple(t *testing.T) {
	environment := testEnv(t)
	agent := testSessionAgent(environment, nil, nil, "initial").(*sessionAgent)
	runtimeFor := func(id string, disable bool) InstalledRuntime {
		tool := fantasy.NewAgentTool[struct{}](id+"-tool", id, func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse(id), nil
		})
		planTool := fantasy.NewAgentTool[struct{}](id+"-plan-tool", id, func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse(id), nil
		})
		return InstalledRuntime{
			LargeModel:           Model{ModelCfg: config.SelectedModel{Provider: id, Model: id}},
			SmallModel:           Model{ModelCfg: config.SelectedModel{Provider: id, Model: id}},
			Instructions:         fantasy.NewInstructions(fantasy.StaticInstruction(fantasy.InstructionKindTooling, id)),
			Tools:                []fantasy.AgentTool{tool},
			PlanModeTools:        []fantasy.AgentTool{planTool},
			DisableAutoSummarize: disable,
			SystemPromptBuilder: func(context.Context, session.Session, Model) (fantasy.Instructions, error) {
				return fantasy.NewInstructions(fantasy.DynamicInstruction(fantasy.InstructionKindLifecycle, id)), nil
			},
			Snapshot: config.NewTestStore(&config.Config{
				Schema: id,
			}).RuntimeSnapshot(),
		}
	}
	first := runtimeFor("first", false)
	second := runtimeFor("second", true)
	agent.SetRuntime(first)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for index := 0; index < 1000; index++ {
			if index%2 == 0 {
				agent.SetRuntime(second)
			} else {
				agent.SetRuntime(first)
			}
		}
	}()
	for index := 0; index < 1000; index++ {
		installed := agent.Runtime()
		id := installed.LargeModel.ModelCfg.Model
		require.Equal(t, id, installed.SmallModel.ModelCfg.Model)
		require.Equal(t, id, installed.Instructions.String())
		require.Equal(t, id+"-tool", installed.Tools[0].Info().Name)
		require.Equal(t, id+"-plan-tool", installed.PlanModeTools[0].Info().Name)
		require.Equal(t, id == "second", installed.DisableAutoSummarize)
		require.Equal(t, id, installed.Snapshot.Config().Schema)
		lifecycle, err := installed.SystemPromptBuilder(t.Context(), session.Session{}, installed.LargeModel)
		require.NoError(t, err)
		require.Equal(t, id, lifecycle.String())
	}
	<-done
}
