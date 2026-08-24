package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/stretchr/testify/require"
)

func testAgentDefinitionConfig() *config.Config {
	return &config.Config{
		Options: &config.Options{},
		Providers: csync.NewMapFrom(map[string]config.ProviderConfig{
			"provider": {
				ID: "provider",
				Models: []catwalk.Model{
					{ID: "model"},
					{ID: "org/model/version"},
				},
			},
			"disabled": {
				ID:      "disabled",
				Disable: true,
				Models:  []catwalk.Model{{ID: "model"}},
			},
		}),
	}
}

func writeAgentDefinition(t *testing.T, dir, filename, name, model, body string, tools []string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	content := fmt.Sprintf("---\nname: %s\ndescription: Description for %s\nmodel: %s\n", name, name, model)
	if tools != nil {
		content += "tools:\n"
		for _, tool := range tools {
			content += fmt.Sprintf("  - %q\n", tool)
		}
	}
	content += "---\n\n" + body
	path := filepath.Join(dir, filename)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestAgentDefinitionDiscoveryScopesOverrideAndOrdering(t *testing.T) {
	userDir := filepath.Join(t.TempDir(), "user")
	projectDir := filepath.Join(t.TempDir(), "project")
	writeAgentDefinition(t, userDir, "zeta.md", "zeta", "provider/model", "User zeta", nil)
	writeAgentDefinition(t, userDir, "reviewer.md", "reviewer", "provider/model", "User reviewer", []string{"view"})
	writeAgentDefinition(t, projectDir, "reviewer.md", "reviewer", "provider/org/model/version", "Project reviewer", []string{"grep"})
	writeAgentDefinition(t, projectDir, "alpha.md", "alpha", "provider/model", "Project alpha", []string{"*"})
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "ignored.txt"), []byte("ignored"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(projectDir, "nested.md"), 0o755))

	definitions, err := loadAgentDefinitionsFromDirs(userDir, projectDir, testAgentDefinitionConfig())
	require.NoError(t, err)
	require.Len(t, definitions, 3)
	require.Equal(t, []string{"alpha", "reviewer", "zeta"}, []string{definitions[0].Name, definitions[1].Name, definitions[2].Name})
	require.Equal(t, agentDefinitionSourceProject, definitions[1].Source)
	require.Equal(t, "Project reviewer", definitions[1].Instructions)
	require.Equal(t, "org/model/version", definitions[1].Model.Model)
	require.True(t, definitions[0].AllTools)
}

func TestAgentDefinitionDiscoverySupportsSingleScopeAndAbsentDirectories(t *testing.T) {
	userDir := filepath.Join(t.TempDir(), "user")
	projectDir := filepath.Join(t.TempDir(), "missing")
	writeAgentDefinition(t, userDir, "user.md", "user_agent", "provider/model", "User instructions", nil)

	definitions, err := loadAgentDefinitionsFromDirs(userDir, projectDir, testAgentDefinitionConfig())
	require.NoError(t, err)
	require.Len(t, definitions, 1)
	require.Equal(t, "user_agent", definitions[0].Name)
	require.Empty(t, definitions[0].Tools)

	definitions, err = loadAgentDefinitionsFromDirs(filepath.Join(t.TempDir(), "none"), projectDir, testAgentDefinitionConfig())
	require.NoError(t, err)
	require.Empty(t, definitions)
}

func TestAgentDefinitionDiscoveryDefersDuplicateNameError(t *testing.T) {
	dir := t.TempDir()
	writeAgentDefinition(t, dir, "a.md", "reviewer", "provider/model", "First", nil)
	writeAgentDefinition(t, dir, "b.md", "reviewer", "provider/model", "Second", nil)

	definitions, err := loadAgentDefinitionsDir(dir, agentDefinitionSourceUser, testAgentDefinitionConfig())
	require.NoError(t, err)
	require.Len(t, definitions, 1)
	require.ErrorContains(t, definitions[0].ValidationErr, "duplicate user agent definition")
	require.ErrorContains(t, definitions[0].ValidationErr, filepath.Join(dir, "a.md"))
	require.ErrorContains(t, definitions[0].ValidationErr, filepath.Join(dir, "b.md"))
}

