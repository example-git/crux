package workspace_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/agent"
	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/example-git/crux/internal/server"
	"github.com/example-git/crux/internal/shell"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

// xdgIsolate redirects HOME and XDG_* to fresh temp dirs so config
// loading does not touch the host's real config.
func xdgIsolate(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("CRUX_GLOBAL_CONFIG", t.TempDir())
	t.Setenv("CRUX_GLOBAL_DATA", t.TempDir())
	t.Setenv("CRUX_CACHE_DIR", t.TempDir())
}

// runtimeServer wires the production server handler around an
// httptest.NewServer for integration testing.
type shellTaskCoordinator struct {
	manager *shell.BackgroundShellManager
}

func (shellTaskCoordinator) Run(context.Context, string, string, ...message.Attachment) (*fantasy.AgentResult, error) {
	return nil, fmt.Errorf("agent unavailable")
}

func (shellTaskCoordinator) RunAccepted(context.Context, *agent.AcceptedRun, string, string, ...message.Attachment) (*fantasy.AgentResult, error) {
	return nil, fmt.Errorf("agent unavailable")
}

func (shellTaskCoordinator) BeginAccepted(string) *agent.AcceptedRun       { return nil }
func (shellTaskCoordinator) Cancel(string)                                 {}
func (shellTaskCoordinator) CancelAll()                                    {}
func (shellTaskCoordinator) IsSessionBusy(string) bool                     { return false }
func (shellTaskCoordinator) IsBusy() bool                                  { return false }
func (shellTaskCoordinator) QueuedPrompts(string) int                      { return 0 }
func (shellTaskCoordinator) QueuedPromptsList(string) []agent.QueuedPrompt { return nil }
func (shellTaskCoordinator) ClearQueue(string)                             {}
func (shellTaskCoordinator) Summarize(context.Context, string) error       { return nil }
func (shellTaskCoordinator) Model() agent.Model                            { return agent.Model{} }
func (shellTaskCoordinator) UpdateModels(context.Context) error            { return nil }
func (shellTaskCoordinator) UpdateModelsForState(context.Context, config.AgentModelState) error {
	return nil
}
func (shellTaskCoordinator) GenerateTitle(context.Context, string, string)         {}
func (shellTaskCoordinator) SuggestPrompt(context.Context, string) (string, error) { return "", nil }

func (c shellTaskCoordinator) ListTasks() []managedtask.View {
	ids := c.manager.List()
	tasks := make([]managedtask.View, 0, len(ids))
	for _, id := range ids {
		if backgroundShell, ok := c.manager.Get(id); ok {
			tasks = append(tasks, shellTaskView(backgroundShell))
		}
	}
	return tasks
}

func (c shellTaskCoordinator) TaskOutput(ctx context.Context, id string, wait bool, timeout time.Duration) (managedtask.OutputResult, error) {
	backgroundShell, ok := c.manager.Get(id)
	if !ok {
		return managedtask.OutputResult{}, fmt.Errorf("background shell not found: %s", id)
	}
	output, status, err := backgroundShell.ReadOutput(ctx, managedtask.ReadOptions{Stream: managedtask.OutputStreamMerged}, wait, timeout)
	return managedtask.OutputResult{
		Task:            shellTaskView(backgroundShell),
		Output:          string(output.Output),
		RetrievalStatus: status,
		Status:          status,
		NextOffset:      output.NextOffset,
		OutputTruncated: output.OutputTruncated,
	}, err
}

func (c shellTaskCoordinator) StopTask(ctx context.Context, id string) (managedtask.View, error) {
	if _, err := c.manager.Stop(ctx, id); err != nil {
		return managedtask.View{}, err
	}
	backgroundShell, ok := c.manager.Get(id)
	if !ok {
		return managedtask.View{}, fmt.Errorf("background shell not found: %s", id)
	}
	return shellTaskView(backgroundShell), nil
}

func (shellTaskCoordinator) ContinueTask(context.Context, string, string, string, string) (managedtask.View, error) {
	return managedtask.View{}, fmt.Errorf("task is not a background agent")
}

func (shellTaskCoordinator) DeliverTaskNotification(_ context.Context, _ managedtask.Notification, onPersisted, _ func()) error {
	onPersisted()
	return nil
}

