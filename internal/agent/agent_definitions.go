package agent

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/home"
	"gopkg.in/yaml.v3"
)

type agentDefinitionSource string

type AgentDefinitionScope string

const (
	agentDefinitionSourceUser    agentDefinitionSource = "user"
	agentDefinitionSourceProject agentDefinitionSource = "project"

	AgentDefinitionScopeUser    AgentDefinitionScope = "user"
	AgentDefinitionScopeProject AgentDefinitionScope = "project"
)

var (
	agentDefinitionNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	agentScriptFlagPattern     = regexp.MustCompile(`^--[a-zA-Z0-9][a-zA-Z0-9_-]*$`)
)

var (
	ErrInvalidAgentDefinition = errors.New("invalid agent definition")
	ErrAgentDefinitionExists  = errors.New("agent definition already exists")
)

var forbiddenCustomAgentTools = map[string]struct{}{
	AgentToolName:                  {},
	tools.AgenticFetchToolName:     {},
	tools.CompletePlanToolName:     {},
	tools.EnterPlanToolName:        {},
	tools.ExitPlanToolName:         {},
	tools.QuestionToolName:         {},
	tools.ListMCPResourcesToolName: {},
	tools.ReadMCPResourceToolName:  {},
}

type agentDefinitionScriptVariable struct {
	Flag     string   `yaml:"flag"`
	Required bool     `yaml:"required"`
	Default  *string  `yaml:"default"`
	Value    *string  `yaml:"value"`
	Values   []string `yaml:"values"`
}

type agentDefinitionScript struct {
	Path      string                                   `yaml:"path"`
	Timeout   string                                   `yaml:"timeout"`
	Variables map[string]agentDefinitionScriptVariable `yaml:"variables"`
}

type agentDefinitionFrontmatter struct {
	Name        string                 `yaml:"name"`
	Description string                 `yaml:"description"`
	Model       string                 `yaml:"model"`
	Tools       []string               `yaml:"tools"`
	Script      *agentDefinitionScript `yaml:"script"`
}

type agentDefinition struct {
	Name          string
	Description   string
	Model         config.SelectedModel
	Tools         []string
	AllTools      bool
	Script        *config.AgentScript
	Instructions  string
	Path          string
	Source        agentDefinitionSource
	ValidationErr error
}

type AgentDefinitionScriptVariableTemplate struct {
	Flag     string
	Required bool
	Default  *string
	Value    *string
	Values   []string
}

type AgentDefinitionScriptTemplate struct {
	Path      string
	Timeout   string
	Variables map[string]AgentDefinitionScriptVariableTemplate
}

type AgentDefinitionTemplate struct {
	Scope       AgentDefinitionScope
	Name        string
	Description string
	Model       string
	Tools       []string
	Script      *AgentDefinitionScriptTemplate
}

func discoverAgentDefinitions(workingDir string, cfg *config.Config) ([]agentDefinition, error) {
	return loadAgentDefinitionsFromDirs(
		filepath.Join(home.Dir(), ".ai-cli", "agents"),
		filepath.Join(workingDir, ".ai-cli", "agents"),
		cfg,
	)
}

func loadAgentDefinitionsFromDirs(userDir, projectDir string, cfg *config.Config) ([]agentDefinition, error) {
	userDefinitions, err := loadAgentDefinitionsDir(userDir, agentDefinitionSourceUser, cfg)
	if err != nil {
		return nil, err
	}
	projectDefinitions, err := loadAgentDefinitionsDir(projectDir, agentDefinitionSourceProject, cfg)
	if err != nil {
		return nil, err
	}

	definitions := make(map[string]agentDefinition, len(userDefinitions)+len(projectDefinitions))
	for _, definition := range userDefinitions {
		definitions[definition.Name] = definition
	}
	for _, definition := range projectDefinitions {
		definitions[definition.Name] = definition
	}

	result := make([]agentDefinition, 0, len(definitions))
	for _, definition := range definitions {
		result = append(result, definition)
	}
	slices.SortFunc(result, func(a, b agentDefinition) int {
		return strings.Compare(a.Name, b.Name)
	})
	return result, nil
}

