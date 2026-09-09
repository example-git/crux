package mcp

import (
	"context"
	"iter"
	"log/slog"

	"github.com/example-git/crux/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Prompt = mcp.Prompt

// Prompts returns all available MCP prompts.
func (runtime *Manager) Prompts() iter.Seq2[string, []*Prompt] {
	if runtime.ctx.Err() != nil {
		return func(func(string, []*Prompt) bool) {}
	}
	return runtime.allPrompts.Seq2()
}

// GetPromptMessages retrieves the content of an MCP prompt with the given arguments.
func (runtime *Manager) GetPromptMessages(ctx context.Context, cfg *config.ConfigStore, clientName, promptName string, args map[string]string) ([]string, error) {
	if err := runtime.requireStore(cfg); err != nil {
		return nil, err
	}
	ctx, done, admissionErr := runtime.admit(ctx)
	if admissionErr != nil {
		return nil, admissionErr
	}
	defer done()
	c, err := runtime.getOrRenewClient(ctx, cfg, clientName)
	if err != nil {
		return nil, err
	}
	result, err := c.GetPrompt(ctx, &mcp.GetPromptParams{
		Name:      promptName,
		Arguments: args,
	})
	if err != nil {
		return nil, err
	}

	var messages []string
	for _, msg := range result.Messages {
		if msg.Role != "user" {
			continue
		}
		if textContent, ok := msg.Content.(*mcp.TextContent); ok {
			messages = append(messages, textContent.Text)
		}
	}
	return messages, nil
}

// RefreshPrompts gets the updated list of prompts from the MCP and updates the
// workspace state.
func (runtime *Manager) RefreshPrompts(ctx context.Context, name string) {
	ctx, done, admissionErr := runtime.serverOperation(ctx, name)
	if admissionErr != nil {
		return
	}
	defer done()
	session, ok := runtime.sessions.Get(name)
	if !ok {
		slog.Warn("Refresh prompts: no session", "name", name)
		return
	}

	prompts, err := getPrompts(ctx, session)
	if err != nil {
		runtime.updateState(name, StateError, err, nil, Counts{})
		return
	}

	runtime.updatePrompts(name, prompts)

	prev, _ := runtime.states.Get(name)
	prev.Counts.Prompts = len(prompts)
	runtime.updateState(name, StateConnected, nil, session, prev.Counts)
}

func getPrompts(ctx context.Context, c *ClientSession) ([]*Prompt, error) {
	if c.InitializeResult().Capabilities.Prompts == nil {
		return nil, nil
	}
	result, err := c.ListPrompts(ctx, &mcp.ListPromptsParams{})
	if err != nil {
		return nil, err
	}
	return result.Prompts, nil
}

// updatePrompts updates this workspace's prompt cache.
func (runtime *Manager) updatePrompts(mcpName string, prompts []*Prompt) {
	if len(prompts) == 0 {
		runtime.allPrompts.Del(mcpName)
		return
	}
	runtime.allPrompts.Set(mcpName, prompts)
}