func shellTaskView(backgroundShell *shell.BackgroundShell) managedtask.View {
	return managedtask.View{
		ID:          backgroundShell.ID,
		Type:        managedtask.TypeShell,
		Description: backgroundShell.Description,
		Ownership:   backgroundShell.Ownership,
		State:       backgroundShell.State(),
		OutputRef:   backgroundShell.OutputRef(),
	}
}

type runtimeServer struct {
	srv     *server.Server
	httpSrv *httptest.Server
	host    string
}

func newRuntimeServer(t *testing.T) *runtimeServer {
	t.Helper()
	s := server.NewServer(nil, "tcp", "127.0.0.1:0")
	hs := httptest.NewServer(s.Handler())
	t.Cleanup(hs.Close)

	u, err := url.Parse(hs.URL)
	require.NoError(t, err)
	return &runtimeServer{srv: s, httpSrv: hs, host: u.Host}
}

func (r *runtimeServer) newClient(t *testing.T, path string) *client.Client {
	t.Helper()
	c, err := client.NewClient(path, "tcp", r.host)
	require.NoError(t, err)
	// Retire the client during cleanup so the server releases every
	// claim it holds and tears the workspace down at once, closing the
	// pooled DB connection. Without this a test that leaves clients
	// attached (the SSE cache tests never shut theirs down) keeps the
	// workspace, and its open crux.db, alive past t.TempDir cleanup,
	// which Windows cannot remove while the file is locked.
	t.Cleanup(func() { _ = c.RetireClient(context.Background()) })
	return c
}

// TestClientWorkspace_ConfigChangedRefreshesSiblingCache is the
// cross-client refresh end-to-end test required by PLAN item 4. Two
// ClientWorkspace instances pointed at the same backend workspace
// subscribe to events; when one mutates configuration via the server,
// the other's cached Config snapshot reflects the new value without
// a manual refresh.
func TestClientWorkspace_ConfigChangedRefreshesSiblingCache(t *testing.T) {
	xdgIsolate(t)
	rt := newRuntimeServer(t)

	cwd := t.TempDir()
	dataDir := t.TempDir()

	cA := rt.newClient(t, cwd)
	cB := rt.newClient(t, cwd)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	wsProto, err := cA.CreateWorkspace(ctx, proto.Workspace{Path: cwd, DataDir: dataDir})
	require.NoError(t, err)
	// Client B joins the same workspace by path; the server
	// deduplicates and returns the existing workspace.
	wsProtoB, err := cB.CreateWorkspace(ctx, proto.Workspace{Path: cwd, DataDir: dataDir})
	require.NoError(t, err)
	require.Equal(t, wsProto.ID, wsProtoB.ID)

	wsA := workspace.NewClientWorkspace(cA, *wsProto)
	wsB := workspace.NewClientWorkspace(cB, *wsProtoB)

	// Both clients attach event streams. They run for the
	// lifetime of the test; cancelling via context tears them
	// down. consumeEvents is exercised by Subscribe in production;
	// here we run it inline so we don't need a real *tea.Program.
	evcA, err := cA.SubscribeEvents(ctx, wsProto.ID)
	require.NoError(t, err)
	evcB, err := cB.SubscribeEvents(ctx, wsProto.ID)
	require.NoError(t, err)

	go wsA.ConsumeEventsForTest(evcA, func(tea.Msg) {})
	go wsB.ConsumeEventsForTest(evcB, func(tea.Msg) {})

	// Pre-condition: neither cache has compact mode enabled yet.
	require.NotNil(t, wsA.Config())
	require.NotNil(t, wsB.Config())
	require.False(t, compactMode(wsA.Config()), "compact mode must start disabled on client A")
	require.False(t, compactMode(wsB.Config()), "compact mode must start disabled on client B")

	// Client A flips a real config-mutating workspace operation
	// (SetCompactMode) via the server. PLAN item 4 acceptance:
	// B's cached ws.Config must reflect this change without restart.
	// SetCompactMode is used over UpdatePreferredModel because the
	// latter's autoReload reverts unknown-provider models back to
	// defaults during configureSelectedModels, which would make the
	// assertion test infrastructure rather than the cache wiring.
	require.NoError(t, wsA.SetCompactMode(config.ScopeGlobal, true))

	// Client A writes and refreshes synchronously inside
	// SetCompactMode, so its cache must already reflect the change.
	// Eventually here absorbs any background work but should pass
	// immediately.
	require.Eventually(t, func() bool { return compactMode(wsA.Config()) },
		3*time.Second, 25*time.Millisecond,
		"client A cache must reflect its own compact-mode mutation")

	// Client B must see the same change via the ConfigChanged SSE
	// event triggering its own cached refresh.
	require.Eventually(t, func() bool { return compactMode(wsB.Config()) },
		3*time.Second, 25*time.Millisecond,
		"client B cache must reflect A's compact-mode mutation via SSE")
}

