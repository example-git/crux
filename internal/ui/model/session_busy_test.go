package model

import (
	"context"
	"image"
	"testing"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/example-git/crux/internal/agent"
	"github.com/example-git/crux/internal/agent/notify"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/history"
	"github.com/example-git/crux/internal/imageattachment"
	"github.com/example-git/crux/internal/lsp"
	"github.com/example-git/crux/internal/message"
	oauthusage "github.com/example-git/crux/internal/oauth/usage"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/example-git/crux/internal/question"
	"github.com/example-git/crux/internal/session"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/example-git/crux/internal/ui/attachments"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/completions"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/example-git/crux/internal/ui/notification"
	"github.com/example-git/crux/internal/workspace"
)

// countingWorkspace is a workspace.Workspace stub that counts every probe
// that is a synchronous HTTP round-trip in client/server mode, split per
// method so tests can pin exactly which probes ran. The embedded interface
// panics on anything unimplemented.
type countingWorkspace struct {
	workspace.Workspace

	ready     bool
	agentBusy bool
	yolo      bool
	queued    []agent.QueuedPrompt
	model     workspace.AgentModel
	lspStates map[string]workspace.LSPClientInfo
	lspDiags  map[string]lsp.DiagnosticCounts
	mode      session.Mode
	tasks     []managedtask.View

	readyCalls      int
	agentBusyCalls  int
	queuedCalls     int
	queueListCalls  int
	permCalls       int
	permSetCalls    int
	clearQueueCalls int
	cancelCalls     int
	modelCalls      int
	lspStateCalls   int
	lspDiagCalls    int
	modeCalls       int
	taskListCalls   int
}

func (w *countingWorkspace) AgentIsReady() bool { w.readyCalls++; return w.ready }
func (w *countingWorkspace) AgentIsBusy() bool  { w.agentBusyCalls++; return w.agentBusy }

func (w *countingWorkspace) AgentReadyErr() error {
	w.readyCalls++
	if w.ready {
		return nil
	}
	return workspace.ErrAgentNotInitialized
}

func (w *countingWorkspace) AgentQueuedPrompts(string) int {
	w.queuedCalls++
	return len(w.queued)
}

func (w *countingWorkspace) AgentQueuedPromptsList(string) []agent.QueuedPrompt {
	w.queueListCalls++
	return w.queued
}

func queuedPrompts(prompts ...string) []agent.QueuedPrompt {
	queued := make([]agent.QueuedPrompt, len(prompts))
	for i, prompt := range prompts {
		queued[i] = agent.QueuedPrompt{SubmissionID: prompt, Prompt: prompt}
	}
	return queued
}

func (w *countingWorkspace) PermissionSkipRequests() bool { w.permCalls++; return w.yolo }

func (w *countingWorkspace) PermissionSetSkipRequests(skip bool) {
	w.permSetCalls++
	w.yolo = skip
}

func (w *countingWorkspace) AgentClearQueue(string) { w.clearQueueCalls++; w.queued = nil }
func (w *countingWorkspace) AgentCancel(string)     { w.cancelCalls++ }

func (w *countingWorkspace) AgentModel() workspace.AgentModel {
	w.modelCalls++
	return w.model
}

func (w *countingWorkspace) LSPGetStates() map[string]workspace.LSPClientInfo {
	w.lspStateCalls++
	return w.lspStates
}

func (w *countingWorkspace) LSPGetDiagnosticCounts(name string) lsp.DiagnosticCounts {
	w.lspDiagCalls++
	return w.lspDiags[name]
}

func (w *countingWorkspace) ListMessages(context.Context, string) ([]message.Message, error) {
	return nil, nil
}

func (w *countingWorkspace) ListUserMessages(context.Context, string) ([]message.Message, error) {
	return nil, nil
}

func (w *countingWorkspace) ListTasks(context.Context) ([]managedtask.View, error) {
	w.taskListCalls++
	return w.tasks, nil
}

func (w *countingWorkspace) WorkingDir() string { return "" }

func (w *countingWorkspace) LSPStart(context.Context, string) {}

func (w *countingWorkspace) Config() *config.Config {
	return &config.Config{Options: &config.Options{}}
}

func (w *countingWorkspace) ProviderSurfaces() []providerregistry.Surface { return nil }

func (w *countingWorkspace) SetSessionMode(_ context.Context, sessionID string, mode session.Mode) (session.Session, error) {
	w.modeCalls++
	w.mode = mode
	return session.Session{ID: sessionID, Mode: mode}, nil
}

// syncProbes sums every synchronous counter; Update/View must keep this at
// zero — the invariant is that no workspace call ever happens on the Update
// goroutine (which is also the render loop).
func (w *countingWorkspace) syncProbes() int {
	return w.readyCalls + w.agentBusyCalls +
		w.queuedCalls + w.queueListCalls + w.permCalls +
		w.modelCalls + w.lspStateCalls + w.lspDiagCalls
}

func (w *countingWorkspace) resetCounters() {
	w.readyCalls, w.agentBusyCalls = 0, 0
	w.queuedCalls, w.queueListCalls, w.permCalls = 0, 0, 0
	w.permSetCalls, w.clearQueueCalls, w.cancelCalls = 0, 0, 0
	w.modelCalls, w.lspStateCalls, w.lspDiagCalls = 0, 0, 0
	w.taskListCalls = 0
}

// newBusyUI builds a UI wired to the stub workspace with an active session
// "s1", enough state for Update to run end to end.
func newBusyUI(ws *countingWorkspace) *UI {
	com := common.DefaultCommon(ws)
	return &UI{
		com:         com,
		status:      NewStatus(com, nil),
		chat:        NewChat(com, config.ScrollbarDefault),
		textarea:    textarea.New(),
		state:       uiChat,
		focus:       uiFocusEditor,
		width:       140,
		height:      45,
		session:     &session.Session{ID: "s1"},
		keyMap:      DefaultKeyMap(),
		dialog:      dialog.NewOverlay(),
		attachments: attachments.New(nil, attachments.Keymap{}),
	}
}

// pinTTLs makes the TTL backstop inert for the duration of the test so
// assertions about event-driven refreshes cannot flake by straddling a TTL
// boundary (the tests using it must not call t.Parallel).
func pinTTLs(t *testing.T) {
	t.Helper()
	oldBusy, oldQueue, oldLSP := busyCacheTTL, promptQueueTTL, lspStatesTTL
	busyCacheTTL = time.Hour
	promptQueueTTL = time.Hour
	lspStatesTTL = time.Hour
	t.Cleanup(func() { busyCacheTTL, promptQueueTTL, lspStatesTTL = oldBusy, oldQueue, oldLSP })
}

// warmCaches marks all memoized workspace state fresh so only explicit
// invalidation (not startup staleness) can trigger refresh dispatches.
func warmCaches(m *UI, busy bool) {
	m.agentBusyCache.set(busy)
	m.yoloCache.set(false)
	m.agentReady = true
	m.promptQueueCheckedAt = time.Now()
	m.lspCheckedAt = time.Now()
}

