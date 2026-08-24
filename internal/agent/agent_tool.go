package agent

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"slices"
	"strings"

	fantasy "github.com/example-git/crux/foundation"

	"github.com/example-git/crux/internal/agent/prompt"
	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/config"
	managedtask "github.com/example-git/crux/internal/task"
)

//go:embed templates/agent_tool.md
var agentToolDescription string

type AgentParams struct {
	Prompt          string `json:"prompt" description:"The task for the agent to perform"`
	SubagentType    string `json:"subagent_type,omitempty" description:"Optional preset subagent type. Omit this to use the generic task agent."`
	RunInBackground bool   `json:"run_in_background,omitempty" description:"Set to true to run this agent in the background. Multiple background agents may run concurrently, and you will be notified when each completes."`
	ContinueTaskID  string `json:"continue_task_id,omitempty" description:"Continue a terminal background-agent transcript by task ID."`
}

type AgentResponseMetadata struct {
	Background     bool   `json:"background"`
	TaskID         string `json:"task_id,omitempty"`
	ChildSessionID string `json:"child_session_id,omitempty"`
}

const (
	AgentToolName = "agent"
)

type presetSubagent struct {
	agent SessionAgent
	title string
	err   error
}

func (c *coordinator) agentTool(ctx context.Context) (fantasy.AgentTool, error) {
	cfg := c.cfg.Config()
	taskCfg, ok := cfg.Agents[config.AgentTask]
	if !ok {
		return nil, errors.New("task agent not configured")
	}
	taskPromptTemplate, err := taskPrompt(prompt.WithWorkingDir(c.cfg.WorkingDir()))
	if err != nil {
		return nil, err
	}
	taskAgent, err := c.buildAgent(ctx, taskPromptTemplate, taskCfg, true)
	if err != nil {
		return nil, err
	}

	presets := map[string]presetSubagent{
		config.AgentTask: {agent: taskAgent, title: "New Agent Session"},
	}
	var definitions []agentDefinition
	if c.loadAgentDefinitions != nil {
		definitions, err = c.loadAgentDefinitions(c.cfg.WorkingDir(), cfg)
		if err != nil {
			return nil, err
		}
	}

	descriptionLines := []string{
		fmt.Sprintf("- %s: %s (model: configured large model; tools: %s)", config.AgentTask, taskCfg.Description, strings.Join(taskCfg.AllowedTools, ", ")),
	}
	for _, definition := range definitions {
		if definition.ValidationErr != nil {
			presets[definition.Name] = presetSubagent{
				title: "Agent: " + definition.Name,
				err:   definition.ValidationErr,
			}
			descriptionLines = append(descriptionLines, fmt.Sprintf("- %s: invalid definition at %s", definition.Name, definition.Path))
			continue
		}
		allowedTools := slices.Clone(definition.Tools)
		if definition.AllTools {
			allowedTools = c.customWildcardTools()
			if definition.Script == nil {
				allowedTools = slices.DeleteFunc(allowedTools, func(name string) bool {
					return name == tools.ScriptToolName
				})
			}
		}
		model := definition.Model
		agentCfg := config.Agent{
			ID:                   definition.Name,
			Name:                 definition.Name,
			Description:          definition.Description,
			Model:                config.SelectedModelTypeLarge,
			PrimaryModelOverride: &model,
			Instructions:         definition.Instructions,
			DefinitionPath:       definition.Path,
			AllowAllTools:        definition.AllTools,
			Script:               definition.Script,
			AllowedTools:         allowedTools,
			AllowedMCP:           map[string][]string{},
		}
		customPromptTemplate, err := customAgentPrompt(definition.Instructions, prompt.WithWorkingDir(c.cfg.WorkingDir()))
		if err != nil {
			presets[definition.Name] = presetSubagent{
				title: "Agent: " + definition.Name,
				err:   fmt.Errorf("building custom agent prompt for %q: %w", definition.Path, err),
			}
			descriptionLines = append(descriptionLines, fmt.Sprintf("- %s: invalid definition at %s", definition.Name, definition.Path))
			continue
		}
		customAgent, err := c.buildAgent(ctx, customPromptTemplate, agentCfg, true)
		if err != nil {
			presets[definition.Name] = presetSubagent{
				title: "Agent: " + definition.Name,
				err:   fmt.Errorf("building custom agent from %q: %w", definition.Path, err),
			}
			descriptionLines = append(descriptionLines, fmt.Sprintf("- %s: invalid definition at %s", definition.Name, definition.Path))
			continue
		}
		presets[definition.Name] = presetSubagent{
			agent: customAgent,
			title: "Agent: " + definition.Name,
		}
		toolSummary := "none"
		if len(allowedTools) > 0 {
			toolSummary = strings.Join(allowedTools, ", ")
		}
		descriptionLines = append(descriptionLines, fmt.Sprintf(
			"- %s: %s (model: %s/%s; tools: %s)",
			definition.Name,
			definition.Description,
			definition.Model.Provider,
			definition.Model.Model,
			toolSummary,
		))
	}

	availableTypes := make([]string, 0, len(presets))
	for name := range presets {
		availableTypes = append(availableTypes, name)
	}
	slices.Sort(availableTypes)
	description := strings.TrimSpace(agentToolDescription) + "\n\nAvailable subagent types:\n" + strings.Join(descriptionLines, "\n")

	return fantasy.NewParallelAgentTool(
		AgentToolName,
		description,
		func(ctx context.Context, params AgentParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Prompt == "" {
				return fantasy.NewTextErrorResponse("prompt is required"), nil
			}

			sessionID := tools.GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, errors.New("session id missing from context")
			}
			agentMessageID := tools.GetMessageFromContext(ctx)
			if agentMessageID == "" {
				return fantasy.ToolResponse{}, errors.New("agent message id missing from context")
			}

			var continuation *BackgroundAgentTask
			effectiveAgentType := params.SubagentType
			if params.ContinueTaskID != "" {
				if !params.RunInBackground {
					return fantasy.NewTextErrorResponse("continue_task_id requires run_in_background=true"), nil
				}
				continuation, effectiveAgentType, err = c.resolveBackgroundAgentContinuation(ctx, params.ContinueTaskID, effectiveAgentType)
				if err != nil {
					return fantasy.NewTextErrorResponse(err.Error()), nil
				}
			}

			selected, err := selectPresetSubagent(presets, effectiveAgentType, availableTypes)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			params.SubagentType = effectiveAgentType
			runParams := subAgentParams{
				Agent:          selected.agent,
				SessionID:      sessionID,
				AgentMessageID: agentMessageID,
				ToolCallID:     call.ID,
				Prompt:         params.Prompt,
				SessionTitle:   selected.title,
			}
			if continuation != nil {
				return c.continueBackgroundSubAgent(ctx, params, call, selected, runParams, continuation)
			}
			if !params.RunInBackground {
				return c.runSubAgent(ctx, runParams)
			}
			return c.startBackgroundSubAgent(ctx, params, call, selected, runParams)
		},
	), nil
}