// compactMode is a tiny accessor that survives nil intermediates so
// the Eventually polling loop can call it on a transient cache state.
func compactMode(cfg *config.Config) bool {
	if cfg == nil || cfg.Options == nil {
		return false
	}
	return cfg.Options.TUI.CompactMode
}

// TestClientWorkspace_ConfigChangedSignalArrives is a smaller test
// that asserts the SSE wiring delivers a ConfigChanged event to the
// raw client subscription. It catches breakage in the
// wrapEvent/decoder bridge independent of the workspace cache.
func TestClientWorkspace_ConfigChangedSignalArrives(t *testing.T) {
	xdgIsolate(t)
	rt := newRuntimeServer(t)

	cwd := t.TempDir()
	dataDir := t.TempDir()

	c := rt.newClient(t, cwd)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	wsProto, err := c.CreateWorkspace(ctx, proto.Workspace{Path: cwd, DataDir: dataDir})
	require.NoError(t, err)

	evc, err := c.SubscribeEvents(ctx, wsProto.ID)
	require.NoError(t, err)

	require.NoError(t, c.SetConfigField(ctx, wsProto.ID, config.ScopeGlobal, "options.debug", true))

	gotConfigChanged := false
	deadline := time.After(3 * time.Second)
loop:
	for !gotConfigChanged {
		select {
		case ev, ok := <-evc:
			if !ok {
				break loop
			}
			if cc, isCC := ev.(pubsub.Event[proto.ConfigChanged]); isCC {
				require.Equal(t, wsProto.ID, cc.Payload.WorkspaceID)
				gotConfigChanged = true
			}
		case <-deadline:
			break loop
		}
	}
	require.True(t, gotConfigChanged, "expected ConfigChanged event over SSE")
}

func TestServer_SharedWorkspaceManagedTaskLifecycle(t *testing.T) {
	xdgIsolate(t)
	runtime := newRuntimeServer(t)

	workingDir := t.TempDir()
	dataDir := t.TempDir()
	clientA := runtime.newClient(t, workingDir)
	clientB := runtime.newClient(t, workingDir)
	ctx := t.Context()

	workspaceA, err := clientA.CreateWorkspace(ctx, proto.Workspace{Path: workingDir, DataDir: dataDir})
	require.NoError(t, err)
	workspaceB, err := clientB.CreateWorkspace(ctx, proto.Workspace{Path: workingDir, DataDir: dataDir})
	require.NoError(t, err)
	require.Equal(t, workspaceA.ID, workspaceB.ID)

	live, err := runtime.srv.Backend().GetWorkspace(workspaceA.ID)
	require.NoError(t, err)
	live.AgentCoordinator = shellTaskCoordinator{manager: live.BackgroundShells}
	backgroundShell, err := live.BackgroundShells.StartOwned(context.Background(), workingDir, nil, "printf shared-output; sleep 30", "shared shell", managedtask.Ownership{
		ParentSessionID:  "parent-session",
		OriginToolCallID: "tool-call",
	})
	require.NoError(t, err)
	backgroundShell.MarkBackgrounded()
	t.Cleanup(func() {
		_, _ = live.StopTask(context.Background(), backgroundShell.ID)
	})

	for _, currentClient := range []*client.Client{clientA, clientB} {
		require.Eventually(t, func() bool {
			tasks, listErr := currentClient.ListTasks(ctx, workspaceA.ID)
			return listErr == nil && len(tasks) == 1 && tasks[0].ID == backgroundShell.ID && tasks[0].State.Status == managedtask.StatusRunning
		}, 3*time.Second, 25*time.Millisecond)
	}

	require.Eventually(t, func() bool {
		output, outputErr := clientB.TaskOutput(ctx, workspaceA.ID, backgroundShell.ID, false, 0)
		return outputErr == nil && output.Output == "shared-output"
	}, 3*time.Second, 25*time.Millisecond)

	stopped, err := clientB.StopTask(ctx, workspaceA.ID, backgroundShell.ID)
	require.NoError(t, err)
	require.Equal(t, managedtask.StatusKilled, stopped.State.Status)

	tasks, err := clientA.ListTasks(ctx, workspaceA.ID)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	require.Equal(t, managedtask.StatusKilled, tasks[0].State.Status)

	var notification managedtask.Notification
	require.Eventually(t, func() bool {
		notifications, listErr := clientA.ListTaskNotifications(ctx, workspaceA.ID, "parent-session", true)
		if listErr != nil || len(notifications) != 1 {
			return false
		}
		notification = notifications[0]
		return notification.TaskID == backgroundShell.ID
	}, 3*time.Second, 25*time.Millisecond)

	read, err := clientB.MarkTaskNotificationRead(ctx, workspaceA.ID, notification.ID)
	require.NoError(t, err)
	require.False(t, read.ReadAt.IsZero())
	unread, err := clientA.ListTaskNotifications(ctx, workspaceA.ID, "parent-session", true)
	require.NoError(t, err)
	require.Empty(t, unread)
}