// runCmds executes a command tree the way the Bubble Tea runtime would,
// feeding cache-refresh messages back into Update. Other leaf commands are
// executed (for their side effects on the stub) but their messages dropped.
func runCmds(m *UI, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		for _, c := range msg {
			runCmds(m, c)
		}
	case busyStateMsg, promptQueueMsg, agentRunSubmittedMsg, lspStatesMsg, agentModelChangedMsg, planModeToggledMsg:
		_, next := m.Update(msg)
		runCmds(m, next)
	}
}

// plainMsg is an arbitrary tea.Msg standing in for keystroke/mouse/tick
// traffic through Update.
type plainMsg struct{}

type recordingNotificationBackend struct {
	notifications []notification.Notification
}

func (b *recordingNotificationBackend) Send(value notification.Notification) tea.Cmd {
	b.notifications = append(b.notifications, value)
	return nil
}

func TestControlArrowDownFromPopulatedEditorOpensTasksWhenPresent(t *testing.T) {
	workspace := &countingWorkspace{
		ready: true,
		tasks: []managedtask.View{{ID: "b12345678", Type: managedtask.TypeShell}},
	}
	model := newBusyUI(workspace)
	model.textarea.SetValue("draft prompt")
	model.promptHistory.index = -1

	command := model.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModCtrl})
	require.NotNil(t, command)
	require.Zero(t, workspace.taskListCalls)
	var availability tasksAvailabilityMsg
	switch message := command().(type) {
	case tasksAvailabilityMsg:
		availability = message
	case tea.BatchMsg:
		for _, child := range message {
			if childMessage, matches := child().(tasksAvailabilityMsg); matches {
				availability = childMessage
			}
		}
	}
	require.True(t, availability.available)
	require.Equal(t, 1, workspace.taskListCalls)

	model.Update(availability)
	require.True(t, model.dialog.ContainsDialog(dialog.TasksID))
}

func TestArrowDownPreservesHistoryNavigationWithoutCheckingTasks(t *testing.T) {
	workspace := &countingWorkspace{
		ready: true,
		tasks: []managedtask.View{{ID: "b12345678", Type: managedtask.TypeShell}},
	}
	model := newBusyUI(workspace)
	model.promptHistory.messages = []string{"previous prompt"}
	model.promptHistory.index = -1
	require.True(t, model.historyPrev())
	require.Equal(t, "previous prompt", model.textarea.Value())
	model.textarea.MoveToEnd()

	model.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyDown})

	require.Zero(t, workspace.taskListCalls)
	require.Equal(t, "", model.textarea.Value())
	require.Equal(t, -1, model.promptHistory.index)
	require.False(t, model.dialog.ContainsDialog(dialog.TasksID))
}

func TestArrowDownDoesNotOpenTasksWhileQuestionIsActive(t *testing.T) {
	workspace := &countingWorkspace{
		ready: true,
		tasks: []managedtask.View{{ID: "b12345678", Type: managedtask.TypeShell}},
	}
	model := newBusyUI(workspace)
	model.activeInline = dialog.NewQuestionForm(model.com.Styles, question.Request{
		Questions: []question.Question{{
			ID:          "question-1",
			Type:        question.TypeYesNo,
			Text:        "Continue?",
			Description: "Choose whether to continue.",
		}},
	})
	model.activeInline.SetFocused(true)

	model.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyDown})

	require.Zero(t, workspace.taskListCalls)
	require.False(t, model.dialog.ContainsDialog(dialog.TasksID))
}

func TestControlArrowDownOpensTasksWhileQuestionIsActive(t *testing.T) {
	workspace := &countingWorkspace{
		ready: true,
		tasks: []managedtask.View{{ID: "b12345678", Type: managedtask.TypeShell}},
	}
	model := newBusyUI(workspace)
	model.activeInline = dialog.NewQuestionForm(model.com.Styles, question.Request{
		Questions: []question.Question{{
			ID:          "question-1",
			Type:        question.TypeYesNo,
			Text:        "Continue?",
			Description: "Choose whether to continue.",
		}},
	})
	model.activeInline.SetFocused(true)

	command := model.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModCtrl})
	require.NotNil(t, command)
	var availability tasksAvailabilityMsg
	switch message := command().(type) {
	case tasksAvailabilityMsg:
		availability = message
	case tea.BatchMsg:
		for _, child := range message {
			if childMessage, matches := child().(tasksAvailabilityMsg); matches {
				availability = childMessage
			}
		}
	}
	require.True(t, availability.available)

	model.Update(availability)
	require.True(t, model.dialog.ContainsDialog(dialog.TasksID))
}

func TestControlArrowDownOpensTasksWhileAutocompleteIsActive(t *testing.T) {
	workspace := &countingWorkspace{
		ready: true,
		tasks: []managedtask.View{{ID: "b12345678", Type: managedtask.TypeShell}},
	}
	model := newBusyUI(workspace)
	model.completions = completions.New(
		model.com.Styles.Completions.Normal,
		model.com.Styles.Completions.Focused,
		model.com.Styles.Completions.Match,
	)
	model.completionsOpen = true

	model.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyDown})
	require.Zero(t, workspace.taskListCalls)
	require.True(t, model.completionsOpen)

	command := model.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModCtrl})
	require.NotNil(t, command)
	var availability tasksAvailabilityMsg
	switch message := command().(type) {
	case tasksAvailabilityMsg:
		availability = message
	case tea.BatchMsg:
		for _, child := range message {
			if childMessage, matches := child().(tasksAvailabilityMsg); matches {
				availability = childMessage
			}
		}
	}
	require.True(t, availability.available)
	require.Equal(t, 1, workspace.taskListCalls)

	model.Update(availability)
	require.False(t, model.completionsOpen)
	require.True(t, model.dialog.ContainsDialog(dialog.TasksID))
}

func TestArrowDownDoesNotOpenTasksWhileDialogIsOpen(t *testing.T) {
	workspace := &countingWorkspace{
		ready: true,
		tasks: []managedtask.View{{ID: "b12345678", Type: managedtask.TypeShell}},
	}

	for _, test := range []struct {
		name string
		open func(*UI)
	}{
		{name: "tasks", open: func(model *UI) { model.openTasksDialog() }},
		{name: "other", open: func(model *UI) { model.openNotificationsDialog() }},
	} {
		t.Run(test.name, func(t *testing.T) {
			model := newBusyUI(workspace)
			test.open(model)

			model.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyDown})
			model.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModCtrl})

			require.Zero(t, workspace.taskListCalls)
		})
	}
}

func TestTaskAvailabilityDoesNotOverrideNewDialog(t *testing.T) {
	workspace := &countingWorkspace{ready: true}
	model := newBusyUI(workspace)
	model.openNotificationsDialog()

	model.Update(tasksAvailabilityMsg{available: true})

	require.True(t, model.dialog.ContainsDialog(dialog.NotificationsID))
	require.False(t, model.dialog.ContainsDialog(dialog.TasksID))
}