func TestAgentDefinitionDiscoveryDefersMalformedDefinitionError(t *testing.T) {
	dir := t.TempDir()
	writeAgentDefinition(t, dir, "valid.md", "valid", "provider/model", "Valid instructions", []string{"view"})
	invalidPath := filepath.Join(dir, "reviewer.md")
	require.NoError(t, os.WriteFile(invalidPath, []byte("---\nname: reviewer\ndescription: Review\nmodel: provider/model\ntoolz: []\n---\nInstructions"), 0o600))

	definitions, err := loadAgentDefinitionsDir(dir, agentDefinitionSourceUser, testAgentDefinitionConfig())
	require.NoError(t, err)
	require.Len(t, definitions, 2)
	require.Equal(t, "reviewer", definitions[0].Name)
	require.ErrorContains(t, definitions[0].ValidationErr, "field toolz not found")
	require.Equal(t, invalidPath, definitions[0].Path)
	require.Equal(t, "valid", definitions[1].Name)
	require.NoError(t, definitions[1].ValidationErr)
}

func TestAgentDefinitionDiscoveryDefersInvalidScriptError(t *testing.T) {
	dir := t.TempDir()
	writeAgentDefinition(t, dir, "runner.md", "runner", "provider/model", "Run script", []string{"script"})

	definitions, err := loadAgentDefinitionsDir(dir, agentDefinitionSourceProject, testAgentDefinitionConfig())
	require.NoError(t, err)
	require.Len(t, definitions, 1)
	require.Equal(t, "runner", definitions[0].Name)
	require.ErrorContains(t, definitions[0].ValidationErr, "requires a script configuration")
}

func TestAgentDefinitionDiscoveryUsesFilenameForMalformedFrontmatter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "researcher.md")
	require.NoError(t, os.WriteFile(path, []byte("---\nname: [\n---\nInstructions"), 0o600))

	definitions, err := loadAgentDefinitionsDir(dir, agentDefinitionSourceUser, testAgentDefinitionConfig())
	require.NoError(t, err)
	require.Len(t, definitions, 1)
	require.Equal(t, "researcher", definitions[0].Name)
	require.ErrorContains(t, definitions[0].ValidationErr, "frontmatter")
}

func TestAgentDefinitionParserHandlesBOMAndCRLF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "definition.md")
	content := "\uFEFF---\r\nname: reviewer\r\ndescription: Review code\r\nmodel: provider/model\r\ntools: []\r\n---\r\n\r\nLiteral {{.Template}} body.\r\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	definition, err := parseAgentDefinition(path, agentDefinitionSourceUser, testAgentDefinitionConfig())
	require.NoError(t, err)
	require.Equal(t, "Literal {{.Template}} body.", definition.Instructions)
	require.Empty(t, definition.Tools)
}

func TestAgentDefinitionParsesConfiguredScript(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "scripts", "classify.py")
	require.NoError(t, os.MkdirAll(filepath.Dir(scriptPath), 0o755))
	require.NoError(t, os.WriteFile(scriptPath, []byte("print('ok')\n"), 0o600))
	defaultFormat := "json"
	content := `---
name: classifier
description: Classifies input
model: provider/model
tools: [script]
script:
  path: ./scripts/classify.py
  timeout: 45s
  variables:
    input:
      required: true
    format:
      flag: --output-format
      default: json
      values: [json, text]
    mode:
      value: quick
---
Run the configured script.`
	path := filepath.Join(dir, "classifier.md")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	definition, err := parseAgentDefinition(path, agentDefinitionSourceProject, testAgentDefinitionConfig())
	require.NoError(t, err)
	require.Equal(t, scriptPath, definition.Script.Path)
	require.Equal(t, 45*time.Second, definition.Script.Timeout)
	require.Equal(t, "--input", definition.Script.Variables["input"].Flag)
	require.True(t, definition.Script.Variables["input"].Required)
	require.Equal(t, &defaultFormat, definition.Script.Variables["format"].Default)
	require.Equal(t, "--output-format", definition.Script.Variables["format"].Flag)
	require.Equal(t, []string{"json", "text"}, definition.Script.Variables["format"].Values)
	require.Equal(t, "quick", *definition.Script.Variables["mode"].Value)
}