// -- Concurrent session lifecycle --
//
// These drive the real HTTP surface, because the bug they cover only
// appears when two sessions share a server: the first client's workspace
// died, and the sibling kept the server (and its 404s) alive so the first
// client never got a fresh start.

// TestServer_ConcurrentSessionsSameDirShareOneWorkspace checks the
// baseline for two sessions in one directory: they share a workspace, and
// one of them leaving must not disturb the other.
func TestServer_ConcurrentSessionsSameDirShareOneWorkspace(t *testing.T) {
	xdgIsolate(t)
	rt := newRuntimeServer(t)
	rt.srv.Backend().SetDetachGrace(0)

	cwd, dataDir := t.TempDir(), t.TempDir()
	cA, cB := rt.newClient(t, cwd), rt.newClient(t, cwd)
	ctx := t.Context()

	wsA, err := cA.CreateWorkspace(ctx, proto.Workspace{Path: cwd, DataDir: dataDir})
	require.NoError(t, err)
	wsB, err := cB.CreateWorkspace(ctx, proto.Workspace{Path: cwd, DataDir: dataDir})
	require.NoError(t, err)
	require.Equal(t, wsA.ID, wsB.ID, "same directory must share one workspace")

	// A leaves. B's claim must keep the workspace addressable.
	require.NoError(t, cA.RetireClient(ctx))
	got, err := cB.GetWorkspace(ctx, wsB.ID)
	require.NoError(t, err, "one client leaving must not destroy the shared workspace")
	require.Equal(t, wsB.ID, got.ID)
}

// TestServer_ConcurrentSessionsDifferentDirsAreIndependent is the other
// half: two directories get two workspaces on one server, and retiring
// one client must leave the other's workspace completely alone.
func TestServer_ConcurrentSessionsDifferentDirsAreIndependent(t *testing.T) {
	xdgIsolate(t)
	rt := newRuntimeServer(t)
	rt.srv.Backend().SetDetachGrace(0)

	cwdA, cwdB := t.TempDir(), t.TempDir()
	cA, cB := rt.newClient(t, cwdA), rt.newClient(t, cwdB)
	ctx := t.Context()

	wsA, err := cA.CreateWorkspace(ctx, proto.Workspace{Path: cwdA, DataDir: t.TempDir()})
	require.NoError(t, err)
	wsB, err := cB.CreateWorkspace(ctx, proto.Workspace{Path: cwdB, DataDir: t.TempDir()})
	require.NoError(t, err)
	require.NotEqual(t, wsA.ID, wsB.ID)

	require.NoError(t, cA.RetireClient(ctx))

	_, err = cB.GetWorkspace(ctx, wsB.ID)
	require.NoError(t, err, "a sibling session's workspace must survive")
	_, err = cB.GetWorkspace(ctx, wsA.ID)
	require.ErrorIs(t, err, client.ErrNotFound, "the retired client's workspace must be gone")
}

// TestServer_RefusesShutdownWhileWorkspaceLive is the invariant that keeps
// an upgrading client from killing a running session. The refusal has to
// come from the server: a client checking idleness itself needs a second
// round trip that a new session can slip into.
func TestServer_RefusesShutdownWhileWorkspaceLive(t *testing.T) {
	xdgIsolate(t)
	rt := newRuntimeServer(t)

	cwd := t.TempDir()
	cLive := rt.newClient(t, cwd)
	ctx := t.Context()

	ws, err := cLive.CreateWorkspace(ctx, proto.Workspace{Path: cwd, DataDir: t.TempDir()})
	require.NoError(t, err)

	// A second client, standing in for one that found a version mismatch.
	err = rt.newClient(t, t.TempDir()).ShutdownServerIfIdle(ctx)
	require.ErrorIs(t, err, client.ErrServerBusy,
		"a server hosting a workspace must refuse to stand down")

	_, err = cLive.GetWorkspace(ctx, ws.ID)
	require.NoError(t, err, "the live session's workspace must be untouched")
}

