package backend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/agent"
	"github.com/example-git/crux/internal/app"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/imageattachment"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/skills"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// blockingCoordinator is a minimal agent.Coordinator whose RunAccepted
// blocks until release is closed. It records that RunAccepted was
// entered so tests can observe the dispatched goroutine. Every other
// method returns a zero value.
type blockingCoordinator struct {
	entered       chan struct{}
	release       chan struct{}
	parts         chan []message.ContentPart
	materializers chan agent.InputMaterializer
	attachments   chan []message.Attachment
	runCount      atomic.Int32
}

func newBlockingCoordinator() *blockingCoordinator {
	return &blockingCoordinator{
		entered:       make(chan struct{}, 1),
		release:       make(chan struct{}),
		parts:         make(chan []message.ContentPart, 1),
		materializers: make(chan agent.InputMaterializer, 1),
		attachments:   make(chan []message.Attachment, 1),
	}
}

func (c *blockingCoordinator) Run(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	return nil, nil
}

func (c *blockingCoordinator) RunAccepted(ctx context.Context, accept *agent.AcceptedRun, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	c.runCount.Add(1)
	select {
	case c.parts <- agent.InputPartsFromContext(ctx):
	default:
	}
	select {
	case c.materializers <- agent.InputMaterializerFromContext(ctx):
	default:
	}
	select {
	case c.attachments <- append([]message.Attachment(nil), attachments...):
	default:
	}
	select {
	case c.entered <- struct{}{}:
	default:
	}
	<-c.release
	return nil, nil
}

func (c *blockingCoordinator) BeginAccepted(sessionID string) *agent.AcceptedRun { return nil }
func (c *blockingCoordinator) Cancel(string)                                     {}
func (c *blockingCoordinator) CancelAll()                                        {}
func (c *blockingCoordinator) IsBusy() bool                                      { return false }
func (c *blockingCoordinator) IsSessionBusy(string) bool                         { return false }
func (c *blockingCoordinator) QueuedPrompts(string) int                          { return 0 }
func (c *blockingCoordinator) QueuedPromptsList(string) []agent.QueuedPrompt     { return nil }
func (c *blockingCoordinator) ClearQueue(string)                                 {}
func (c *blockingCoordinator) Summarize(context.Context, string) error           { return nil }
func (c *blockingCoordinator) Model() agent.Model                                { return agent.Model{} }
func (c *blockingCoordinator) UpdateModels(context.Context) error                { return nil }
func (c *blockingCoordinator) UpdateModelsForState(context.Context, config.AgentModelState) error {
	return nil
}
func (c *blockingCoordinator) GenerateTitle(context.Context, string, string) {}

func (c *blockingCoordinator) SuggestPrompt(context.Context, string) (string, error) { return "", nil }

type admissionCoordinator struct {
	*blockingCoordinator
	invokeCallback bool
	callbackErr    error
	runErr         error
}

func (c *admissionCoordinator) RunAccepted(ctx context.Context, _ *agent.AcceptedRun, _, _ string, _ ...message.Attachment) (*fantasy.AgentResult, error) {
	c.runCount.Add(1)
	select {
	case c.entered <- struct{}{}:
	default:
	}
	if c.invokeCallback {
		agent.AdmissionCallbackFromContext(ctx)(c.callbackErr)
	}
	if c.runErr != nil {
		return nil, c.runErr
	}
	<-c.release
	return nil, nil
}

// insertAgentWorkspace installs a synthetic workspace with the given
// coordinator (or none) and a workspace run context, mirroring the
// fields CreateWorkspace initializes.
func insertAgentWorkspace(t *testing.T, b *Backend, coord agent.Coordinator) *Workspace {
	t.Helper()
	root := t.TempDir()
	a := app.NewForTest(b.ctx)
	a.AgentCoordinator = coord
	t.Cleanup(a.ShutdownForTest)
	ws := &Workspace{
		App:          a,
		ID:           uuid.New().String(),
		Path:         root,
		resolvedPath: root,
		clients:      make(map[string]*clientState),
		shutdownFn:   func() {},
	}
	ws.ctx, ws.cancel = context.WithCancel(b.ctx)
	b.mu.Lock()
	b.workspaces.Set(ws.ID, ws)
	b.pathIndex[ws.resolvedPath] = ws.ID
	b.mu.Unlock()
	return ws
}