// TestUpdateDoesNotProbeWorkspacePerMessage pins the hot-path fix: Update
// used to call AgentQueuedPrompts (a synchronous HTTP GET in client/server
// mode) at the top of every message while the agent was busy, and the
// placeholder path probed AgentIsReady/AgentIsBusy/PermissionSkipRequests —
// every keystroke blocked the single Update goroutine on network round-
// trips. Now Update performs no synchronous workspace call at all; refreshes
// are dispatched as commands.
func TestUpdateDoesNotProbeWorkspacePerMessage(t *testing.T) {
	pinTTLs(t)

	ws := &countingWorkspace{ready: true}
	m := newBusyUI(ws)

	for range 25 {
		m.Update(plainMsg{})
	}
	require.Zero(t, ws.queuedCalls,
		"Update must not call AgentQueuedPrompts per message (HTTP per keystroke in client mode)")
	require.Zero(t, ws.syncProbes(),
		"Update must not make any synchronous workspace call")
}

// TestReadsNeverProbeWorkspace pins the read side of the invariant: the
// busy/yolo getters used by render paths serve the memoized value and never
// probe, so View can never block on HTTP.
func TestReadsNeverProbeWorkspace(t *testing.T) {
	pinTTLs(t)

	ws := &countingWorkspace{ready: true, agentBusy: true}
	m := newBusyUI(ws)

	for range 10 {
		m.isAgentBusy()
		m.yoloModeCached()
	}
	require.Zero(t, ws.syncProbes(), "cache reads must never probe the workspace")
}

// TestStreamingUpdatedEventsDoNotProbe pins the streaming path: per-chunk
// message UpdatedEvents arrive once per streamed token and must neither
// probe the workspace synchronously nor schedule busy/queue refreshes —
// only CreatedEvents (run boundaries) do.
func TestStreamingUpdatedEventsDoNotProbe(t *testing.T) {
	pinTTLs(t)

	ws := &countingWorkspace{ready: true}
	m := newBusyUI(ws)
	warmCaches(m, true)
	ws.resetCounters()

	for range 25 {
		m.Update(pubsub.Event[message.Message]{
			Type:    pubsub.UpdatedEvent,
			Payload: message.Message{ID: "m1", SessionID: "s1", Role: message.Assistant},
		})
	}
	require.Zero(t, ws.syncProbes(),
		"per-chunk UpdatedEvents must not probe the workspace")
	require.False(t, m.busyFetchInFlight,
		"per-chunk UpdatedEvents must not schedule a busy refresh")
	require.False(t, m.promptQueueInFlight,
		"per-chunk UpdatedEvents must not schedule a queue refresh")
}

// TestMessageCreatedEventRefreshesBusyAndQueue: a CreatedEvent is a run
// boundary and must invalidate the memoized busy state and fetch fresh
// busy/queue values off-thread.
func TestMessageCreatedEventRefreshesBusyAndQueue(t *testing.T) {
	pinTTLs(t)

	ws := &countingWorkspace{ready: true, agentBusy: true, queued: queuedPrompts("queued prompt")}
	m := newBusyUI(ws)
	warmCaches(m, false)
	ws.resetCounters()

	_, cmd := m.Update(pubsub.Event[message.Message]{
		Type:    pubsub.CreatedEvent,
		Payload: message.Message{ID: "m1", SessionID: "s1", Role: message.User},
	})
	require.Zero(t, ws.syncProbes(), "the event handler itself must not probe synchronously")
	require.True(t, m.busyFetchInFlight, "CreatedEvent must schedule a busy refresh")
	require.True(t, m.promptQueueInFlight, "CreatedEvent must schedule a queue refresh")

	runCmds(m, cmd)
	require.True(t, m.isAgentBusy(), "refreshed busy state must land in the cache")
	require.Equal(t, 1, m.promptQueue, "refreshed queue count must land in the cache")
	require.False(t, m.busyFetchInFlight)
	require.False(t, m.promptQueueInFlight)
}

// TestAgentTerminalNotificationsRefreshBusy pins the busy→idle edge: the
// agent clears its active request before publishing TypeAgentFinished (and
// TypeAgentError) precisely so observers can re-probe. The handler must
// invalidate the memoized busy state and re-fetch busy + queue.
func TestAgentTerminalNotificationsRefreshBusy(t *testing.T) {
	pinTTLs(t)

	for _, typ := range []notify.Type{notify.TypeAgentFinished, notify.TypeAgentError} {
		t.Run(string(typ), func(t *testing.T) {
			ws := &countingWorkspace{ready: true} // agent now idle
			m := newBusyUI(ws)
			warmCaches(m, true) // stale: still busy
			ws.resetCounters()
			require.True(t, m.isAgentBusy())

			_, cmd := m.Update(pubsub.Event[notify.Notification]{
				Type:    pubsub.CreatedEvent,
				Payload: notify.Notification{Type: typ, SessionID: "s1"},
			})
			require.True(t, m.busyFetchInFlight, "terminal notification must schedule a busy refresh")
			require.True(t, m.promptQueueInFlight, "terminal notification must schedule a queue refresh")

			runCmds(m, cmd)
			require.False(t, m.isAgentBusy(),
				"busy→idle edge must reach the cache without waiting for the TTL")
		})
	}
}

func TestRecoveredTaskNotificationsStayWithOwningSession(t *testing.T) {
	backend := &recordingNotificationBackend{}
	model := newBusyUI(&countingWorkspace{ready: true})
	model.caps.ReportFocusEvents = true
	model.notifyWindowFocused = false
	model.notifyBackend = backend

	model.Update(pubsub.Event[managedtask.Notification]{
		Type: pubsub.CreatedEvent,
		Payload: managedtask.Notification{
			ParentSessionID: "other-session",
			Summary:         "foreign recovered task",
		},
	})
	require.Empty(t, backend.notifications)

	model.Update(pubsub.Event[managedtask.Notification]{
		Type: pubsub.CreatedEvent,
		Payload: managedtask.Notification{
			ParentSessionID: "s1",
			Summary:         "own recovered task",
		},
	})
	require.Equal(t, []notification.Notification{{
		Title:   "Background task finished",
		Message: "own recovered task",
	}}, backend.notifications)
}

