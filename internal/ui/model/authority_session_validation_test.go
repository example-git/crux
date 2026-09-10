package model

import (
	"context"
	"testing"

	"github.com/charmbracelet/x/ansi"
	tea "github.com/example-git/crux/foundation/bubbletea"
	"github.com/example-git/crux/internal/agent"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/history"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/session"
	"github.com/example-git/crux/internal/ui/util"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

type authoritySessionWorkspace struct {
	*countingWorkspace
	t               *testing.T
	id              string
	io              bool
	reads, presence []string
	messages        map[string][]message.Message
	authority       *config.RemoteAuthority
}

func (w *authoritySessionWorkspace) AuthenticationWorkspaceID() string          { return w.id }
func (w *authoritySessionWorkspace) AcceptedAuthority() *config.RemoteAuthority { return w.authority }

func (w *authoritySessionWorkspace) Config() *config.Config {
	return &config.Config{Options: &config.Options{TUI: &config.TUIOptions{}}, Models: map[config.SelectedModelType]config.SelectedModel{config.SelectedModelTypeLarge: {Provider: "exact-provider"}}}
}

func (w *authoritySessionWorkspace) GetSession(_ context.Context, id string) (session.Session, error) {
	require.True(w.t, w.io, "session IO outside Cmd")
	w.reads = append(w.reads, "session:"+id)
	return session.Session{ID: id, Title: w.id}, nil
}

func (w *authoritySessionWorkspace) ListSessionHistory(context.Context, string) ([]history.File, error) {
	require.True(w.t, w.io, "file IO outside Cmd")
	return nil, nil
}

func (w *authoritySessionWorkspace) FileTrackerListReadFiles(context.Context, string) ([]string, error) {
	require.True(w.t, w.io, "read-file IO outside Cmd")
	return nil, nil
}

func (w *authoritySessionWorkspace) ListMessages(_ context.Context, id string) ([]message.Message, error) {
	require.True(w.t, w.io, "message IO outside Cmd")
	w.reads = append(w.reads, "messages:"+id)
	return w.messages[id], nil
}

func (w *authoritySessionWorkspace) ListUserMessages(_ context.Context, id string) ([]message.Message, error) {
	require.True(w.t, w.io, "prompt-history IO outside Cmd")
	w.reads = append(w.reads, "prompts:"+id)
	return w.messages[id], nil
}

func (w *authoritySessionWorkspace) ListAllUserMessages(context.Context) ([]message.Message, error) {
	require.True(w.t, w.io, "prompt-history IO outside Cmd")
	w.reads = append(w.reads, "prompts:all")
	return nil, nil
}

func (w *authoritySessionWorkspace) SetCurrentSession(_ context.Context, id string) error {
	require.True(w.t, w.io, "presence IO outside Cmd")
	w.presence = append(w.presence, id)
	return nil
}

func (w *authoritySessionWorkspace) CreateAgentToolSessionID(messageID, callID string) string {
	return messageID + "$$" + callID
}

func newAuthoritySessionUI(t *testing.T, id string) (*UI, *authoritySessionWorkspace) {
	t.Helper()
	base := &countingWorkspace{}
	ui := newBusyUI(base)
	ws := &authoritySessionWorkspace{countingWorkspace: base, t: t, id: id, messages: map[string][]message.Message{}}
	ui.com.Workspace = ws
	ui.focus = uiFocusNone
	warmCaches(ui, false)
	return ui, ws
}

func authoritySessionLoad(t *testing.T, ws *authoritySessionWorkspace, command tea.Cmd) loadSessionMsg {
	t.Helper()
	ws.io = true
	defer func() { ws.io = false }()
	value, ok := command().(loadSessionMsg)
	require.True(t, ok)
	return value
}

func TestAuthoritySessionValidationConnectionSource(t *testing.T) {
	ui, current := newAuthoritySessionUI(t, "new-id")
	_, previous := newAuthoritySessionUI(t, "new-id")
	ui.status.SetInfoMsg(util.InfoMsg{Msg: "current workspace state"})
	for _, event := range []workspace.ConnectionEvent{
		{Source: previous, WorkspaceID: "new-id", State: workspace.ConnectionRecovered},
		{Source: previous, WorkspaceID: "new-id", State: workspace.ConnectionDegraded, Stuck: true},
		{Source: current, WorkspaceID: "old-id", State: workspace.ConnectionRecovered},
		{WorkspaceID: "new-id", State: workspace.ConnectionRecovered},
	} {
		require.Empty(t, ui.handleConnectionEvent(event))
		require.Equal(t, "current workspace state", ui.status.msg.Msg)
		require.Zero(t, ui.sessionLoadGeneration)
	}
	commands := ui.handleConnectionEvent(workspace.ConnectionEvent{Source: current, WorkspaceID: "new-id", PreviousWorkspaceID: "old-id", Recreated: true, State: workspace.ConnectionRecovered})
	require.Len(t, commands, 2)
	require.EqualValues(t, 1, ui.sessionLoadGeneration)
	require.Contains(t, ui.status.msg.Msg, "New workspace runtime acknowledged")
	require.Contains(t, ui.status.msg.Msg, "Reloading the saved session")
	require.NotContains(t, ui.status.msg.Msg, "history restored")
	require.Empty(t, current.reads, "connection Update only dispatches commands")
	commands = ui.handleConnectionEvent(workspace.ConnectionEvent{Source: current, WorkspaceID: "new-id", State: workspace.ConnectionRecovered})
	require.Len(t, commands, 2)
	require.Contains(t, ui.status.msg.Msg, "Reattached to the existing workspace")
}

func TestAuthoritySessionValidationCapturedLoadAndStaleReply(t *testing.T) {
	ui, original := newAuthoritySessionUI(t, "original-id")
	oldSession := ui.session
	command := ui.loadSession("old-session")
	_, replacement := newAuthoritySessionUI(t, "replacement-id")
	ui.com.Workspace = replacement
	loaded := authoritySessionLoad(t, original, command)
	require.Equal(t, "original-id", loaded.workspaceID)
	require.Equal(t, []string{"session:old-session", "messages:old-session"}, original.reads)
	require.Empty(t, replacement.reads)
	require.Empty(t, original.presence)
	_, _ = ui.Update(loaded)
	require.Same(t, oldSession, ui.session)
	ui.com.Workspace = original
	original.id = "recreated-id"
	_, _ = ui.Update(loaded)
	require.Same(t, oldSession, ui.session)
	command = ui.loadSession("new-session")
	current := authoritySessionLoad(t, original, command)
	_, _ = ui.Update(loaded)
	require.Same(t, oldSession, ui.session)
	_, follow := ui.Update(current)
	require.Equal(t, "new-session", ui.session.ID)
	require.Equal(t, "recreated-id", ui.session.Title)
	require.Empty(t, original.presence, "presence is dispatched after validated load")
	original.io = true
	runCmds(ui, follow)
	original.io = false
	require.Equal(t, []string{"new-session"}, original.presence)
}

func TestAuthoritySessionValidationNestedReadsStayInCommand(t *testing.T) {
	ui, ws := newAuthoritySessionUI(t, "captured-id")
	parent := message.Message{ID: "parent", Role: message.Assistant}
	parent.SetToolCalls([]message.ToolCall{{ID: "agent-call", Name: agent.AgentToolName, Input: "{}", Finished: true}})
	ws.messages["selected"] = []message.Message{parent}
	ws.messages["parent$$agent-call"] = []message.Message{{ID: "child", Role: message.Assistant}}
	before := ui.session
	result := authoritySessionLoad(t, ws, ui.loadSession("selected"))
	require.NoError(t, result.err)
	require.Len(t, result.nested["parent$$agent-call"], 1)
	require.Equal(t, []string{"session:selected", "messages:selected", "messages:parent$$agent-call"}, ws.reads)
	require.Same(t, before, ui.session, "Cmd must not mutate UI model")
	require.Empty(t, ws.presence)
}

func TestAuthoritySessionValidationAcceptedLabels(t *testing.T) {
	ui, ws := newAuthoritySessionUI(t, "display-id")
	ws.authority = &config.RemoteAuthority{Mode: "client", Principal: "synthetic-principal", Revision: 7, Digest: "not-a-credential", Accounts: []config.RemoteAccountIdentity{{ProviderID: "other", AccountID: "wrong-account"}, {ProviderID: "exact-provider", AccountID: "chosen\x1b[31m-account\n", Generation: 3}}}
	for _, width := range []int{36, 96} {
		header := ansi.Strip(renderHeaderDetails(ui.com, ui.session, 0, false, width, nil))
		require.Contains(t, header, "Client providers · r7")
		details := ansi.Strip(ui.workspaceAuthorityInfo(width))
		require.Contains(t, details, "Credentials: owning client")
		require.Contains(t, details, "Selected account: chosen-account")
		require.NotContains(t, details, "wrong-account")
	}
	ws.authority = &config.RemoteAuthority{Mode: "server", Principal: "synthetic-principal"}
	require.Equal(t, "Server providers", workspaceAuthorityLabel(ws.authority))
	require.Contains(t, ansi.Strip(ui.workspaceAuthorityInfo(96)), "Credentials: execution server")
	require.Empty(t, ws.reads)
	require.Empty(t, ws.presence)
}

func TestAuthoritySessionValidationPromptHistoryScope(t *testing.T) {
	ui, original := newAuthoritySessionUI(t, "original-id")
	original.messages["s1"] = []message.Message{{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "original prompt"}}}}
	command := ui.loadPromptHistory()
	_, replacement := newAuthoritySessionUI(t, "replacement-id")
	ui.com.Workspace = replacement
	original.io = true
	result := command().(promptHistoryLoadedMsg)
	original.io = false
	require.Equal(t, []string{"prompts:s1"}, original.reads)
	require.Empty(t, replacement.reads)
	ui.promptHistory.messages = []string{"current prompt"}
	_, _ = ui.Update(result)
	require.Equal(t, []string{"current prompt"}, ui.promptHistory.messages)
	ui.com.Workspace = original
	original.id = "recreated-id"
	_, _ = ui.Update(result)
	require.Equal(t, []string{"current prompt"}, ui.promptHistory.messages)
	original.id = "original-id"
	ui.session = &session.Session{ID: "other-session"}
	_, _ = ui.Update(result)
	require.Equal(t, []string{"current prompt"}, ui.promptHistory.messages)
	ui.session = &session.Session{ID: "s1"}
	newer := ui.loadPromptHistory()
	_, _ = ui.Update(result)
	require.Equal(t, []string{"current prompt"}, ui.promptHistory.messages)
	original.io = true
	current := newer().(promptHistoryLoadedMsg)
	original.io = false
	_, _ = ui.Update(current)
	require.Equal(t, []string{"original prompt"}, ui.promptHistory.messages)
}

func TestAuthoritySessionValidationInitialAndFileReplies(t *testing.T) {
	ui, current := newAuthoritySessionUI(t, "current-id")
	_, old := newAuthoritySessionUI(t, "current-id")
	ui.initialPrompt = "retained prompt"
	ui.state = uiLanding
	ui.sessionLoadGeneration = 2
	for _, source := range []workspace.Workspace{old, current} {
		id := "current-id"
		if source == current {
			id = "old-id"
		}
		_, _ = ui.Update(initialSessionUnavailableMsg{source: source, workspaceID: id, generation: 2})
		require.Equal(t, "retained prompt", ui.initialPrompt)
		_, _ = ui.Update(sessionFilesUpdatesMsg{source: source, workspaceID: id, sessionID: "s1", sessionFiles: []SessionFile{{LatestVersion: history.File{Path: "stale.go"}}}})
		require.Empty(t, ui.sessionFiles)
	}
	_, command := ui.Update(initialSessionUnavailableMsg{source: current, workspaceID: "current-id", generation: 2})
	require.Empty(t, ui.initialPrompt)
	require.NotNil(t, command)
}