func TestSendMessage_WorkspaceNotFound(t *testing.T) {
	t.Parallel()
	b, _ := newTestBackend(t)
	err := b.SendMessage("nope", proto.AgentMessage{SessionID: "S1", Prompt: "hi"})
	require.ErrorIs(t, err, ErrWorkspaceNotFound)
}

func TestSendMessage_AgentNotInitialized(t *testing.T) {
	t.Parallel()
	b, _ := newTestBackend(t)
	ws := insertAgentWorkspace(t, b, nil)
	err := b.SendMessage(ws.ID, proto.AgentMessage{SessionID: "S1", Prompt: "hi"})
	require.ErrorIs(t, err, ErrAgentNotInitialized)
}

func TestSendMessage_EmptyPrompt(t *testing.T) {
	t.Parallel()
	b, _ := newTestBackend(t)
	ws := insertAgentWorkspace(t, b, newBlockingCoordinator())
	err := b.SendMessage(ws.ID, proto.AgentMessage{SessionID: "S1", Prompt: ""})
	require.ErrorIs(t, err, agent.ErrEmptyPrompt)
}

func TestSendMessage_MaterializesWorkspaceImagesInOrder(t *testing.T) {
	t.Parallel()
	content := []byte("\x89PNG\r\n\x1a\nfixture")
	for _, absolute := range []bool{false, true} {
		name := "relative"
		if absolute {
			name = "absolute"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b, _ := newTestBackend(t)
			coord := newBlockingCoordinator()
			ws := insertAgentWorkspace(t, b, coord)
			imagePath := filepath.Join(ws.resolvedPath, "pixel.png")
			require.NoError(t, os.WriteFile(imagePath, content, 0o600))
			requestedPath := "pixel.png"
			if absolute {
				requestedPath = imagePath
			}

			err := b.SendMessage(ws.ID, proto.AgentMessage{
				SessionID: "S1",
				InputParts: []proto.AgentInputPart{
					{Type: "text", Text: "before"},
					{Type: "local_file", Path: requestedPath},
					{Type: "text", Text: "after"},
				},
				PermissionMode: proto.AgentPermissionInteractive,
			})
			require.NoError(t, err)
			parts := <-coord.parts
			require.Equal(t, message.TextContent{Text: "before"}, parts[0])
			require.Equal(t, message.BinaryContent{Path: requestedPath, MIMEType: "image/png", Data: content}, parts[1])
			require.Equal(t, message.TextContent{Text: "after"}, parts[2])
			close(coord.release)
			ws.runWG.Wait()
		})
	}
}

func TestSendMessageMaterializesSelectedSkillInInputOrder(t *testing.T) {
	t.Parallel()
	b, _ := newTestBackend(t)
	coord := newBlockingCoordinator()
	ws := insertAgentWorkspace(t, b, coord)
	skillPath := filepath.Join(ws.resolvedPath, "fixture", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(skillPath), 0o700))
	content := []byte("---\nname: fixture\ndescription: Fixture skill.\n---\n\nUse the fixture workflow.\n")
	require.NoError(t, os.WriteFile(skillPath, content, 0o600))
	skill := &skills.Skill{
		Name:          "fixture",
		Description:   "Fixture skill.",
		UserInvocable: true,
		SkillFilePath: skillPath,
	}
	ws.Skills = skills.NewManager([]*skills.Skill{skill}, []*skills.Skill{skill}, nil)

	err := b.SendMessage(ws.ID, proto.AgentMessage{
		SessionID: "S1",
		InputParts: []proto.AgentInputPart{
			{Type: "text", Text: "before"},
			{Type: "skill", Text: "fixture", Path: skillPath},
			{Type: "text", Text: "after"},
		},
		PermissionMode: proto.AgentPermissionInteractive,
	})
	require.NoError(t, err)
	require.Equal(t, []message.ContentPart{
		message.TextContent{Text: "before"},
		message.SkillContent{Name: "fixture", Path: skillPath, Content: content},
		message.TextContent{Text: "after"},
	}, <-coord.parts)
	require.Empty(t, <-coord.attachments)
	close(coord.release)
	ws.runWG.Wait()
}