func TestAgentNotificationsStayWithOwningSession(t *testing.T) {
	pinTTLs(t)

	backend := &recordingNotificationBackend{}
	model := newBusyUI(&countingWorkspace{ready: true})
	warmCaches(model, true)
	model.caps.ReportFocusEvents = true
	model.notifyWindowFocused = false
	model.notifyBackend = backend

	model.Update(pubsub.Event[notify.Notification]{
		Type: pubsub.CreatedEvent,
		Payload: notify.Notification{
			SessionID:    "other-session",
			SessionTitle: "Other session",
			Type:         notify.TypeAgentFinished,
		},
	})
	require.Empty(t, backend.notifications)
	require.False(t, model.busyFetchInFlight)
	require.False(t, model.promptQueueInFlight)

	model.Update(pubsub.Event[notify.Notification]{
		Type: pubsub.CreatedEvent,
		Payload: notify.Notification{
			SessionID:    "s1",
			SessionTitle: "Current session",
			Type:         notify.TypeAgentFinished,
		},
	})
	require.Equal(t, []notification.Notification{{
		Title:   "Crux is waiting...",
		Message: "Agent's turn completed in \"Current session\"",
	}}, backend.notifications)
	require.True(t, model.busyFetchInFlight)
	require.True(t, model.promptQueueInFlight)
}

// TestSessionSwitchRefreshesQueueAndBusy: switching sessions must drop the
// previous session's queue pill and memoized busy state and fetch the new
// session's, so esc never offers to clear the wrong queue.
func TestSessionSwitchRefreshesQueueAndBusy(t *testing.T) {
	pinTTLs(t)

	ws := &countingWorkspace{ready: true, queued: queuedPrompts("a", "b")}
	m := newBusyUI(ws)
	warmCaches(m, true)
	m.promptQueue = 5 // stale queue pill from the previous session
	m.promptQueueItems = queuedPrompts("x", "y", "z", "w", "v")
	ws.resetCounters()

	_, cmd := m.Update(loadSessionMsg{session: &session.Session{ID: "s2"}})
	require.Zero(t, m.promptQueue, "switching sessions must drop the old session's queue pill")
	require.True(t, m.promptQueueInFlight, "session switch must schedule a queue refresh")
	require.True(t, m.busyFetchInFlight, "session switch must schedule a busy refresh")

	runCmds(m, cmd)
	require.Equal(t, 2, m.promptQueue, "the new session's queue must be fetched")
	require.Equal(t, queuedPrompts("a", "b"), m.promptQueueItems)
}

// TestToggleYoloWritesThroughCache: both yolo toggle paths share
// toggleYoloMode, which must write the known new value through the cache —
// no invalidation, no re-probe.
func TestToggleYoloWritesThroughCache(t *testing.T) {
	pinTTLs(t)

	ws := &countingWorkspace{ready: true, yolo: false}
	m := newBusyUI(ws)

	got := m.toggleYoloMode()
	require.True(t, got)
	require.Equal(t, 1, ws.permSetCalls)
	readsAfterToggle := ws.permCalls
	require.Equal(t, 1, readsAfterToggle, "toggle reads the authoritative value exactly once")

	require.True(t, m.yoloModeCached(), "the new value must be served from the cache")
	require.True(t, m.yoloCache.fresh(busyCacheTTL), "write-through must stamp the cache fresh")
	m.yoloModeCached()
	require.Equal(t, readsAfterToggle, ws.permCalls, "reads after the toggle must not re-probe")

	got = m.toggleYoloMode()
	require.False(t, got)
	require.False(t, m.yoloModeCached())
}

// TestLocalYoloToggleSupersedesInFlightProbe pins the generation bump in
// toggleYoloMode: a busy/yolo probe dispatched before the toggle carries the
// old generation. Without advancing busyFetchGen its stale result would land
// with a still-matching generation and clobber the just-toggled value.
func TestLocalYoloToggleSupersedesInFlightProbe(t *testing.T) {
	pinTTLs(t)

	ws := &countingWorkspace{ready: true, yolo: false}
	m := newBusyUI(ws)
	warmCaches(m, false)

	// A busy/yolo probe carrying the pre-toggle generation is in flight.
	m.busyFetchInFlight = true
	staleGen := m.busyFetchGen

	require.True(t, m.toggleYoloMode())
	require.NotEqual(t, staleGen, m.busyFetchGen,
		"toggle must advance the busy generation to supersede in-flight probes")
	require.True(t, m.yoloModeCached(), "toggle must write the new value through the cache")

	// The stale probe (old generation, old yolo=false) lands.
	m.busyFetchInFlight = true
	cmds := m.applyBusyState(busyStateMsg{gen: staleGen, yolo: false})
	require.True(t, m.yoloModeCached(),
		"stale probe must not overwrite the freshly toggled value")
	require.NotEmpty(t, cmds, "stale probe must re-dispatch an authoritative refresh")
	require.True(t, m.busyFetchInFlight, "re-dispatched refresh must be in flight")
}

// TestSendMessageSetsOptimisticBusy pins the esc-after-enter behavior:
// submitting a prompt optimistically marks the agent busy so an immediate
// esc routes to cancelAgent instead of reading a stale idle value and doing
// nothing.
func TestSendMessageSetsOptimisticBusy(t *testing.T) {
	pinTTLs(t)

	ws := &countingWorkspace{ready: true} // workspace still reports idle
	m := newBusyUI(ws)
	warmCaches(m, false)

	require.False(t, m.isAgentBusy())
	m.sessionFilesFetchGen = 4
	m.sessionFiles = []SessionFile{{
		FirstVersion:  history.File{Path: "/workspace/current.go"},
		LatestVersion: history.File{Path: "/workspace/current.go"},
		Additions:     1,
	}}
	cmd := m.sendMessage("hello") // returned cmds (AgentRun etc.) deliberately not run
	require.NotNil(t, cmd)
	require.True(t, m.isAgentBusy(),
		"sendMessage must optimistically mark the agent busy")
	require.Len(t, m.sessionFiles, 1, "submission alone must not start a new agent-turn file scope")
	require.EqualValues(t, 4, m.sessionFilesFetchGen)

	for _, role := range []message.MessageRole{message.Assistant, message.Tool} {
		_, _ = m.Update(pubsub.Event[message.Message]{
			Type: pubsub.CreatedEvent,
			Payload: message.Message{
				ID:        string(role),
				SessionID: "s1",
				Role:      role,
			},
		})
		require.Len(t, m.sessionFiles, 1, "internal agent messages must keep the current file scope")
		require.EqualValues(t, 4, m.sessionFilesFetchGen)
	}

	_, _ = m.Update(pubsub.Event[message.Message]{
		Type: pubsub.CreatedEvent,
		Payload: message.Message{
			ID:        "next-user-turn",
			SessionID: "s1",
			Role:      message.User,
		},
	})
	require.Empty(t, m.sessionFiles, "the next top-level agent run must start a fresh file scope")
	require.EqualValues(t, 5, m.sessionFilesFetchGen)

	// esc right after enter: isAgentBusy gates cancelAgent, a single
	// press cancels immediately.
	require.Zero(t, m.promptQueue)
	m.cancelAgent()
	require.Equal(t, 1, ws.cancelCalls, "a single esc press must cancel the agent")
}