func loadAgentDefinitionsDir(dir string, source agentDefinitionSource, cfg *config.Config) ([]agentDefinition, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s agent definitions directory %q: %w", source, dir, err)
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int {
		return strings.Compare(a.Name(), b.Name())
	})

	definitions := make([]agentDefinition, 0, len(entries))
	seen := make(map[string]int)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		content, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading agent definition %q: %w", path, err)
		}
		definition, err := parseAgentDefinitionContent(path, source, cfg, string(content))
		if err != nil {
			definition = invalidAgentDefinition(path, source, string(content), err)
		}
		if definition.Name == "" || definition.Name == config.AgentCoder || definition.Name == config.AgentTask {
			continue
		}
		if previous, ok := seen[definition.Name]; ok {
			previousPath := definitions[previous].Path
			definitions[previous].ValidationErr = fmt.Errorf("duplicate %s agent definition %q in %q and %q", source, definition.Name, previousPath, path)
			continue
		}
		seen[definition.Name] = len(definitions)
		definitions = append(definitions, definition)
	}
	return definitions, nil
}

func invalidAgentDefinition(path string, source agentDefinitionSource, content string, validationErr error) agentDefinition {
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if frontmatter, _, err := splitAgentDefinitionFrontmatter(content); err == nil {
		var metadata struct {
			Name string `yaml:"name"`
		}
		if yaml.Unmarshal([]byte(frontmatter), &metadata) == nil && strings.TrimSpace(metadata.Name) != "" {
			name = strings.TrimSpace(metadata.Name)
		}
	}
	return agentDefinition{
		Name:          name,
		Description:   "Invalid custom agent definition",
		Path:          path,
		Source:        source,
		ValidationErr: validationErr,
	}
}

func parseAgentDefinition(path string, source agentDefinitionSource, cfg *config.Config) (agentDefinition, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return agentDefinition{}, fmt.Errorf("reading agent definition %q: %w", path, err)
	}
	return parseAgentDefinitionContent(path, source, cfg, string(content))
}