func TestSendMessageRejectsInvalidSkillsBeforeAdmission(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		part  proto.AgentInputPart
		match string
	}{
		{name: "missing path", part: proto.AgentInputPart{Type: "skill", Text: "fixture"}, match: "skill path must not be empty"},
		{name: "missing name", part: proto.AgentInputPart{Type: "skill", Path: "fixture/SKILL.md"}, match: "skill name must not be empty"},
		{name: "unavailable", part: proto.AgentInputPart{Type: "skill", Text: "fixture", Path: "fixture/SKILL.md"}, match: "skill not found"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			b, _ := newTestBackend(t)
			coord := newBlockingCoordinator()
			ws := insertAgentWorkspace(t, b, coord)
			ws.Skills = skills.NewManager(nil, nil, nil)
			err := b.SendMessage(ws.ID, proto.AgentMessage{
				SessionID:      "S1",
				InputParts:     []proto.AgentInputPart{test.part},
				PermissionMode: proto.AgentPermissionInteractive,
			})
			require.ErrorContains(t, err, test.match)
			require.Zero(t, coord.runCount.Load())
		})
	}

	t.Run("name mismatch", func(t *testing.T) {
		t.Parallel()
		b, _ := newTestBackend(t)
		coord := newBlockingCoordinator()
		ws := insertAgentWorkspace(t, b, coord)
		skillPath := filepath.Join(ws.resolvedPath, "fixture", "SKILL.md")
		require.NoError(t, os.MkdirAll(filepath.Dir(skillPath), 0o700))
		require.NoError(t, os.WriteFile(skillPath, []byte("fixture"), 0o600))
		skill := &skills.Skill{Name: "fixture", SkillFilePath: skillPath}
		ws.Skills = skills.NewManager([]*skills.Skill{skill}, []*skills.Skill{skill}, nil)
		err := b.SendMessage(ws.ID, proto.AgentMessage{
			SessionID:      "S1",
			InputParts:     []proto.AgentInputPart{{Type: "skill", Text: "different", Path: skillPath}},
			PermissionMode: proto.AgentPermissionInteractive,
		})
		require.ErrorContains(t, err, "does not match selected skill")
		require.Zero(t, coord.runCount.Load())
	})
}

func TestSendMessageDefersSteerInputMaterializationUntilAfterAdmission(t *testing.T) {
	t.Parallel()
	b, _ := newTestBackend(t)
	coord := newBlockingCoordinator()
	ws := insertAgentWorkspace(t, b, coord)

	err := b.SendMessage(ws.ID, proto.AgentMessage{
		SessionID:      "S1",
		DeliveryMode:   "steer",
		InputParts:     []proto.AgentInputPart{{Type: "local_file", Path: "missing.png"}},
		PermissionMode: proto.AgentPermissionInteractive,
	})
	require.NoError(t, err)
	var materializer agent.InputMaterializer
	select {
	case materializer = <-coord.materializers:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatched steer did not provide an input materializer")
	}
	require.NotNil(t, materializer)
	_, _, err = materializer(t.Context())
	require.ErrorContains(t, err, "open local file")
	close(coord.release)
	ws.runWG.Wait()
}