// TestCancelAgentRestoresAndClearsQueueFromCachedItems: the queue-clear
// decision and restored text must come from the memoized queue state without
// synchronous workspace probes.
func TestCancelAgentRestoresAndClearsQueueFromCachedItems(t *testing.T) {
	pinTTLs(t)

	ws := &countingWorkspace{ready: true, queued: queuedPrompts("first", "second")}
	m := newBusyUI(ws)
	warmCaches(m, true)
	m.promptQueue = 2
	m.promptQueueItems = queuedPrompts("first", "second")
	m.promptQueueDrafts = map[string]queuedPromptDraft{
		"first":  {attachments: []message.Attachment{{FileName: "first.png", Content: []byte("first image")}}},
		"second": {attachments: []message.Attachment{{FileName: "second.jpg", Content: []byte("second image")}}},
	}
	m.textarea.SetValue("draft")
	m.attachments.Update(message.Attachment{FileName: "draft.txt", Content: []byte("draft attachment")})
	ws.resetCounters()

	m.cancelAgent()
	require.Equal(t, 1, ws.clearQueueCalls, "esc with a queue must clear it")
	require.Zero(t, ws.queuedCalls, "the decision must use the cached count, not a probe")
	require.Zero(t, ws.queueListCalls, "the decision must use the cached items, not a probe")
	require.Equal(t, "first\n\nsecond\n\ndraft", m.textarea.Value())
	require.Equal(t, []string{"first.png", "second.jpg", "draft.txt"}, []string{
		m.attachments.List()[0].FileName,
		m.attachments.List()[1].FileName,
		m.attachments.List()[2].FileName,
	})
	require.Equal(t, []byte("first image"), m.attachments.List()[0].Content)
	require.Equal(t, []byte("second image"), m.attachments.List()[1].Content)
	require.Zero(t, m.promptQueue, "the cached count must be zeroed immediately")
	require.Empty(t, m.promptQueueItems)
	require.Empty(t, m.promptQueueDrafts)
	require.Zero(t, ws.cancelCalls, "clearing the queue must not cancel the active turn")
}

func TestValidateQueuedPromptAttachmentsEnforcesBounds(t *testing.T) {
	declaredImageLimit := int64(imageattachment.MaxSourceBytes + 1)
	largeContent := make([]byte, declaredImageLimit)

	require.NoError(t, validateQueuedPromptAttachments([]message.Attachment{{MimeType: "IMAGE/PNG", Content: largeContent}}, declaredImageLimit))
	require.ErrorContains(t, validateQueuedPromptAttachments([]message.Attachment{{MimeType: "image/png", Content: make([]byte, 1025)}}, 1024), "1024-byte")
	require.Error(t, validateQueuedPromptAttachments([]message.Attachment{{MimeType: "application/octet-stream", Content: largeContent}}, declaredImageLimit))
	require.Error(t, validateQueuedPromptAttachments(make([]message.Attachment, queuedPromptMaxAttachments+1), declaredImageLimit))
	require.Error(t, validateQueuedPromptAttachments([]message.Attachment{
		{Content: make([]byte, queuedPromptMaxBytes/2+1)},
		{Content: make([]byte, queuedPromptMaxBytes/2+1)},
	}, declaredImageLimit))
}

func TestApplyPromptQueueRetainsDraftsBySubmissionID(t *testing.T) {
	ws := &countingWorkspace{ready: true}
	m := newBusyUI(ws)
	m.promptQueueDrafts = map[string]queuedPromptDraft{
		"completed-id": {attachments: []message.Attachment{{FileName: "old.png"}}},
		"pending-id":   {attachments: []message.Attachment{{FileName: "pending.png"}}, expiresAt: time.Now().Add(time.Minute)},
		"same-text-id": {attachments: []message.Attachment{{FileName: "wrong-client.png"}}, expiresAt: time.Now().Add(time.Minute)},
	}

	m.applyPromptQueue(promptQueueMsg{forSession: "s1", prompts: []agent.QueuedPrompt{
		{SubmissionID: "remote-id", Prompt: "pending"},
		{SubmissionID: "pending-id", Prompt: "pending"},
	}})

	require.Equal(t, 2, m.promptQueue)
	require.Len(t, m.promptQueueDrafts, 1)
	require.Contains(t, m.promptQueueDrafts, "pending-id")
}

func TestReconcileQueuedDraftsPreservesUnconfirmedDuringAcceptanceRace(t *testing.T) {
	m := newBusyUI(&countingWorkspace{ready: true})
	now := time.Now()
	m.promptQueueDrafts = map[string]queuedPromptDraft{
		"pending-id": {
			attachments:  []message.Attachment{{FileName: "pending.png", Content: []byte("sensitive")}},
			pendingUntil: now.Add(time.Second),
			expiresAt:    now.Add(time.Minute),
		},
	}

	m.reconcileQueuedDrafts(nil, now)
	require.Contains(t, m.promptQueueDrafts, "pending-id")

	m.reconcileQueuedDrafts(nil, now.Add(2*time.Second))
	require.Empty(t, m.promptQueueDrafts)
}

func TestApplyPromptQueueExpiresAttachmentBytes(t *testing.T) {
	ws := &countingWorkspace{ready: true}
	m := newBusyUI(ws)
	m.promptQueueDrafts = map[string]queuedPromptDraft{
		"pending-id": {
			attachments: []message.Attachment{{FileName: "pending.png", Content: []byte("sensitive")}},
			expiresAt:   time.Now().Add(-time.Second),
		},
	}

	m.applyPromptQueue(promptQueueMsg{forSession: "s1", prompts: []agent.QueuedPrompt{{SubmissionID: "pending-id", Prompt: "pending"}}})

	draft := m.promptQueueDrafts["pending-id"]
	require.True(t, draft.expired)
	require.Nil(t, draft.attachments)
}

func TestCancelAgentDoesNotRestoreExpiredAttachmentBytes(t *testing.T) {
	ws := &countingWorkspace{ready: true, queued: queuedPrompts("pending")}
	m := newBusyUI(ws)
	warmCaches(m, true)
	m.promptQueue = 1
	m.promptQueueItems = queuedPrompts("pending")
	m.promptQueueDrafts = map[string]queuedPromptDraft{
		"pending": {
			attachments: []message.Attachment{{FileName: "pending.png", Content: []byte("sensitive")}},
			expiresAt:   time.Now().Add(-time.Second),
			confirmed:   true,
		},
	}

	m.cancelAgent()

	require.Equal(t, "pending", m.textarea.Value())
	require.Empty(t, m.attachments.List())
	require.Empty(t, m.promptQueueDrafts)
	require.Equal(t, 1, ws.clearQueueCalls)
}

func TestEscapeCancelsAgentWithoutClearingDraft(t *testing.T) {
	pinTTLs(t)

	ws := &countingWorkspace{ready: true, agentBusy: true}
	m := newBusyUI(ws)
	warmCaches(m, true)
	m.textarea.SetValue("keep this draft")
	ws.resetCounters()

	m.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Equal(t, 1, ws.cancelCalls)
	require.Equal(t, "keep this draft", m.textarea.Value())
	require.Zero(t, ws.clearQueueCalls)
}

