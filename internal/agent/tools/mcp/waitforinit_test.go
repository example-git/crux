package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// swapInitGate installs an armed gate on this test's private manager, so the
// test can drive startup completion without launching a transport.
func swapInitGate(t *testing.T, runtime *Manager) chan struct{} {
	t.Helper()
	orig := runtime.initDone
	runtime.initDone = make(chan struct{})

	runtime.initMu.Lock()
	origStarted := runtime.initStarted
	runtime.initStarted = true
	runtime.initMu.Unlock()

	t.Cleanup(func() {
		runtime.initDone = orig
		runtime.initMu.Lock()
		runtime.initStarted = origStarted
		runtime.initMu.Unlock()
	})
	return runtime.initDone
}

// TestWaitForInit_BlocksUntilInitCompletes pins the contract the
// non-interactive path relies on: WaitForInit blocks while MCP initialization
// is still in flight and returns once it completes. Non-interactive runs
// (`crux run`) wait on it before reading the tool registry so slow-to-start
// servers (e.g. stdio Python via uv) have registered their tools first.
// Interactive runs deliberately do not gate on it (a slow server froze the
// TUI's first prompt); they build the tool palette from whatever is registered
// at send time and pick up late servers on later runs. See coordinator.run.
func TestWaitForInit_BlocksUntilInitCompletes(t *testing.T) {
	runtime := newManager()
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	gate := swapInitGate(t, runtime)

	// Init not done yet: WaitForInit must block until the context expires.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, runtime.WaitForInit(ctx), context.DeadlineExceeded,
		"WaitForInit must block while initialization is in flight")

	// Once initialization completes (the gate closes), WaitForInit returns nil.
	runtime.initOnce.Do(func() { close(gate) })
	require.NoError(t, runtime.WaitForInit(context.Background()),
		"WaitForInit must return once initialization has completed")
}

// TestWaitForInit_ReturnsWhenNotArmed is the regression test for callers
// outside app startup. Those paths never call mcp.Initialize (which is
// what arms the gate), so WaitForInit must return immediately instead of
// blocking on a channel that will never close. Before the fix it blocked
// until ctx was cancelled, hanging RunNonInteractive's gate forever.
func TestWaitForInit_ReturnsWhenNotArmed(t *testing.T) {
	runtime := newManager()
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	// Ensure the gate looks unarmed regardless of test ordering.
	runtime.initMu.Lock()
	orig := runtime.initStarted
	runtime.initStarted = false
	runtime.initMu.Unlock()
	t.Cleanup(func() {
		runtime.initMu.Lock()
		runtime.initStarted = orig
		runtime.initMu.Unlock()
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, runtime.WaitForInit(ctx),
		"WaitForInit must return immediately when initialization was never armed")
}

// TestWaitForInit_ToolsVisibleAfterInit pins the visibility guarantee
// WaitForInit gives the non-interactive path: any tool registered before
// initialization completes must be visible once WaitForInit returns. The
// interactive coordinator deliberately no longer relies on this (it reads the
// registry ungated and picks up late tools on subsequent runs); this test
// keeps the guarantee for non-interactive runs, which still wait.
func TestWaitForInit_ToolsVisibleAfterInit(t *testing.T) {
	runtime := newManager()
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	const name = "test-waitforinit-tools"
	t.Cleanup(func() {
		if s, ok := runtime.sessions.Take(name); ok {
			_ = s.Close()
		}
		runtime.allTools.Del(name)
		runtime.states.Del(name)
	})

	sess, _ := liveSession(t, "slow_tool")
	gate := swapInitGate(t, runtime)

	// A slow MCP server registers its tools, then initialization completes
	// (the gate closes). runtime.initOnce.Do(func(){close(gate)}) happens-after the registration, and
	// WaitForInit returning happens-after observing the close, so the tools are
	// guaranteed visible once WaitForInit returns.
	go func() {
		runtime.sessions.Set(name, sess)
		runtime.allTools.Set(name, []*Tool{{Name: "slow_tool"}})
		runtime.updateState(name, StateConnected, nil, sess, Counts{Tools: 1})
		runtime.initOnce.Do(func() { close(gate) })
	}()

	require.NoError(t, runtime.WaitForInit(context.Background()))

	tools, ok := runtime.allTools.Get(name)
	require.True(t, ok, "a slow server's tools must be visible after WaitForInit returns")
	require.Len(t, tools, 1)
	require.Equal(t, "slow_tool", tools[0].Name)
}