func (c *coordinator) buildContinuationAgent(ctx context.Context, agentType string) (presetSubagent, error) {
	cfg := c.cfg.Config()
	if agentType == config.AgentTask {
		taskCfg, ok := cfg.Agents[config.AgentTask]
		if !ok {
			return presetSubagent{}, errors.New("task agent not configured")
		}
		template, err := taskPrompt(prompt.WithWorkingDir(c.cfg.WorkingDir()))
		if err != nil {
			return presetSubagent{}, err
		}
		built, err := c.buildAgent(ctx, template, taskCfg, true)
		if err != nil {
			return presetSubagent{}, err
		}
		return presetSubagent{agent: built, title: "New Agent Session"}, nil
	}
	if c.loadAgentDefinitions == nil {
		return presetSubagent{}, fmt.Errorf("unknown subagent_type %q", agentType)
	}
	definitions, err := c.loadAgentDefinitions(c.cfg.WorkingDir(), cfg)
	if err != nil {
		return presetSubagent{}, err
	}
	for _, definition := range definitions {
		if definition.Name != agentType {
			continue
		}
		if definition.ValidationErr != nil {
			return presetSubagent{}, definition.ValidationErr
		}
		allowedTools := slices.Clone(definition.Tools)
		if definition.AllTools {
			allowedTools = c.customWildcardTools()
			if definition.Script == nil {
				allowedTools = slices.DeleteFunc(allowedTools, func(name string) bool {
					return name == tools.ScriptToolName
				})
			}
		}
		model := definition.Model
		agentCfg := config.Agent{
			ID:                   definition.Name,
			Name:                 definition.Name,
			Description:          definition.Description,
			Model:                config.SelectedModelTypeLarge,
			PrimaryModelOverride: &model,
			Instructions:         definition.Instructions,
			DefinitionPath:       definition.Path,
			AllowAllTools:        definition.AllTools,
			Script:               definition.Script,
			AllowedTools:         allowedTools,
			AllowedMCP:           map[string][]string{},
		}
		template, err := customAgentPrompt(definition.Instructions, prompt.WithWorkingDir(c.cfg.WorkingDir()))
		if err != nil {
			return presetSubagent{}, fmt.Errorf("building custom agent prompt for %q: %w", definition.Path, err)
		}
		built, err := c.buildAgent(ctx, template, agentCfg, true)
		if err != nil {
			return presetSubagent{}, fmt.Errorf("building custom agent from %q: %w", definition.Path, err)
		}
		return presetSubagent{agent: built, title: "Agent: " + definition.Name}, nil
	}
	return presetSubagent{}, fmt.Errorf("unknown subagent_type %q", agentType)
}

