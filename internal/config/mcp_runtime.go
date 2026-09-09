package config

import (
	"context"
	"fmt"
)

// MCPRuntime is the workspace-owned MCP lifetime. It is kept on the store,
// outside immutable configuration publications and all wire representations.
type MCPRuntime interface{ Close(context.Context) error }

type mcpRuntimeSlot struct {
	runtime MCPRuntime
}

// MCPRuntime returns the store's single MCP lifetime, constructing it once.
// A closed runtime remains installed: late callers cannot resurrect a workspace.
// The factory must not reenter this accessor.
func (s *ConfigStore) MCPRuntime(factory func() MCPRuntime) MCPRuntime {
	s.mcpRuntimeOnce.Do(func() { s.mcpRuntime.runtime = factory() })
	return s.mcpRuntime.runtime
}

func (mcpRuntimeSlot) Format(s fmt.State, verb rune) { fmt.Fprint(s, "[private MCP runtime]") }
func (mcpRuntimeSlot) MarshalJSON() ([]byte, error)  { return nil, fmt.Errorf("MCP runtime is private") }