// TestBackstopRefreshesStaleCaches: when the memoized state outlives its TTL
// with no event edge, the Update tail schedules exactly one off-thread
// refresh (deduplicated while in flight) and the result lands as a message.
func TestBackstopRefreshesStaleCaches(t *testing.T) {
	pinTTLs(t)

	ws := &countingWorkspace{ready: true, agentBusy: true}
	m := newBusyUI(ws)
	// Caches start at their zero value: stale by definition.

	_, cmd := m.Update(plainMsg{})
	require.True(t, m.busyFetchInFlight, "stale caches must trigger a backstop refresh")
	require.Zero(t, ws.syncProbes(), "the backstop itself must not probe synchronously")

	// A second Update while the fetch is in flight must not stack another.
	before := m.busyFetchInFlight
	m.Update(plainMsg{})
	require.Equal(t, before, m.busyFetchInFlight)
	require.Zero(t, ws.syncProbes())

	runCmds(m, cmd)
	require.False(t, m.busyFetchInFlight)
	require.True(t, m.isAgentBusy(), "the backstop result must land in the cache")
	require.Equal(t, 1, ws.agentBusyCalls, "exactly one probe per backstop refresh")

	// Freshly refreshed caches must not re-dispatch.
	m.Update(plainMsg{})
	require.False(t, m.busyFetchInFlight, "fresh caches must not re-dispatch the backstop")
}

// TestSetSessionMessagesGatesAnimationsOnBusy verifies that reloading a
// session does not start spinner animations when the agent is not busy.
// A session that was killed mid-generation can persist an assistant message
// with no Finish part, which still reports isSpinning() even though nothing
// is running. Starting animations for it would leave a ghost "working"
// spinner after the session is reloaded.
func TestSetSessionMessagesGatesAnimationsOnBusy(t *testing.T) {
	pinTTLs(t)

	ws := &countingWorkspace{ready: true, agentBusy: false}
	m := newBusyUI(ws)
	warmCaches(m, false)

	// A message that looks unfinished (no Finish part, no content).
	msgs := []message.Message{
		{
			ID:        "m1",
			SessionID: "s1",
			Role:      message.Assistant,
			Parts: []message.ContentPart{
				message.ReasoningContent{Thinking: "thinking..."},
			},
		},
	}

	// When the agent is not busy, setSessionMessages must not start animations.
	cmd := m.setSessionMessages(msgs)
	require.Nil(t, cmd, "setSessionMessages must not start animations when agent is idle")

	// When the agent is busy, animations should start.
	warmCaches(m, true)
	cmd = m.setSessionMessages(msgs)
	require.NotNil(t, cmd, "setSessionMessages must start animations when agent is busy")
}

// TestStaleBusyRefreshDiscardedAndReDispatched pins the generation guard for
// busy/permission state: a probe started before a newer state transition
// (here an optimistic busy write) must not overwrite the newer value when it
// lands, and the authoritative refresh must not be lost merely because the
// older probe was in flight — the stale result re-dispatches it.
func TestStaleBusyRefreshDiscardedAndReDispatched(t *testing.T) {
	pinTTLs(t)

	ws := &countingWorkspace{ready: true}
	m := newBusyUI(ws)
	warmCaches(m, false)

	// A busy probe is in flight; capture the generation it was dispatched
	// with, then a newer transition (optimistic send) supersedes it.
	m.busyFetchInFlight = true
	staleGen := m.busyFetchGen
	m.agentBusyCache.set(true) // optimistic busy
	m.busyFetchGen++           // newer state transition

	// The stale probe (agent reported idle) lands with the old generation.
	cmds := m.applyBusyState(busyStateMsg{gen: staleGen, agentBusy: false})
	require.True(t, m.isAgentBusy(),
		"a stale busy result must not overwrite the newer optimistic busy state")
	require.NotEmpty(t, cmds,
		"a stale busy result must re-dispatch the authoritative refresh")
	require.True(t, m.busyFetchInFlight, "the re-dispatched probe must be in flight")

	// The fresh probe (matching generation) is applied normally.
	freshGen := m.busyFetchGen
	m.applyBusyState(busyStateMsg{gen: freshGen, agentBusy: false})
	require.False(t, m.isAgentBusy(), "a current-generation result must land in the cache")
}

// TestStalePromptQueueDiscardedAndReDispatched pins the generation guard for
// the queue: a fetch started before a newer transition (here a queue clear)
// must not repopulate the cleared queue, and it must re-dispatch the
// authoritative fetch instead of being applied.
func TestStalePromptQueueDiscardedAndReDispatched(t *testing.T) {
	pinTTLs(t)

	ws := &countingWorkspace{ready: true, queued: queuedPrompts("real")}
	m := newBusyUI(ws)
	warmCaches(m, false)
	m.promptQueue = 1
	m.promptQueueItems = queuedPrompts("real")

	// A fetch is in flight; capture its generation, then a newer transition
	// (esc clears the queue) supersedes it.
	m.promptQueueInFlight = true
	staleGen := m.promptQueueGen
	m.invalidatePromptQueue()
	m.promptQueue = 0
	m.promptQueueItems = nil

	// The stale fetch (still saw one prompt) lands for the same session.
	cmds := m.applyPromptQueue(promptQueueMsg{
		forSession: "s1",
		gen:        staleGen,
		prompts:    queuedPrompts("stale"),
	})
	require.Zero(t, m.promptQueue,
		"a stale queue result must not repopulate the cleared queue")
	require.Empty(t, m.promptQueueItems)
	require.NotEmpty(t, cmds,
		"a stale queue result must re-dispatch the authoritative fetch")
	require.True(t, m.promptQueueInFlight, "the re-dispatched fetch must be in flight")
}

// TestStalePromptQueuePreservesSessionScoping pins that the generation guard
// does not weaken session scoping: a fetch scoped to a different session is
// still discarded and re-fetched even when its generation would otherwise
// match.
func TestStalePromptQueuePreservesSessionScoping(t *testing.T) {
	pinTTLs(t)

	ws := &countingWorkspace{ready: true}
	m := newBusyUI(ws) // active session "s1"
	warmCaches(m, false)
	m.promptQueueInFlight = true
	gen := m.promptQueueGen

	cmds := m.applyPromptQueue(promptQueueMsg{
		forSession: "other",
		gen:        gen,
		prompts:    queuedPrompts("from other session"),
	})
	require.Zero(t, m.promptQueue,
		"a result from a different session must never populate the queue")
	require.NotEmpty(t, cmds, "a session-mismatched result must re-fetch for the current session")
}