func TestSendMessageRejectsMalformedSteerInputBeforeAdmission(t *testing.T) {
	t.Parallel()
	b, _ := newTestBackend(t)
	coord := newBlockingCoordinator()
	ws := insertAgentWorkspace(t, b, coord)

	err := b.SendMessage(ws.ID, proto.AgentMessage{
		SessionID:      "S1",
		DeliveryMode:   "steer",
		InputParts:     []proto.AgentInputPart{{Type: "local_file"}},
		PermissionMode: proto.AgentPermissionInteractive,
	})
	require.ErrorContains(t, err, "local file path must not be empty")
	require.Zero(t, coord.runCount.Load())
}

func TestSendMessageRejectsInvalidWorkspaceImagesBeforeAdmission(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		part  func(*testing.T, *Workspace) proto.AgentInputPart
		match string
	}{
		{name: "missing", part: func(t *testing.T, ws *Workspace) proto.AgentInputPart {
			return proto.AgentInputPart{Type: "local_file", Path: "missing.png"}
		}, match: "open local file"},
		{name: "directory", part: func(t *testing.T, ws *Workspace) proto.AgentInputPart {
			return proto.AgentInputPart{Type: "local_file", Path: "."}
		}, match: "not a regular file"},
		{name: "traversal", part: func(t *testing.T, ws *Workspace) proto.AgentInputPart {
			path := filepath.Join(filepath.Dir(ws.resolvedPath), filepath.Base(ws.resolvedPath)+"-outside.png")
			require.NoError(t, os.WriteFile(path, []byte("\x89PNG\r\n\x1a\nfixture"), 0o600))
			t.Cleanup(func() { require.NoError(t, os.Remove(path)) })
			return proto.AgentInputPart{Type: "local_file", Path: filepath.Join("..", filepath.Base(path))}
		}, match: "outside the workspace"},
		{name: "symlink escape", part: func(t *testing.T, ws *Workspace) proto.AgentInputPart {
			outside := filepath.Join(t.TempDir(), "outside.png")
			require.NoError(t, os.WriteFile(outside, []byte("\x89PNG\r\n\x1a\nfixture"), 0o600))
			link := filepath.Join(ws.resolvedPath, "link.png")
			require.NoError(t, os.Symlink(outside, link))
			return proto.AgentInputPart{Type: "local_file", Path: "link.png"}
		}, match: "outside the workspace"},
		{name: "non image", part: func(t *testing.T, ws *Workspace) proto.AgentInputPart {
			path := filepath.Join(ws.resolvedPath, "note.txt")
			require.NoError(t, os.WriteFile(path, []byte("plain text"), 0o600))
			return proto.AgentInputPart{Type: "local_file", Path: "note.txt"}
		}, match: "not an image"},
		{name: "oversized", part: func(t *testing.T, ws *Workspace) proto.AgentInputPart {
			path := filepath.Join(ws.resolvedPath, "large.png")
			require.NoError(t, os.WriteFile(path, []byte("\x89PNG\r\n\x1a\n"), 0o600))
			require.NoError(t, os.Truncate(path, imageattachment.MaxSourceBytes+1))
			return proto.AgentInputPart{Type: "local_file", Path: "large.png"}
		}, match: "exceeds"},
		{name: "empty path", part: func(t *testing.T, ws *Workspace) proto.AgentInputPart {
			return proto.AgentInputPart{Type: "local_file"}
		}, match: "must not be empty"},
		{name: "unsupported type", part: func(t *testing.T, ws *Workspace) proto.AgentInputPart {
			return proto.AgentInputPart{Type: "remote_image"}
		}, match: "unsupported agent input part type"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			b, _ := newTestBackend(t)
			coord := newBlockingCoordinator()
			ws := insertAgentWorkspace(t, b, coord)
			err := b.SendMessage(ws.ID, proto.AgentMessage{
				SessionID:      "S1",
				InputParts:     []proto.AgentInputPart{test.part(t, ws)},
				PermissionMode: proto.AgentPermissionInteractive,
			})
			require.ErrorContains(t, err, test.match)
			require.Zero(t, coord.runCount.Load())
		})
	}
}