// TestServer_DetachGraceSurvivesStreamBlip is the server-side half of the
// SSE regression. Cutting the stream used to destroy the workspace
// instantly, so the client's reconnect — 250ms later — came back to an ID
// the server no longer knew, and 404'd from then on.
func TestServer_DetachGraceSurvivesStreamBlip(t *testing.T) {
	xdgIsolate(t)
	rt := newRuntimeServer(t)
	rt.srv.Backend().SetDetachGrace(30 * time.Second)

	cwd := t.TempDir()
	c := rt.newClient(t, cwd)

	ws, err := c.CreateWorkspace(t.Context(), proto.Workspace{Path: cwd, DataDir: t.TempDir()})
	require.NoError(t, err)

	streamCtx, killStream := context.WithCancel(t.Context())
	evc, err := c.SubscribeEvents(streamCtx, ws.ID)
	require.NoError(t, err)

	// Cut the stream the way a network blip does: no release first.
	killStream()
	for range evc { //nolint:revive // Drain until the server closes it.
	}

	require.Eventually(t, func() bool {
		_, err := c.GetWorkspace(t.Context(), ws.ID)
		return err == nil
	}, 3*time.Second, 25*time.Millisecond,
		"the workspace must survive the drop so the reconnect can re-attach")

	// The reconnect lands and the workspace is still the same one.
	evc2, err := c.SubscribeEvents(t.Context(), ws.ID)
	require.NoError(t, err, "the client must be able to re-attach to its workspace")
	require.NotNil(t, evc2)
}

// TestClientWorkspace_RecoversAfterServerSideTeardown is the whole bug,
// end to end: the server drops the client's workspace while the server
// itself stays up (a sibling session keeps it alive). The client must
// notice the 404, re-register, and end up on a workspace it can use again
// rather than 404ing forever.
func TestClientWorkspace_RecoversAfterServerSideTeardown(t *testing.T) {
	xdgIsolate(t)
	t.Cleanup(workspace.SetSSEBackoffForTest(5*time.Millisecond, 25*time.Millisecond))

	rt := newRuntimeServer(t)
	rt.srv.Backend().SetDetachGrace(0)

	cwd, dataDir := t.TempDir(), t.TempDir()
	// A sibling session in another directory keeps the server alive, which
	// is what made the 404s permanent instead of self-healing.
	sibling := rt.newClient(t, t.TempDir())
	siblingCwd := t.TempDir()
	_, err := sibling.CreateWorkspace(t.Context(), proto.Workspace{
		Path: siblingCwd, DataDir: t.TempDir(),
	})
	require.NoError(t, err)

	c := rt.newClient(t, cwd)
	wsProto, err := c.CreateWorkspace(t.Context(), proto.Workspace{Path: cwd, DataDir: dataDir})
	require.NoError(t, err)
	originalID := wsProto.ID

	// The server drops the workspace while the client still holds the
	// snapshot naming it. That is what an upgrade, or a teardown racing a
	// stream drop, leaves behind: a live server that answers 404 for the
	// only workspace ID this client knows.
	require.NoError(t, c.DeleteWorkspace(t.Context(), originalID))
	_, err = c.GetWorkspace(t.Context(), originalID)
	require.ErrorIs(t, err, client.ErrNotFound, "the workspace must really be gone")

	ws := workspace.NewClientWorkspace(c, *wsProto)
	done := make(chan struct{})
	go func() {
		ws.RunSubscriptionForTest(func(tea.Msg) {})
		close(done)
	}()

	require.Eventually(t, func() bool {
		id := ws.WorkspaceIDForTest()
		return id != "" && id != originalID
	}, 10*time.Second, 25*time.Millisecond,
		"the client must re-register instead of retrying a workspace the server forgot")

	// The recovered workspace is usable, which is what 404s prevented.
	_, err = ws.ListSessions(t.Context())
	require.NoError(t, err, "the recovered workspace must serve requests again")

	ws.Shutdown()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the subscription loop did not stop after Shutdown")
	}
}