// TestRenderHelpersDoNotProbeWorkspace pins the render-path side of the
// invariant for the model and LSP info: selectedLargeModel, lspInfo, and
// lspErrorCount render from memoized state only. They run on every frame
// (landing view, sidebar, compact header), and the probes behind them
// (AgentIsReady, AgentModel, LSPGetStates, LSPGetDiagnosticCounts) are
// synchronous HTTP round-trips in client/server mode.
func TestRenderHelpersDoNotProbeWorkspace(t *testing.T) {
	pinTTLs(t)

	ws := &countingWorkspace{ready: true}
	m := newBusyUI(ws)
	m.agentReady = true
	m.lspStates = map[string]workspace.LSPClientInfo{
		"gopls": {Name: "gopls", State: lsp.StateReady, DiagnosticCount: 3},
	}
	m.lspDiagnostics = map[string]lsp.DiagnosticCounts{
		"gopls": {Error: 2, Warning: 1},
	}

	for range 10 {
		require.NotNil(t, m.selectedLargeModel())
		m.lspInfo(40, 5, true)
		require.Equal(t, 3, m.lspErrorCount())
	}

	// modelInfo reaches provider config only through the memoized model;
	// with the agent not ready it renders the empty state.
	m.agentReady = false
	for range 10 {
		m.modelInfo(40)
	}

	require.Zero(t, ws.syncProbes(), "render helpers must never probe the workspace")
}

// TestBusyRefreshCarriesReadyAndModel: the off-thread busy probe must also
// deliver the coordinator's readiness and selected model so the sidebar and
// landing view render them without per-frame probes.
func TestBusyRefreshCarriesReadyAndModel(t *testing.T) {
	pinTTLs(t)

	ws := &countingWorkspace{
		ready: true,
		model: workspace.AgentModel{ModelCfg: config.SelectedModel{Model: "test-model", Provider: "prov"}},
	}
	m := newBusyUI(ws)
	require.Nil(t, m.selectedLargeModel(), "before any probe the model is unknown")

	_, cmd := m.Update(plainMsg{}) // stale caches: the backstop dispatches
	runCmds(m, cmd)

	require.True(t, m.agentReady, "the probe must land readiness in the cache")
	sel := m.selectedLargeModel()
	require.NotNil(t, sel)
	require.Equal(t, "test-model", sel.ModelCfg.Model, "the probe must land the model in the cache")
}

// TestAgentModelChangedRefreshesModel: after a model change
// (selection/thinking/reasoning cmds sequence agentModelChangedCmd), the
// handler must re-fetch ready/model off-thread — no synchronous probe — and
// the fresh model must replace the memoized one.
func TestAgentModelChangedRefreshesModel(t *testing.T) {
	pinTTLs(t)

	ws := &countingWorkspace{
		ready: true,
		model: workspace.AgentModel{ModelCfg: config.SelectedModel{Model: "new-model"}},
	}
	m := newBusyUI(ws)
	warmCaches(m, false)
	m.agentModel = workspace.AgentModel{ModelCfg: config.SelectedModel{Model: "old-model"}}
	ws.resetCounters()

	_, cmd := m.Update(agentModelChangedMsg{})
	require.Zero(t, ws.syncProbes(), "the model-change handler must not probe synchronously")
	require.True(t, m.busyFetchInFlight, "a model change must schedule a ready/model refresh")

	runCmds(m, cmd)
	require.Equal(t, "new-model", m.agentModel.ModelCfg.Model,
		"the refreshed model must land in the cache")
}

// TestMCPStateChangedRefreshesModel pins the fourth UpdateAgentModel call
// site: an MCP state change rebuilds the agent, which can change the
// effective model, so the memoized ready/model state must be re-fetched
// off-thread afterwards — the edge the updateAgentModelCmd helper exists to
// make unforgettable.
func TestMCPStateChangedRefreshesModel(t *testing.T) {
	pinTTLs(t)

	ws := &countingWorkspace{
		ready: true,
		model: workspace.AgentModel{ModelCfg: config.SelectedModel{Model: "post-mcp-model"}},
	}
	m := newBusyUI(ws)
	warmCaches(m, false)
	m.agentModel = workspace.AgentModel{ModelCfg: config.SelectedModel{Model: "pre-mcp-model"}}
	ws.resetCounters()

	// handleStateChanged sequences the rebuild with agentModelChangedCmd;
	// tea.Sequence's wrapper msg is unexported, so drive the two steps the
	// way the runtime would: run the cmd (the stub records the call), then
	// deliver the invalidation message.
	_ = m.handleStateChanged()()
	_, cmd := m.Update(agentModelChangedMsg{})
	require.True(t, m.busyFetchInFlight, "an MCP state change must schedule a ready/model refresh")
	runCmds(m, cmd)

	require.True(t, m.agentReady)
	require.Equal(t, "post-mcp-model", m.agentModel.ModelCfg.Model,
		"an MCP state change must refresh the memoized model")
}

// TestLSPEventRefreshIsOffThreadAndDeduped pins the LSP side of the
// invariant: an LSP event must not fetch states synchronously in Update
// (LSPGetStates + per-server LSPGetDiagnosticCounts are HTTP round-trips in
// client/server mode, and diagnostics events arrive per edited file). It
// schedules one off-thread fetch, dedups while one is in flight, and
// re-dispatches a queued refresh when the in-flight fetch lands.
func TestLSPEventRefreshIsOffThreadAndDeduped(t *testing.T) {
	pinTTLs(t)

	ws := &countingWorkspace{
		ready:     true,
		lspStates: map[string]workspace.LSPClientInfo{"gopls": {Name: "gopls", DiagnosticCount: 3}},
		lspDiags:  map[string]lsp.DiagnosticCounts{"gopls": {Error: 2, Warning: 1}},
	}
	m := newBusyUI(ws)
	warmCaches(m, false)
	ws.resetCounters()

	_, cmd := m.Update(pubsub.Event[workspace.LSPEvent]{
		Payload: workspace.LSPEvent{Type: workspace.LSPEventDiagnosticsChanged, Name: "gopls"},
	})
	require.Zero(t, ws.syncProbes(), "the LSP event handler must not probe synchronously")
	require.True(t, m.lspFetchInFlight, "an LSP event must schedule an off-thread refresh")

	// A second event while the fetch is in flight queues a re-fetch instead
	// of stacking another dispatch.
	m.Update(pubsub.Event[workspace.LSPEvent]{
		Payload: workspace.LSPEvent{Type: workspace.LSPEventDiagnosticsChanged, Name: "gopls"},
	})
	require.Zero(t, ws.syncProbes())
	require.True(t, m.lspRefreshQueued, "an event during an in-flight fetch must queue a re-fetch")

	runCmds(m, cmd)
	require.False(t, m.lspFetchInFlight)
	require.False(t, m.lspRefreshQueued, "the queued flag must clear once the re-dispatched fetch lands")
	require.Equal(t, 3, m.lspStates["gopls"].DiagnosticCount, "fetched states must land in the cache")
	require.Equal(t, 2, m.lspDiagnostics["gopls"].Error, "fetched severity counts must land in the cache")
	require.Equal(t, 3, m.lspErrorCount())
	require.Equal(t, 2, ws.lspStateCalls, "one fetch plus the queued re-fetch")
}

