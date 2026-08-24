package agent

import (
	"context"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/hooks"
	"github.com/example-git/crux/internal/permission"
	"github.com/stretchr/testify/require"
)

// fakeTool records the context it was invoked with so tests can assert on
// values stamped onto it by the hookedTool decorator.
type fakeTool struct {
	name    string
	called  bool
	gotCtx  context.Context
	gotCall fantasy.ToolCall
	resp    fantasy.ToolResponse
}

func (f *fakeTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{Name: f.name}
}

func (f *fakeTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	f.called = true
	f.gotCtx = ctx
	f.gotCall = call
	return f.resp, nil
}

func (f *fakeTool) ProviderOptions() fantasy.ProviderOptions     { return nil }
func (f *fakeTool) SetProviderOptions(_ fantasy.ProviderOptions) {}

// newRunner builds a hooks.Runner from a single HookConfig, running the
// config-loader path that compiles the matcher regex.
func newRunner(t *testing.T, cmd string) *hooks.Runner {
	t.Helper()
	cfg := &config.Config{
		Hooks: map[string][]config.HookConfig{
			hooks.EventPreToolUse: {{Command: cmd}},
		},
	}
	require.NoError(t, cfg.ValidateHooks())
	return hooks.NewRunner(cfg.Hooks[hooks.EventPreToolUse], t.TempDir(), t.TempDir())
}

func TestHookedTool_AllowStampsHookApproval(t *testing.T) {
	t.Parallel()

	inner := &fakeTool{name: "view", resp: fantasy.NewTextResponse("ok")}
	runner := newRunner(t, `echo '{"decision":"allow"}'`)
	tool := newHookedTool(inner, runner)

	_, err := tool.Run(t.Context(), fantasy.ToolCall{ID: "call-1", Name: "view"})
	require.NoError(t, err)
	require.True(t, inner.called, "inner tool should have run")

	// The inner tool's permission service can now treat call-1 as pre-approved.
	svc := permission.NewPermissionService(t.TempDir(), false, nil)
	granted, err := svc.Request(inner.gotCtx, permission.CreatePermissionRequest{
		SessionID:  "s1",
		ToolCallID: "call-1",
		ToolName:   "view",
		Action:     "read",
		Path:       t.TempDir(),
	})
	require.NoError(t, err)
	require.True(t, granted, "hook allow should bypass the permission prompt")
}

func TestHookedTool_SilentDoesNotStampApproval(t *testing.T) {
	t.Parallel()

	inner := &fakeTool{name: "view", resp: fantasy.NewTextResponse("ok")}
	runner := newRunner(t, `exit 0`) // no stdout, no decision
	tool := newHookedTool(inner, runner)

	_, err := tool.Run(t.Context(), fantasy.ToolCall{ID: "call-2", Name: "view"})
	require.NoError(t, err)
	require.True(t, inner.called)

	// With no hook opinion, a fresh permission request has nothing stamped
	// and must fall through to the normal flow. We verify by checking that
	// the context does not look pre-approved for this call ID: sending a
	// request that no subscriber resolves will block until cancelled.
	svc := permission.NewPermissionService(t.TempDir(), false, nil)
	ctx, cancel := context.WithCancel(inner.gotCtx)
	cancel()
	granted, err := svc.Request(ctx, permission.CreatePermissionRequest{
		SessionID:  "s1",
		ToolCallID: "call-2",
		ToolName:   "view",
		Action:     "read",
		Path:       t.TempDir(),
	})
	require.Error(t, err, "no approval stamped => request should reach the prompt path")
	require.False(t, granted)
}

func TestHookedTool_DenySkipsInnerTool(t *testing.T) {
	t.Parallel()

	inner := &fakeTool{name: "bash"}
	runner := newRunner(t, `echo "blocked" >&2; exit 2`)
	tool := newHookedTool(inner, runner)

	resp, err := tool.Run(t.Context(), fantasy.ToolCall{ID: "call-3", Name: "bash"})
	require.NoError(t, err)
	require.False(t, inner.called, "denied call must not reach the inner tool")
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "blocked")
}

func TestDetachedHookedToolExecution(t *testing.T) {
	t.Run("foreground sub-agent bypasses hooks", func(t *testing.T) {
		inner := &fakeTool{name: "bash", resp: fantasy.NewTextResponse("ok")}
		tool := newDetachedHookedTool(inner, newRunner(t, `echo "blocked" >&2; exit 2`))

		resp, err := tool.Run(t.Context(), fantasy.ToolCall{ID: "call-foreground", Name: "bash"})
		require.NoError(t, err)
		require.True(t, inner.called)
		require.False(t, resp.IsError)
	})

	t.Run("detached sub-agent runs denying hook", func(t *testing.T) {
		inner := &fakeTool{name: "bash"}
		tool := newDetachedHookedTool(inner, newRunner(t, `echo "blocked" >&2; exit 2`))

		resp, err := tool.Run(permission.WithDetachedAgent(t.Context()), fantasy.ToolCall{ID: "call-denied", Name: "bash"})
		require.NoError(t, err)
		require.False(t, inner.called)
		require.True(t, resp.IsError)
		require.Contains(t, resp.Content, "blocked")
	})

	t.Run("detached sub-agent applies allow and rewrite", func(t *testing.T) {
		inner := &fakeTool{name: "bash", resp: fantasy.NewTextResponse("ok")}
		tool := newDetachedHookedTool(inner, newRunner(t, `echo '{"decision":"allow","updated_input":{"command":"echo rewritten"}}'`))

		_, err := tool.Run(permission.WithDetachedAgent(t.Context()), fantasy.ToolCall{ID: "call-rewritten", Name: "bash", Input: `{"command":"echo original"}`})
		require.NoError(t, err)
		require.True(t, inner.called)
		require.JSONEq(t, `{"command":"echo rewritten"}`, inner.gotCall.Input)

		service := permission.NewPermissionService(t.TempDir(), false, nil)
		granted, err := service.Request(inner.gotCtx, permission.CreatePermissionRequest{
			SessionID:  "child",
			ToolCallID: "call-rewritten",
			ToolName:   "bash",
			Action:     "execute",
			Path:       t.TempDir(),
		})
		require.NoError(t, err)
		require.True(t, granted)
	})
}

func TestWrapToolsWithHooks(t *testing.T) {
	t.Parallel()

	runner := newRunner(t, `exit 0`)
	inputs := []fantasy.AgentTool{&fakeTool{name: "a"}, &fakeTool{name: "b"}}

	t.Run("top-level agent wraps every tool", func(t *testing.T) {
		t.Parallel()
		out := wrapToolsWithHooks(inputs, runner, false)
		require.Len(t, out, len(inputs))
		for i, tool := range out {
			_, ok := tool.(*hookedTool)
			require.Truef(t, ok, "tool %d should be a *hookedTool", i)
		}
	})

	t.Run("sub-agent wraps every tool for detached execution", func(t *testing.T) {
		t.Parallel()
		out := wrapToolsWithHooks(inputs, runner, true)
		require.Len(t, out, len(inputs))
		for i, tool := range out {
			wrapped, ok := tool.(*hookedTool)
			require.Truef(t, ok, "tool %d should be a *hookedTool", i)
			require.True(t, wrapped.detachedOnly)
		}
	})

	t.Run("nil runner skips the wrap for both agent kinds", func(t *testing.T) {
		t.Parallel()
		require.Equal(t, inputs, wrapToolsWithHooks(inputs, nil, false))
		require.Equal(t, inputs, wrapToolsWithHooks(inputs, nil, true))
	})
}