func (c *coordinator) ContinueTask(ctx context.Context, taskID, parentSessionID, continuationPrompt, originToolCallID string) (managedtask.View, error) {
	if continuationPrompt == "" {
		return managedtask.View{}, errors.New("prompt is required")
	}
	if parentSessionID == "" {
		return managedtask.View{}, errors.New("parent session ID is required")
	}
	continuation, agentType, err := c.resolveBackgroundAgentContinuation(ctx, taskID, "")
	if err != nil {
		return managedtask.View{}, err
	}
	info := continuation.Info()
	if info.Ownership.ParentSessionID != parentSessionID {
		return managedtask.View{}, fmt.Errorf("background agent task does not belong to parent session %s", parentSessionID)
	}
	selected, err := c.buildContinuationAgent(ctx, agentType)
	if err != nil {
		return managedtask.View{}, err
	}
	backgroundTask, err := c.startBackgroundAgentContinuation(continuationPrompt, originToolCallID, selected, subAgentParams{
		Agent:          selected.agent,
		SessionID:      parentSessionID,
		ToolCallID:     originToolCallID,
		Prompt:         continuationPrompt,
		SessionTitle:   selected.title,
		ChildSessionID: info.ChildSessionID,
	}, continuation)
	if err != nil {
		return managedtask.View{}, err
	}
	return managedAgentInfo(backgroundTask.Info()), nil
}

func (c *coordinator) resolveBackgroundAgentContinuation(ctx context.Context, taskID, requestedAgentType string) (*BackgroundAgentTask, string, error) {
	if c.backgroundAgents == nil {
		return nil, "", fmt.Errorf("background agent task not found: %s", taskID)
	}
	continuation, err := c.backgroundAgents.task(taskID)
	if err != nil {
		return nil, "", err
	}
	info := continuation.Info()
	if !info.State.Status.Terminal() {
		return nil, "", fmt.Errorf("background agent task is not terminal: %s", taskID)
	}
	if info.ChildSessionID == "" {
		return nil, "", fmt.Errorf("background agent transcript is unavailable: %s", taskID)
	}
	child, err := c.sessions.Get(ctx, info.ChildSessionID)
	if err != nil || child.ParentSessionID != info.Ownership.ParentSessionID {
		return nil, "", fmt.Errorf("background agent transcript is unavailable: %s", taskID)
	}
	if requestedAgentType != "" && requestedAgentType != info.AgentType {
		return nil, "", fmt.Errorf("subagent_type %q does not match continuation agent type %q", requestedAgentType, info.AgentType)
	}
	return continuation, info.AgentType, nil
}

func (c *coordinator) startBackgroundAgentContinuation(continuationPrompt, originToolCallID string, selected presetSubagent, runParams subAgentParams, continuation *BackgroundAgentTask) (*BackgroundAgentTask, error) {
	info := continuation.Info()
	usageBaseline := c.backgroundAgentUsage(info.ChildSessionID)
	backgroundTask, err := c.backgroundAgents.ReserveContinuation(continuationPrompt, info.AgentType, selected.title, info.ID, usageBaseline, managedtask.Ownership{
		ParentSessionID:  runParams.SessionID,
		OriginToolCallID: originToolCallID,
	})
	if err != nil {
		return nil, err
	}
	runParams.ChildSessionID = info.ChildSessionID
	if err := c.backgroundAgents.Start(backgroundTask, info.ChildSessionID, func(runCtx context.Context) backgroundAgentResult {
		response, runErr := c.runSubAgent(runCtx, runParams)
		result := backgroundAgentResult{Err: runErr}
		if runErr == nil {
			if response.IsError {
				result.Err = errors.New(response.Content)
			} else {
				result.Output = response.Content
			}
		}
		result.Usage = agentUsageSince(c.backgroundAgentUsage(info.ChildSessionID), usageBaseline)
		return result
	}); err != nil {
		return nil, fmt.Errorf("continuing background agent: %w", err)
	}
	return backgroundTask, nil
}

func (c *coordinator) continueBackgroundSubAgent(_ context.Context, params AgentParams, call fantasy.ToolCall, selected presetSubagent, runParams subAgentParams, continuation *BackgroundAgentTask) (fantasy.ToolResponse, error) {
	backgroundTask, err := c.startBackgroundAgentContinuation(params.Prompt, call.ID, selected, runParams, continuation)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	metadata := AgentResponseMetadata{
		Background:     true,
		TaskID:         backgroundTask.ID,
		ChildSessionID: continuation.Info().ChildSessionID,
	}
	response := fmt.Sprintf("Background agent continued with ID: %s\n\nChild session ID: %s", backgroundTask.ID, metadata.ChildSessionID)
	return fantasy.WithResponseMetadata(fantasy.NewTextResponse(response), metadata), nil
}