func TestLSPStateRefreshRebuildsSidebarCache(t *testing.T) {
	pinTTLs(t)

	m := newBusyUI(&countingWorkspace{ready: true})
	m.layout.sidebar = image.Rect(0, 0, 42, 45)
	m.updateSidebarScrollState()
	require.NotContains(t, ansi.Strip(m.sidebarContent), "gopls")

	m.lspFetchInFlight = true
	m.applyLSPStates(lspStatesMsg{
		states: map[string]workspace.LSPClientInfo{
			"gopls": {Name: "gopls", State: lsp.StateReady},
		},
		diagnostics: map[string]lsp.DiagnosticCounts{"gopls": {}},
	})
	require.Contains(t, ansi.Strip(m.sidebarContent), "gopls")
}

func TestProviderUsageRefreshRebuildsSidebarCacheAndRejectsStaleResults(t *testing.T) {
	pinTTLs(t)

	m := newBusyUI(&countingWorkspace{ready: true})
	m.layout.sidebar = image.Rect(0, 0, 42, 45)
	m.sidebarLogo = "logo"
	m.usageFetchGen = 2
	m.updateSidebarScrollState()
	require.NotContains(t, ansi.Strip(m.sidebarDrawLogo), "5h")

	current := &oauthusage.Usage{ProviderID: "codex", Windows: []oauthusage.Window{{Name: "5h", Percent: 75}}}
	_, _ = m.Update(usageUpdatedMsg{gen: 2, usage: current})
	require.Same(t, current, m.providerUsage)
	require.Contains(t, ansi.Strip(m.sidebarDrawLogo), "5h")
	require.Contains(t, ansi.Strip(m.sidebarDrawLogo), "█████")

	stale := &oauthusage.Usage{ProviderID: "codex", Windows: []oauthusage.Window{{Name: "stale", Percent: 10}}}
	_, _ = m.Update(usageUpdatedMsg{gen: 1, usage: stale})
	require.Same(t, current, m.providerUsage)
	require.NotContains(t, ansi.Strip(m.sidebarDrawLogo), "stale")
}

func TestSessionFileRefreshRebuildsSidebarCacheAndRejectsStaleResults(t *testing.T) {
	pinTTLs(t)

	m := newBusyUI(&countingWorkspace{ready: true})
	m.layout.sidebar = image.Rect(0, 0, 42, 45)
	m.updateSidebarScrollState()
	m.sessionFilesFetchGen = 2

	_, _ = m.Update(sessionFilesUpdatesMsg{
		sessionID:  "s1",
		generation: 2,
		sessionFiles: []SessionFile{{
			FirstVersion:  history.File{Path: "/workspace/current.go"},
			LatestVersion: history.File{Path: "/workspace/current.go"},
			Additions:     3,
		}},
	})
	require.Contains(t, ansi.Strip(m.sidebarContent), "current.go")

	_, _ = m.Update(sessionFilesUpdatesMsg{
		sessionID:  "s1",
		generation: 1,
		sessionFiles: []SessionFile{{
			FirstVersion:  history.File{Path: "/workspace/stale.go"},
			LatestVersion: history.File{Path: "/workspace/stale.go"},
			Additions:     1,
		}},
	})
	sidebar := ansi.Strip(m.sidebarContent)
	require.Contains(t, sidebar, "current.go")
	require.NotContains(t, sidebar, "stale.go")
}

// TestRemoteYoloToggleUpdatesEditorPrompt pins the second fix: when an
// asynchronous busy-state refresh reports a yolo mode different from the
// cached one (a remote toggle), applyBusyState must update the textarea
// prompt function too, not just the cache — otherwise the prompt icon/style
// keeps rendering the old mode.
func TestPlanModeUsesDistinctEditorPromptAndPreservesBangPrecedence(t *testing.T) {
	pinTTLs(t)

	m := newBusyUI(&countingWorkspace{ready: true})
	m.textarea.Focus()
	m.textarea.SetWidth(40)
	m.session = &session.Session{Mode: session.ModePlan}
	m.setEditorPrompt(true)
	planPrompt := ansi.Strip(m.textarea.View())
	require.Contains(t, planPrompt, "PLAN")
	require.NotContains(t, planPrompt, " Y ")

	m.bangMode = true
	m.setEditorPrompt(true)
	bangPrompt := ansi.Strip(m.textarea.View())
	require.Contains(t, bangPrompt, "!")
	require.NotContains(t, bangPrompt, "PLAN")
}

func TestShiftTabTogglesPlanModeBothWays(t *testing.T) {
	ws := &countingWorkspace{ready: true}
	m := newBusyUI(ws)
	shiftTab := tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}

	runCmds(m, m.handleKeyPressMsg(shiftTab))
	require.Equal(t, 1, ws.modeCalls)
	require.Equal(t, session.ModePlan, ws.mode)
	require.Equal(t, session.ModePlan, m.session.Mode)

	runCmds(m, m.handleKeyPressMsg(shiftTab))
	require.Equal(t, 2, ws.modeCalls)
	require.Equal(t, session.ModeDefault, ws.mode)
	require.Equal(t, session.ModeDefault, m.session.Mode)
}

func TestShiftTabCannotBypassPlanCompletionApproval(t *testing.T) {
	ws := &countingWorkspace{ready: true}
	m := newBusyUI(ws)
	m.session.Mode = session.ModePlanExecution
	shiftTab := tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}

	runCmds(m, m.handleKeyPressMsg(shiftTab))
	require.Zero(t, ws.modeCalls)
	require.Equal(t, session.ModePlanExecution, m.session.Mode)
}

func TestRemoteYoloToggleUpdatesEditorPrompt(t *testing.T) {
	pinTTLs(t)

	ws := &countingWorkspace{ready: true}
	m := newBusyUI(ws)
	m.textarea.Focus()
	m.textarea.SetWidth(40)
	m.yoloCache.set(false)
	m.setEditorPrompt(false)
	normalPrompt := ansi.Strip(m.textarea.View())

	// A remote toggle flips yolo on; delivered via an off-thread refresh.
	m.applyBusyState(busyStateMsg{gen: m.busyFetchGen, yolo: true})
	require.True(t, m.yoloModeCached(), "the refresh must write the new yolo value through the cache")
	yoloPrompt := ansi.Strip(m.textarea.View())
	require.NotEqual(t, normalPrompt, yoloPrompt,
		"a remote yolo toggle must change the rendered editor prompt")
	require.Contains(t, yoloPrompt, "Y",
		"the yolo prompt icon must render after a remote toggle")

	// Flipping back off must restore the normal prompt.
	m.applyBusyState(busyStateMsg{gen: m.busyFetchGen, yolo: false})
	require.False(t, m.yoloModeCached())
	require.Equal(t, normalPrompt, ansi.Strip(m.textarea.View()),
		"toggling yolo off must restore the normal editor prompt")
}