func parseAgentDefinitionContent(path string, source agentDefinitionSource, cfg *config.Config, content string) (agentDefinition, error) {
	frontmatter, body, err := splitAgentDefinitionFrontmatter(content)
	if err != nil {
		return agentDefinition{}, fmt.Errorf("parsing agent definition %q: %w", path, err)
	}

	var metadata agentDefinitionFrontmatter
	decoder := yaml.NewDecoder(strings.NewReader(frontmatter))
	decoder.KnownFields(true)
	if err := decoder.Decode(&metadata); err != nil {
		return agentDefinition{}, fmt.Errorf("parsing agent definition %q frontmatter: %w", path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			err = errors.New("multiple YAML documents are not supported")
		}
		return agentDefinition{}, fmt.Errorf("parsing agent definition %q frontmatter: %w", path, err)
	}

	metadata.Name = strings.TrimSpace(metadata.Name)
	metadata.Description = strings.TrimSpace(metadata.Description)
	metadata.Model = strings.TrimSpace(metadata.Model)
	body = strings.TrimSpace(body)

	switch {
	case metadata.Name == "":
		return agentDefinition{}, fmt.Errorf("agent definition %q: name is required", path)
	case !agentDefinitionNamePattern.MatchString(metadata.Name):
		return agentDefinition{}, fmt.Errorf("agent definition %q: name %q must match %s", path, metadata.Name, agentDefinitionNamePattern.String())
	case metadata.Name == config.AgentCoder || metadata.Name == config.AgentTask:
		return agentDefinition{}, fmt.Errorf("agent definition %q: name %q is reserved", path, metadata.Name)
	case metadata.Description == "":
		return agentDefinition{}, fmt.Errorf("agent definition %q: description is required", path)
	case metadata.Model == "":
		return agentDefinition{}, fmt.Errorf("agent definition %q: model is required", path)
	case body == "":
		return agentDefinition{}, fmt.Errorf("agent definition %q: instruction body is required", path)
	}

	providerID, modelID, ok := strings.Cut(metadata.Model, "/")
	if !ok || providerID == "" || modelID == "" {
		return agentDefinition{}, fmt.Errorf("agent definition %q: model %q must use provider/model-id format", path, metadata.Model)
	}
	if cfg == nil || cfg.Providers == nil {
		return agentDefinition{}, fmt.Errorf("agent definition %q: provider %q is not configured", path, providerID)
	}
	provider, ok := cfg.Providers.Get(providerID)
	if !ok {
		return agentDefinition{}, fmt.Errorf("agent definition %q: provider %q is not configured", path, providerID)
	}
	if provider.Disable {
		return agentDefinition{}, fmt.Errorf("agent definition %q: provider %q is disabled", path, providerID)
	}
	if cfg.GetModel(providerID, modelID) == nil {
		return agentDefinition{}, fmt.Errorf("agent definition %q: model %q is not configured for provider %q", path, modelID, providerID)
	}

	tools := make([]string, 0, len(metadata.Tools))
	seenTools := make(map[string]bool, len(metadata.Tools))
	allTools := false
	for _, rawTool := range metadata.Tools {
		tool := strings.TrimSpace(rawTool)
		if tool == "" {
			return agentDefinition{}, fmt.Errorf("agent definition %q: tools cannot contain an empty name", path)
		}
		if seenTools[tool] {
			return agentDefinition{}, fmt.Errorf("agent definition %q: tool %q is listed more than once", path, tool)
		}
		seenTools[tool] = true
		if tool == "*" {
			allTools = true
		} else {
			if _, forbidden := forbiddenCustomAgentTools[tool]; forbidden {
				return agentDefinition{}, fmt.Errorf("agent definition %q: tool %q is not available to custom subagents", path, tool)
			}
			if _, known := config.ToolCapabilities(tool); !known {
				return agentDefinition{}, fmt.Errorf("agent definition %q: unknown tool %q", path, tool)
			}
			if cfg.Options != nil && slices.Contains(cfg.Options.DisabledTools, tool) {
				return agentDefinition{}, fmt.Errorf("agent definition %q: tool %q is globally disabled", path, tool)
			}
		}
		tools = append(tools, tool)
	}
	if allTools && len(tools) != 1 {
		return agentDefinition{}, fmt.Errorf("agent definition %q: wildcard tool %q must be the only tool", path, "*")
	}

	script, err := parseAgentDefinitionScript(path, metadata.Script, tools, allTools)
	if err != nil {
		return agentDefinition{}, err
	}

	return agentDefinition{
		Name:         metadata.Name,
		Description:  metadata.Description,
		Model:        config.SelectedModel{Provider: providerID, Model: modelID},
		Tools:        tools,
		AllTools:     allTools,
		Script:       script,
		Instructions: body,
		Path:         path,
		Source:       source,
	}, nil
}

