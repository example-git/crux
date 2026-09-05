package dialog

import (
	"context"
	"errors"
	"testing"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

type agentDefinitionsTestWorkspace struct {
	workspace.Workspace
	cfg          *config.Config
	created      []proto.CreateAgentDefinitionRequest
	path         string
	createErr    error
	refreshErr   error
	refreshCalls int
	refreshState config.AgentModelState
}

func (w *agentDefinitionsTestWorkspace) Config() *config.Config {
	return w.cfg
}

func (w *agentDefinitionsTestWorkspace) CreateAgentDefinition(_ context.Context, request proto.CreateAgentDefinitionRequest) (string, error) {
	w.created = append(w.created, request)
	return w.path, w.createErr
}

func (w *agentDefinitionsTestWorkspace) UpdateAgentModel(_ context.Context, state config.AgentModelState) error {
	w.refreshCalls++
	w.refreshState = state
	return w.refreshErr
}

func newAgentDefinitionsTestDialog(t *testing.T) (*AgentDefinitions, *agentDefinitionsTestWorkspace) {
	t.Helper()
	ws := &agentDefinitionsTestWorkspace{
		cfg: &config.Config{
			Models: map[config.SelectedModelType]config.SelectedModel{
				config.SelectedModelTypeLarge: {Provider: "zeta", Model: "large"},
			},
			Providers: csync.NewMapFrom(map[string]config.ProviderConfig{
				"zeta": {
					ID:     "zeta",
					Models: []catalog.Model{{ID: "small"}, {ID: "large"}},
				},
				"alpha": {
					ID:     "alpha",
					Models: []catalog.Model{{ID: "model"}},
				},
				"disabled": {
					ID:      "disabled",
					Disable: true,
					Models:  []catalog.Model{{ID: "hidden"}},
				},
			}),
			Options: &config.Options{},
		},
		path: "/workspace/.ai-cli/agents/reviewer.md",
	}
	theme := styles.ThemeForProvider("")
	return NewAgentDefinitions(&common.Common{Workspace: ws, Styles: &theme}), ws
}

func TestConfiguredAgentModelsOmitInactiveIntegrations(t *testing.T) {
	cfg := &config.Config{Providers: csync.NewMapFrom(map[string]config.ProviderConfig{
		"available": {
			ID:     "available",
			Models: []catalog.Model{{ID: "model"}},
		},
		"inactive": {
			ID:     "inactive",
			Plugin: &config.ProviderPluginReference{ID: "inactive.plugin", Version: "1"},
			Models: []catalog.Model{{ID: "hidden"}},
		},
	})}

	require.Equal(t, []string{"available/model"}, configuredAgentModels(cfg))
	_, preserved := cfg.Providers.Get("inactive")
	require.True(t, preserved, "filtering presentation must not delete persisted plugin configuration")
}

func TestAgentDefinitionsBuildsSelectedConfiguration(t *testing.T) {
	dialog, _ := newAgentDefinitionsTestDialog(t)
	require.Equal(t, "project", dialog.scope)
	require.Equal(t, []string{"alpha/model", "zeta/large", "zeta/small"}, dialog.models)
	require.Equal(t, "zeta/large", dialog.models[dialog.model])

	name := dialog.inputs[agentDefinitionFieldName]
	name.SetValue("reviewer")
	dialog.inputs[agentDefinitionFieldName] = name
	description := dialog.inputs[agentDefinitionFieldDescription]
	description.SetValue("Reviews changes")
	dialog.inputs[agentDefinitionFieldDescription] = description
	tools := dialog.inputs[agentDefinitionFieldTools]
	tools.SetValue("view, search")
	dialog.inputs[agentDefinitionFieldTools] = tools
	dialog.scope = "user"

	request, err := dialog.request()
	require.NoError(t, err)
	require.Equal(t, proto.CreateAgentDefinitionRequest{
		Scope:       "user",
		Name:        "reviewer",
		Description: "Reviews changes",
		Model:       "zeta/large",
		Tools:       []string{"view", "search"},
	}, request)
}

func TestAgentDefinitionsBuildsScriptConfiguration(t *testing.T) {
	dialog, _ := newAgentDefinitionsTestDialog(t)
	name := dialog.inputs[agentDefinitionFieldName]
	name.SetValue("classifier")
	dialog.inputs[agentDefinitionFieldName] = name
	description := dialog.inputs[agentDefinitionFieldDescription]
	description.SetValue("Classifies input")
	dialog.inputs[agentDefinitionFieldDescription] = description
	dialog.script = true
	scriptPath := dialog.inputs[agentDefinitionFieldScriptPath]
	scriptPath.SetValue("./scripts/classify.py")
	dialog.inputs[agentDefinitionFieldScriptPath] = scriptPath
	timeout := dialog.inputs[agentDefinitionFieldScriptTimeout]
	timeout.SetValue("30s")
	dialog.inputs[agentDefinitionFieldScriptTimeout] = timeout
	variables := dialog.inputs[agentDefinitionFieldScriptVariables]
	variables.SetValue(`{"input":{"required":true},"format":{"default":"json","values":["json","text"]},"mode":{"value":"fast"}}`)
	dialog.inputs[agentDefinitionFieldScriptVariables] = variables

	request, err := dialog.request()
	require.NoError(t, err)
	require.Equal(t, []string{"script"}, request.Tools)
	require.Equal(t, "./scripts/classify.py", request.Script.Path)
	require.Equal(t, "30s", request.Script.Timeout)
	require.True(t, request.Script.Variables["input"].Required)
	require.Equal(t, "json", *request.Script.Variables["format"].Default)
	require.Equal(t, []string{"json", "text"}, request.Script.Variables["format"].Values)
	require.Equal(t, "fast", *request.Script.Variables["mode"].Value)
}

func TestAgentDefinitionsRejectsInvalidScriptConfiguration(t *testing.T) {
	dialog, _ := newAgentDefinitionsTestDialog(t)
	name := dialog.inputs[agentDefinitionFieldName]
	name.SetValue("classifier")
	dialog.inputs[agentDefinitionFieldName] = name
	description := dialog.inputs[agentDefinitionFieldDescription]
	description.SetValue("Classifies input")
	dialog.inputs[agentDefinitionFieldDescription] = description

	tools := dialog.inputs[agentDefinitionFieldTools]
	tools.SetValue("script")
	dialog.inputs[agentDefinitionFieldTools] = tools
	_, err := dialog.request()
	require.ErrorContains(t, err, "enable script configuration")

	dialog.script = true
	_, err = dialog.request()
	require.ErrorContains(t, err, "script path is required")

	scriptPath := dialog.inputs[agentDefinitionFieldScriptPath]
	scriptPath.SetValue("script.py")
	dialog.inputs[agentDefinitionFieldScriptPath] = scriptPath
	variables := dialog.inputs[agentDefinitionFieldScriptVariables]
	variables.SetValue(`{"input":{"unknown":true}}`)
	dialog.inputs[agentDefinitionFieldScriptVariables] = variables
	_, err = dialog.request()
	require.ErrorContains(t, err, "unknown field")
}

func TestAgentDefinitionsSaveRunsAsynchronouslyAndReturnsPath(t *testing.T) {
	dialog, ws := newAgentDefinitionsTestDialog(t)
	name := dialog.inputs[agentDefinitionFieldName]
	name.SetValue("reviewer")
	dialog.inputs[agentDefinitionFieldName] = name
	description := dialog.inputs[agentDefinitionFieldDescription]
	description.SetValue("Reviews changes")
	dialog.inputs[agentDefinitionFieldDescription] = description

	action, ok := dialog.save().(ActionCmd)
	require.True(t, ok)
	require.Empty(t, ws.created)
	require.Zero(t, ws.refreshCalls)

	result := action.Cmd()
	require.Len(t, ws.created, 1)
	require.Equal(t, 1, ws.refreshCalls)
	created, ok := dialog.HandleMsg(result).(ActionAgentDefinitionCreated)
	require.True(t, ok)
	require.Equal(t, ws.path, created.Path)
	require.NoError(t, created.RefreshErr)
}

func TestAgentDefinitionsPreservesCreatedPathWhenRefreshFails(t *testing.T) {
	dialog, ws := newAgentDefinitionsTestDialog(t)
	ws.refreshErr = errors.New("refresh failed")
	name := dialog.inputs[agentDefinitionFieldName]
	name.SetValue("reviewer")
	dialog.inputs[agentDefinitionFieldName] = name
	description := dialog.inputs[agentDefinitionFieldDescription]
	description.SetValue("Reviews changes")
	dialog.inputs[agentDefinitionFieldDescription] = description

	action := dialog.save().(ActionCmd)
	created := dialog.HandleMsg(action.Cmd()).(ActionAgentDefinitionCreated)
	require.Equal(t, ws.path, created.Path)
	require.ErrorContains(t, created.RefreshErr, "refresh failed")
}

func TestAgentDefinitionsRejectsInvalidInputs(t *testing.T) {
	dialog, _ := newAgentDefinitionsTestDialog(t)
	_, err := dialog.request()
	require.ErrorContains(t, err, "agent type name is required")

	name := dialog.inputs[agentDefinitionFieldName]
	name.SetValue("reviewer")
	dialog.inputs[agentDefinitionFieldName] = name
	_, err = dialog.request()
	require.ErrorContains(t, err, "description is required")

	description := dialog.inputs[agentDefinitionFieldDescription]
	description.SetValue("Reviews changes")
	dialog.inputs[agentDefinitionFieldDescription] = description
	tools := dialog.inputs[agentDefinitionFieldTools]
	tools.SetValue("*, view")
	dialog.inputs[agentDefinitionFieldTools] = tools
	_, err = dialog.request()
	require.ErrorContains(t, err, "wildcard tool must be the only tool")

	dialog.models = nil
	_, err = dialog.request()
	require.ErrorContains(t, err, "no configured models")
}

func TestParseAgentDefinitionTools(t *testing.T) {
	require.Equal(t, []string{}, mustParseAgentDefinitionTools(t, "none"))
	require.Equal(t, []string{}, mustParseAgentDefinitionTools(t, ""))
	require.Equal(t, []string{"*"}, mustParseAgentDefinitionTools(t, "*"))
	require.Equal(t, []string{"view", "search"}, mustParseAgentDefinitionTools(t, " view, search "))

	_, err := parseAgentDefinitionTools("view,view")
	require.ErrorContains(t, err, "more than once")
	_, err = parseAgentDefinitionTools("view,")
	require.ErrorContains(t, err, "empty name")
}

func mustParseAgentDefinitionTools(t *testing.T, value string) []string {
	t.Helper()
	tools, err := parseAgentDefinitionTools(value)
	require.NoError(t, err)
	return tools
}