func TestAgentDefinitionRejectsInvalidScriptConfiguration(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "script.py")
	require.NoError(t, os.WriteFile(scriptPath, []byte("print('ok')\n"), 0o600))
	tests := []struct {
		name      string
		tools     string
		script    string
		errorText string
	}{
		{name: "tool without config", tools: "[script]", errorText: "requires a script configuration"},
		{name: "config without tool", tools: "[]", script: "script:\n  path: script.py\n", errorText: "requires tool"},
		{name: "missing path", tools: "[script]", script: "script:\n  timeout: 1m\n", errorText: "script path is required"},
		{name: "invalid extension", tools: "[script]", script: "script:\n  path: definition.md\n", errorText: "must reference a .py file"},
		{name: "invalid timeout", tools: "[script]", script: "script:\n  path: script.py\n  timeout: forever\n", errorText: "invalid script timeout"},
		{name: "excessive timeout", tools: "[script]", script: "script:\n  path: script.py\n  timeout: 11m\n", errorText: "at most 10m"},
		{name: "invalid variable", tools: "[script]", script: "script:\n  path: script.py\n  variables:\n    INPUT:\n      required: true\n", errorText: "variable name"},
		{name: "default and value", tools: "[script]", script: "script:\n  path: script.py\n  variables:\n    mode:\n      default: slow\n      value: fast\n", errorText: "both default and value"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content := "---\nname: runner\ndescription: Runs script\nmodel: provider/model\ntools: " + test.tools + "\n" + test.script + "---\nInstructions"
			path := filepath.Join(dir, "definition.md")
			_, err := parseAgentDefinitionContent(path, agentDefinitionSourceProject, testAgentDefinitionConfig(), content)
			require.ErrorContains(t, err, test.errorText)
		})
	}
}

func TestCreateAgentDefinition(t *testing.T) {
	workingDir := t.TempDir()
	path, err := CreateAgentDefinition(workingDir, testAgentDefinitionConfig(), AgentDefinitionTemplate{
		Scope:       AgentDefinitionScopeProject,
		Name:        "reviewer",
		Description: "Review code",
		Model:       "provider/model",
		Tools:       []string{"view", "grep"},
	})
	require.NoError(t, err)
	require.Equal(t, filepath.Join(workingDir, ".ai-cli", "agents", "reviewer.md"), path)

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(content), "name: reviewer")
	require.Contains(t, string(content), "description: Review code")
	require.Contains(t, string(content), "model: provider/model")
	require.Contains(t, string(content), "tools:\n    - view\n    - grep")
	require.Contains(t, string(content), "# Instructions")

	definition, err := parseAgentDefinition(path, agentDefinitionSourceProject, testAgentDefinitionConfig())
	require.NoError(t, err)
	require.Equal(t, "reviewer", definition.Name)
	require.Equal(t, []string{"view", "grep"}, definition.Tools)
}

func TestCreateAgentDefinitionWithScript(t *testing.T) {
	workingDir := t.TempDir()
	agentDir := filepath.Join(workingDir, ".ai-cli", "agents")
	scriptPath := filepath.Join(agentDir, "scripts", "classify.py")
	require.NoError(t, os.MkdirAll(filepath.Dir(scriptPath), 0o755))
	require.NoError(t, os.WriteFile(scriptPath, []byte("print('ok')\n"), 0o600))
	defaultFormat := "json"
	path, err := CreateAgentDefinition(workingDir, testAgentDefinitionConfig(), AgentDefinitionTemplate{
		Scope:       AgentDefinitionScopeProject,
		Name:        "classifier",
		Description: "Classifies input",
		Model:       "provider/model",
		Tools:       []string{"script"},
		Script: &AgentDefinitionScriptTemplate{
			Path:    "./scripts/classify.py",
			Timeout: "30s",
			Variables: map[string]AgentDefinitionScriptVariableTemplate{
				"input":  {Required: true},
				"format": {Default: &defaultFormat, Values: []string{"json", "text"}},
			},
		},
	})
	require.NoError(t, err)
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(content), "script:\n")
	require.Contains(t, string(content), "path: ./scripts/classify.py")
	require.Contains(t, string(content), "timeout: 30s")
	require.Contains(t, string(content), "required: true")

	definition, err := parseAgentDefinition(path, agentDefinitionSourceProject, testAgentDefinitionConfig())
	require.NoError(t, err)
	require.Equal(t, scriptPath, definition.Script.Path)
}

func TestCreateAgentDefinitionUserScopeAndRejectsOverwrite(t *testing.T) {
	userDir := filepath.Join(t.TempDir(), "user")
	projectDir := filepath.Join(t.TempDir(), "project")
	template := AgentDefinitionTemplate{
		Scope:       AgentDefinitionScopeUser,
		Name:        "researcher",
		Description: "Research code",
		Model:       "provider/model",
		Tools:       []string{"*"},
	}
	path, err := createAgentDefinitionInDirs(userDir, projectDir, testAgentDefinitionConfig(), template)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(userDir, "researcher.md"), path)

	_, err = createAgentDefinitionInDirs(userDir, projectDir, testAgentDefinitionConfig(), template)
	require.ErrorIs(t, err, ErrAgentDefinitionExists)
	require.ErrorContains(t, err, "already exists")
}