func parseAgentDefinitionScript(path string, script *agentDefinitionScript, allowedTools []string, allTools bool) (*config.AgentScript, error) {
	scriptAllowed := slices.Contains(allowedTools, tools.ScriptToolName) || allTools && script != nil
	if script == nil {
		if scriptAllowed {
			return nil, fmt.Errorf("agent definition %q: tool %q requires a script configuration", path, tools.ScriptToolName)
		}
		return nil, nil
	}
	if !scriptAllowed {
		return nil, fmt.Errorf("agent definition %q: script configuration requires tool %q", path, tools.ScriptToolName)
	}

	scriptPath := strings.TrimSpace(script.Path)
	if scriptPath == "" {
		return nil, fmt.Errorf("agent definition %q: script path is required", path)
	}
	scriptPath = home.Long(scriptPath)
	if !filepath.IsAbs(scriptPath) {
		scriptPath = filepath.Join(filepath.Dir(path), scriptPath)
	}
	absolutePath, err := filepath.Abs(scriptPath)
	if err != nil {
		return nil, fmt.Errorf("agent definition %q: resolving script path: %w", path, err)
	}
	if filepath.Ext(absolutePath) != ".py" {
		return nil, fmt.Errorf("agent definition %q: script path %q must reference a .py file", path, absolutePath)
	}
	info, err := os.Stat(absolutePath)
	if err != nil {
		return nil, fmt.Errorf("agent definition %q: inspecting script path %q: %w", path, absolutePath, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("agent definition %q: script path %q is not a regular file", path, absolutePath)
	}

	timeout := 2 * time.Minute
	if strings.TrimSpace(script.Timeout) != "" {
		timeout, err = time.ParseDuration(strings.TrimSpace(script.Timeout))
		if err != nil {
			return nil, fmt.Errorf("agent definition %q: invalid script timeout %q: %w", path, script.Timeout, err)
		}
		if timeout <= 0 || timeout > 10*time.Minute {
			return nil, fmt.Errorf("agent definition %q: script timeout must be greater than zero and at most 10m", path)
		}
	}

	variables := make(map[string]config.AgentScriptVariable, len(script.Variables))
	for name, variable := range script.Variables {
		if !agentDefinitionNamePattern.MatchString(name) {
			return nil, fmt.Errorf("agent definition %q: script variable name %q must match %s", path, name, agentDefinitionNamePattern.String())
		}
		flag := strings.TrimSpace(variable.Flag)
		if flag == "" {
			flag = "--" + strings.ReplaceAll(name, "_", "-")
		}
		if !agentScriptFlagPattern.MatchString(flag) {
			return nil, fmt.Errorf("agent definition %q: script variable %q has invalid flag %q", path, name, flag)
		}
		if variable.Default != nil && variable.Value != nil {
			return nil, fmt.Errorf("agent definition %q: script variable %q cannot set both default and value", path, name)
		}
		values := slices.Clone(variable.Values)
		seenValues := make(map[string]struct{}, len(values))
		for _, value := range values {
			if _, exists := seenValues[value]; exists {
				return nil, fmt.Errorf("agent definition %q: script variable %q repeats allowed value %q", path, name, value)
			}
			seenValues[value] = struct{}{}
		}
		configuredValue := variable.Default
		if variable.Value != nil {
			configuredValue = variable.Value
		}
		if configuredValue != nil && len(values) > 0 && !slices.Contains(values, *configuredValue) {
			return nil, fmt.Errorf("agent definition %q: script variable %q configured value %q is not allowed", path, name, *configuredValue)
		}
		variables[name] = config.AgentScriptVariable{
			Flag:     flag,
			Required: variable.Required,
			Default:  variable.Default,
			Value:    variable.Value,
			Values:   values,
		}
	}

	return &config.AgentScript{Path: absolutePath, Timeout: timeout, Variables: variables}, nil
}

func CreateAgentDefinition(workingDir string, cfg *config.Config, template AgentDefinitionTemplate) (string, error) {
	if template.Scope == AgentDefinitionScopeProject && strings.TrimSpace(workingDir) == "" {
		return "", fmt.Errorf("%w: project scope requires a working directory", ErrInvalidAgentDefinition)
	}
	return createAgentDefinitionInDirs(
		filepath.Join(home.Dir(), ".ai-cli", "agents"),
		filepath.Join(workingDir, ".ai-cli", "agents"),
		cfg,
		template,
	)
}

func createAgentDefinitionInDirs(userDir, projectDir string, cfg *config.Config, template AgentDefinitionTemplate) (string, error) {
	var dir string
	var source agentDefinitionSource
	switch template.Scope {
	case AgentDefinitionScopeUser:
		dir = userDir
		source = agentDefinitionSourceUser
	case AgentDefinitionScopeProject:
		if strings.TrimSpace(projectDir) == "" {
			return "", fmt.Errorf("%w: project scope requires a definitions directory", ErrInvalidAgentDefinition)
		}
		dir = projectDir
		source = agentDefinitionSourceProject
	default:
		return "", fmt.Errorf("%w: invalid scope %q", ErrInvalidAgentDefinition, template.Scope)
	}

	metadata := agentDefinitionFrontmatter{
		Name:        strings.TrimSpace(template.Name),
		Description: strings.TrimSpace(template.Description),
		Model:       strings.TrimSpace(template.Model),
		Tools:       slices.Clone(template.Tools),
	}
	if template.Script != nil {
		metadata.Script = &agentDefinitionScript{
			Path:      strings.TrimSpace(template.Script.Path),
			Timeout:   strings.TrimSpace(template.Script.Timeout),
			Variables: make(map[string]agentDefinitionScriptVariable, len(template.Script.Variables)),
		}
		for name, variable := range template.Script.Variables {
			metadata.Script.Variables[name] = agentDefinitionScriptVariable{
				Flag:     variable.Flag,
				Required: variable.Required,
				Default:  variable.Default,
				Value:    variable.Value,
				Values:   slices.Clone(variable.Values),
			}
		}
	}
	frontmatter, err := yaml.Marshal(metadata)
	if err != nil {
		return "", fmt.Errorf("encoding agent definition: %w", err)
	}
	content := "---\n" + string(frontmatter) + "---\n\n# Instructions\n"
	path := filepath.Join(dir, metadata.Name+".md")
	definition, err := parseAgentDefinitionContent(path, source, cfg, content)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidAgentDefinition, err)
	}

	existing, err := loadAgentDefinitionsDir(dir, source, cfg)
	if err != nil {
		return "", err
	}
	if duplicate := slices.IndexFunc(existing, func(candidate agentDefinition) bool {
		return candidate.Name == definition.Name
	}); duplicate >= 0 {
		return "", fmt.Errorf("%w: %s definition %q at %q", ErrAgentDefinitionExists, source, definition.Name, existing[duplicate].Path)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating %s agent definitions directory %q: %w", source, dir, err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("%w: definition at %q", ErrAgentDefinitionExists, path)
	}
	if err != nil {
		return "", fmt.Errorf("creating agent definition %q: %w", path, err)
	}
	if _, err := io.WriteString(file, content); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("writing agent definition %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("closing agent definition %q: %w", path, err)
	}
	return path, nil
}