func (c *coordinator) startBackgroundSubAgent(ctx context.Context, params AgentParams, call fantasy.ToolCall, selected presetSubagent, runParams subAgentParams) (fantasy.ToolResponse, error) {
	if c.backgroundAgents == nil {
		c.backgroundAgents = NewBackgroundAgentManager(c.cfg.WorkingDir(), c.backgroundShells)
	}
	agentType := params.SubagentType
	if agentType == "" {
		agentType = config.AgentTask
	}
	backgroundTask, err := c.backgroundAgents.Reserve(params.Prompt, agentType, selected.title, managedtask.Ownership{
		ParentSessionID:  runParams.SessionID,
		OriginToolCallID: call.ID,
	})
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	childSessionID := c.sessions.CreateAgentToolSessionID(runParams.AgentMessageID, runParams.ToolCallID)
	childSession, err := c.sessions.CreateTaskSession(ctx, childSessionID, runParams.SessionID, runParams.SessionTitle)
	if err != nil {
		c.backgroundAgents.FailReservation(backgroundTask, fmt.Errorf("create session: %w", err))
		return fantasy.ToolResponse{}, fmt.Errorf("create session: %w", err)
	}
	runParams.ChildSessionID = childSession.ID
	if err := c.backgroundAgents.Start(backgroundTask, childSession.ID, func(runCtx context.Context) backgroundAgentResult {
		response, runErr := c.runSubAgent(runCtx, runParams)
		result := backgroundAgentResult{Err: runErr}
		if runErr == nil {
			if response.IsError {
				result.Err = errors.New(response.Content)
			} else {
				result.Output = response.Content
			}
		}
		result.Usage = c.backgroundAgentUsage(childSession.ID)
		return result
	}); err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("starting background agent: %w", err)
	}
	metadata := AgentResponseMetadata{
		Background:     true,
		TaskID:         backgroundTask.ID,
		ChildSessionID: childSession.ID,
	}
	response := fmt.Sprintf("Background agent started with ID: %s\n\nChild session ID: %s", backgroundTask.ID, metadata.ChildSessionID)
	return fantasy.WithResponseMetadata(fantasy.NewTextResponse(response), metadata), nil
}

func (c *coordinator) backgroundAgentUsage(childSessionID string) managedtask.AgentUsage {
	usage := managedtask.AgentUsage{}
	if persisted, err := c.sessions.Get(context.Background(), childSessionID); err == nil {
		usage.PromptTokens = persisted.PromptTokens
		usage.CompletionTokens = persisted.CompletionTokens
		usage.Cost = persisted.Cost
	}
	if messages, err := c.messages.List(context.Background(), childSessionID); err == nil {
		for _, msg := range messages {
			usage.ToolUseCount += len(msg.ToolCalls())
		}
	}
	return usage
}

func agentUsageSince(usage, baseline managedtask.AgentUsage) managedtask.AgentUsage {
	return managedtask.AgentUsage{
		PromptTokens:     max(usage.PromptTokens-baseline.PromptTokens, 0),
		CompletionTokens: max(usage.CompletionTokens-baseline.CompletionTokens, 0),
		Cost:             max(usage.Cost-baseline.Cost, 0),
		ToolUseCount:     max(usage.ToolUseCount-baseline.ToolUseCount, 0),
	}
}

func selectPresetSubagent(presets map[string]presetSubagent, requested string, availableTypes []string) (presetSubagent, error) {
	selectedType := requested
	if selectedType == "" {
		selectedType = config.AgentTask
	}
	selected, ok := presets[selectedType]
	if !ok {
		return presetSubagent{}, fmt.Errorf(
			"unknown subagent type %q; available types: %s",
			requested,
			strings.Join(availableTypes, ", "),
		)
	}
	if selected.err != nil {
		return presetSubagent{}, selected.err
	}
	return selected, nil
}

func (c *coordinator) customWildcardTools() []string {
	coderCfg, ok := c.cfg.Config().Agents[config.AgentCoder]
	if !ok {
		return nil
	}
	allowed := make([]string, 0, len(coderCfg.AllowedTools))
	for _, name := range coderCfg.AllowedTools {
		if _, forbidden := forbiddenCustomAgentTools[name]; forbidden {
			continue
		}
		if name == "codebase_search" && !c.cfg.Config().Tools.CodebaseSearch.IsEnabled() {
			continue
		}
		if strings.HasPrefix(name, "lsp_") && len(c.cfg.Config().LSP) == 0 && c.cfg.Config().Options.AutoLSP != nil && !*c.cfg.Config().Options.AutoLSP {
			continue
		}
		allowed = append(allowed, name)
	}
	slices.Sort(allowed)
	return allowed
}