func TestCreateAgentDefinitionRejectsInvalidInput(t *testing.T) {
	base := AgentDefinitionTemplate{
		Scope:       AgentDefinitionScopeProject,
		Name:        "reviewer",
		Description: "Review code",
		Model:       "provider/model",
		Tools:       []string{},
	}
	tests := []struct {
		name      string
		update    func(*AgentDefinitionTemplate)
		errorText string
	}{
		{name: "scope", update: func(template *AgentDefinitionTemplate) { template.Scope = "workspace" }, errorText: "invalid scope"},
		{name: "name", update: func(template *AgentDefinitionTemplate) { template.Name = "Reviewer" }, errorText: "must match"},
		{name: "description", update: func(template *AgentDefinitionTemplate) { template.Description = "" }, errorText: "description is required"},
		{name: "model", update: func(template *AgentDefinitionTemplate) { template.Model = "provider/missing" }, errorText: "is not configured"},
		{name: "tools", update: func(template *AgentDefinitionTemplate) { template.Tools = []string{"agent"} }, errorText: "is not available"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			template := base
			test.update(&template)
			_, err := CreateAgentDefinition(t.TempDir(), testAgentDefinitionConfig(), template)
			require.ErrorIs(t, err, ErrInvalidAgentDefinition)
			require.ErrorContains(t, err, test.errorText)
		})
	}
}

func TestAgentDefinitionParserRejectsInvalidDefinitions(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		errorText string
		configure func(*config.Config)
	}{
		{name: "no frontmatter", content: "body", errorText: "no YAML frontmatter"},
		{name: "malformed yaml", content: "---\nname: [\n---\nbody", errorText: "frontmatter"},
		{name: "unknown field", content: "---\nname: reviewer\ndescription: Review\nmodel: provider/model\ntoolz: []\n---\nbody", errorText: "field toolz not found"},
		{name: "missing name", content: "---\ndescription: Review\nmodel: provider/model\n---\nbody", errorText: "name is required"},
		{name: "invalid name", content: "---\nname: Reviewer\ndescription: Review\nmodel: provider/model\n---\nbody", errorText: "must match"},
		{name: "reserved name", content: "---\nname: task\ndescription: Review\nmodel: provider/model\n---\nbody", errorText: "is reserved"},
		{name: "missing description", content: "---\nname: reviewer\nmodel: provider/model\n---\nbody", errorText: "description is required"},
		{name: "missing model", content: "---\nname: reviewer\ndescription: Review\n---\nbody", errorText: "model is required"},
		{name: "missing body", content: "---\nname: reviewer\ndescription: Review\nmodel: provider/model\n---\n", errorText: "instruction body is required"},
		{name: "invalid model syntax", content: "---\nname: reviewer\ndescription: Review\nmodel: model\n---\nbody", errorText: "provider/model-id format"},
		{name: "missing provider", content: "---\nname: reviewer\ndescription: Review\nmodel: absent/model\n---\nbody", errorText: "provider \"absent\" is not configured"},
		{name: "disabled provider", content: "---\nname: reviewer\ndescription: Review\nmodel: disabled/model\n---\nbody", errorText: "provider \"disabled\" is disabled"},
		{name: "missing exact model", content: "---\nname: reviewer\ndescription: Review\nmodel: provider/absent\n---\nbody", errorText: "model \"absent\" is not configured"},
		{name: "unknown tool", content: "---\nname: reviewer\ndescription: Review\nmodel: provider/model\ntools: [missing]\n---\nbody", errorText: "unknown tool \"missing\""},
		{name: "forbidden recursive tool", content: "---\nname: reviewer\ndescription: Review\nmodel: provider/model\ntools: [agent]\n---\nbody", errorText: "tool \"agent\" is not available"},
		{name: "wildcard with named tool", content: "---\nname: reviewer\ndescription: Review\nmodel: provider/model\ntools: [\"*\", view]\n---\nbody", errorText: "wildcard tool"},
		{name: "disabled tool", content: "---\nname: reviewer\ndescription: Review\nmodel: provider/model\ntools: [view]\n---\nbody", errorText: "tool \"view\" is globally disabled", configure: func(cfg *config.Config) { cfg.Options.DisabledTools = []string{"view"} }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := testAgentDefinitionConfig()
			if test.configure != nil {
				test.configure(cfg)
			}
			path := filepath.Join(t.TempDir(), "definition.md")
			require.NoError(t, os.WriteFile(path, []byte(test.content), 0o600))
			_, err := parseAgentDefinition(path, agentDefinitionSourceUser, cfg)
			require.ErrorContains(t, err, path)
			require.ErrorContains(t, err, test.errorText)
		})
	}
}