func resolveCustomAgentTools(agent config.Agent, available, disabled []string) ([]string, error) {
	availableSet := make(map[string]struct{}, len(available))
	for _, name := range available {
		if _, forbidden := forbiddenCustomAgentTools[name]; !forbidden {
			availableSet[name] = struct{}{}
		}
	}
	if agent.AllowAllTools {
		resolved := make([]string, 0, len(availableSet))
		for name := range availableSet {
			if !slices.Contains(disabled, name) {
				resolved = append(resolved, name)
			}
		}
		slices.Sort(resolved)
		return resolved, nil
	}
	for _, name := range agent.AllowedTools {
		if _, ok := availableSet[name]; !ok {
			return nil, fmt.Errorf("agent definition %q: tool %q is not available to subagents in this workspace", agent.DefinitionPath, name)
		}
	}
	return slices.Clone(agent.AllowedTools), nil
}

func splitAgentDefinitionFrontmatter(content string) (frontmatter, body string, err error) {
	content = strings.TrimPrefix(content, "\uFEFF")
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")

	lines := strings.Split(content, "\n")
	start := slices.IndexFunc(lines, func(line string) bool {
		return strings.TrimSpace(line) != ""
	})
	if start == -1 || strings.TrimSpace(lines[start]) != "---" {
		return "", "", errors.New("no YAML frontmatter found")
	}
	endOffset := slices.IndexFunc(lines[start+1:], func(line string) bool {
		return strings.TrimSpace(line) == "---"
	})
	if endOffset == -1 {
		return "", "", errors.New("unclosed frontmatter")
	}
	end := start + 1 + endOffset
	return strings.Join(lines[start+1:end], "\n"), strings.Join(lines[end+1:], "\n"), nil
}
