package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/permission"
	"github.com/example-git/crux/internal/subagentscript"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/google/uuid"
)

const SubagentScriptToolName = "subagent_script"

type subagentScriptService struct {
	coordinator     *coordinator
	sessionID       string
	parentSessionID string
	isSubagent      bool
}

func (c *coordinator) scriptService(ctx context.Context) (*subagentScriptService, error) {
	id := tools.GetSessionFromContext(ctx)
	if id == "" {
		return nil, errors.New("subagent scripting requires the invoking session")
	}
	current, err := c.sessions.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return &subagentScriptService{
		coordinator: c, sessionID: id, parentSessionID: current.ParentSessionID,
		isSubagent: permission.IsSubagent(ctx) || current.ParentSessionID != "",
	}, nil
}

func (s *subagentScriptService) owns(info BackgroundAgentInfo) bool {
	if s.isSubagent {
		return info.ChildSessionID == s.sessionID && info.Ownership.ParentSessionID == s.parentSessionID
	}
	return info.Ownership.ParentSessionID == s.sessionID
}

func (s *subagentScriptService) Call(ctx context.Context, request subagentscript.Request) (any, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	c := s.coordinator
	current, err := c.sessions.Get(ctx, s.sessionID)
	if err != nil {
		return nil, err
	}
	if current.ParentSessionID != s.parentSessionID {
		return nil, errors.New("invoking session ownership changed")
	}
	ctx = context.WithValue(ctx, tools.SessionIDContextKey, s.sessionID)
	if request.Action == "list" {
		result := []managedtask.View{}
		if c.backgroundAgents != nil {
			for _, info := range c.backgroundAgents.List() {
				if s.owns(info) {
					result = append(result, managedAgentInfo(info))
				}
			}
		}
		return result, nil
	}
	if request.TaskID != "" {
		if c.backgroundAgents == nil {
			return nil, errors.New("subagent task is not owned by the invoking session")
		}
		task, ok := c.backgroundAgents.Get(request.TaskID)
		if !ok || !s.owns(task.Info()) {
			return nil, errors.New("subagent task is not owned by the invoking session")
		}
	}
	switch request.Action {
	case "output":
		return c.TaskOutput(ctx, request.TaskID, false, 0)
	case "wait":
		for {
			timeout := 10 * time.Minute
			if request.TimeoutMS != nil {
				timeout = time.Duration(*request.TimeoutMS) * time.Millisecond
			}
			result, err := c.managedTaskOutput(ctx, request.TaskID, true, timeout)
			if err != nil || result.Task.State.Status.Terminal() || request.TimeoutMS != nil {
				return result, err
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
	case "stop":
		return c.StopTask(ctx, request.TaskID)
	}
	if s.isSubagent {
		return nil, permission.ErrSubagentLaunch
	}
	runtime, err := c.runtimeForSession(ctx, s.sessionID)
	if err != nil {
		return nil, err
	}
	ctx = context.WithValue(ctx, sessionRuntimeKey{}, runtime.Snapshot)
	if request.Action == "types" {
		result := []map[string]string{{"name": config.AgentTask, "description": "Generic task agent", "model": runtime.LargeModel.ModelCfg.Provider + "/" + runtime.LargeModel.ModelCfg.Model}}
		if c.loadAgentDefinitions != nil {
			definitions, err := c.loadAgentDefinitions(c.cfg.WorkingDir(), runtime.Snapshot.Config())
			if err != nil {
				return nil, err
			}
			for _, definition := range definitions {
				item := map[string]string{"name": definition.Name, "description": definition.Description, "model": definition.Model.Provider + "/" + definition.Model.Model}
				if definition.ValidationErr != nil {
					item["error"] = definition.ValidationErr.Error()
				}
				result = append(result, item)
			}
		}
		return result, nil
	}
	toolCallID := uuid.NewString()
	if request.Action == "continue" {
		return c.ContinueTask(ctx, request.TaskID, s.sessionID, request.Prompt, toolCallID)
	}
	agentType := request.SubagentType
	if agentType == "" {
		agentType = config.AgentTask
	}
	selected, err := c.buildContinuationAgent(ctx, agentType)
	if err != nil {
		return nil, err
	}
	task, err := c.startManagedSubAgent(ctx, agentType, selected, subAgentParams{
		Agent: selected.agent, SessionID: s.sessionID, AgentMessageID: uuid.NewString(),
		ToolCallID: toolCallID, Prompt: request.Prompt, SessionTitle: selected.title,
	})
	if err != nil {
		return nil, err
	}
	return managedAgentInfo(task.Info()), nil
}

type sessionEnvironmentTool struct {
	fantasy.AgentTool
	coordinator *coordinator
}

func (t sessionEnvironmentTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	service, err := t.coordinator.scriptService(ctx)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	c := t.coordinator
	c.scriptMu.Lock()
	if c.scriptClosed {
		c.scriptMu.Unlock()
		return fantasy.ToolResponse{}, errors.New("subagent scripting is closed")
	}
	if c.scriptBridges == nil {
		c.scriptBridges = make(map[string]*subagentscript.Bridge)
	}
	key := fmt.Sprintf("%s/%t", service.sessionID, service.isSubagent)
	bridge := c.scriptBridges[key]
	if bridge == nil {
		bridge, err = subagentscript.NewBridge(service, service.sessionID, service.parentSessionID)
		if err == nil {
			c.scriptBridges[key] = bridge
		}
	}
	c.scriptMu.Unlock()
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	return t.AgentTool.Run(subagentscript.WithEnvironment(ctx, bridge.Environment()), call)
}

func (c *coordinator) closeScriptBridges() {
	c.scriptMu.Lock()
	c.scriptClosed = true
	bridges := c.scriptBridges
	c.scriptBridges = nil
	c.scriptMu.Unlock()
	for _, bridge := range bridges {
		_ = bridge.Close()
	}
}

type subagentScriptParams struct {
	Code string `json:"code" description:"JavaScript orchestration code, with an optional return value"`
}

func (c *coordinator) subagentScriptTool() fantasy.AgentTool {
	return fantasy.NewAgentTool(SubagentScriptToolName,
		"Run JavaScript to coordinate subagents in the invoking session. Use subagents.types(), list(), start({prompt, subagent_type}), output(id), wait(id, {timeout_ms}), stop(id), continue(id, prompt). start returns immediately; start several tasks before waiting to run them concurrently. wait without options waits until terminal. Functions are synchronous; do not use async/await. Use print(value) to retain output and return a JSON value. env.CRUX_SESSION_ID and env.CRUX_PARENT_SESSION_ID identify the fixed session; no session override is accepted.",
		func(ctx context.Context, params subagentScriptParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			service, err := c.scriptService(ctx)
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			result, runErr := subagentscript.Run(ctx, service, service.sessionID, service.parentSessionID, params.Code)
			data, err := json.Marshal(result)
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if runErr != nil {
				return fantasy.NewTextErrorResponse(string(data) + "\n" + runErr.Error()), nil
			}
			return fantasy.NewTextResponse(string(data)), nil
		})
}