func TestSendMessage_SessionMissing(t *testing.T) {
	t.Parallel()
	b, _ := newTestBackend(t)
	ws := insertAgentWorkspace(t, b, newBlockingCoordinator())
	err := b.SendMessage(ws.ID, proto.AgentMessage{SessionID: "", Prompt: "hi"})
	require.ErrorIs(t, err, agent.ErrSessionMissing)
}

func TestSendMessage_WorkspaceClosing(t *testing.T) {
	t.Parallel()
	b, _ := newTestBackend(t)
	ws := insertAgentWorkspace(t, b, newBlockingCoordinator())
	ws.runMu.Lock()
	ws.closing = true
	ws.runMu.Unlock()
	err := b.SendMessage(ws.ID, proto.AgentMessage{SessionID: "S1", Prompt: "hi", PermissionMode: proto.AgentPermissionInteractive})
	require.ErrorIs(t, err, ErrWorkspaceClosing)
}

// TestSendMessage_SuccessIncrementsRunWG asserts the happy path returns
// nil synchronously and dispatches a tracked goroutine: while
// RunAccepted blocks, runWG.Wait must not complete (the ticket is
// outstanding); after release it drains.
func TestSendMessage_SuccessIncrementsRunWG(t *testing.T) {
	t.Parallel()
	b, _ := newTestBackend(t)
	coord := newBlockingCoordinator()
	ws := insertAgentWorkspace(t, b, coord)

	err := b.SendMessage(ws.ID, proto.AgentMessage{SessionID: "S1", Prompt: "hi", PermissionMode: proto.AgentPermissionInteractive})
	require.NoError(t, err)

	select {
	case <-coord.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatched goroutine never entered RunAccepted")
	}
	require.Equal(t, int32(1), coord.runCount.Load())

	waited := make(chan struct{})
	go func() {
		ws.runWG.Wait()
		close(waited)
	}()

	select {
	case <-waited:
		t.Fatal("runWG.Wait completed while the run was still in flight; ticket was not added")
	case <-time.After(100 * time.Millisecond):
	}

	close(coord.release)

	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("runWG.Wait did not complete after the run returned")
	}
}

func TestSendMessageWaitsForAdmissionWithoutWaitingForRunCompletion(t *testing.T) {
	b, _ := newTestBackend(t)
	coord := &admissionCoordinator{blockingCoordinator: newBlockingCoordinator(), invokeCallback: true}
	ws := insertAgentWorkspace(t, b, coord)

	err := b.SendMessage(ws.ID, proto.AgentMessage{
		SessionID:        "S1",
		Prompt:           "hi",
		PermissionMode:   proto.AgentPermissionInteractive,
		WaitForAdmission: true,
	})
	require.NoError(t, err)
	require.Equal(t, int32(1), coord.runCount.Load())

	waited := make(chan struct{})
	go func() {
		ws.runWG.Wait()
		close(waited)
	}()
	select {
	case <-waited:
		t.Fatal("SendMessage waited for run completion instead of admission")
	case <-time.After(100 * time.Millisecond):
	}
	close(coord.release)
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not complete after release")
	}
}

func TestSendMessageReturnsPreAdmissionRunError(t *testing.T) {
	b, _ := newTestBackend(t)
	expected := errors.New("admission rejected")
	coord := &admissionCoordinator{blockingCoordinator: newBlockingCoordinator(), runErr: expected}
	ws := insertAgentWorkspace(t, b, coord)

	err := b.SendMessage(ws.ID, proto.AgentMessage{
		SessionID:        "S1",
		Prompt:           "hi",
		PermissionMode:   proto.AgentPermissionInteractive,
		WaitForAdmission: true,
	})
	require.ErrorIs(t, err, expected)
	ws.runWG.Wait()
}
