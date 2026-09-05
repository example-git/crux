package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/permission"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/example-git/crux/internal/shell"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/stretchr/testify/require"
)

type mockBashPermissionService struct {
	*pubsub.Broker[permission.PermissionRequest]
}

func (m *mockBashPermissionService) Request(ctx context.Context, req permission.CreatePermissionRequest) (bool, error) {
	return true, nil
}

func (m *mockBashPermissionService) Grant(req permission.PermissionRequest) bool { return true }

func (m *mockBashPermissionService) Deny(req permission.PermissionRequest) bool { return true }

func (m *mockBashPermissionService) GrantPersistent(req permission.PermissionRequest) bool {
	return true
}

func (m *mockBashPermissionService) AutoApproveSession(sessionID string) {}

func (m *mockBashPermissionService) SetSkipRequests(skip bool) {}

func (m *mockBashPermissionService) SkipRequests() bool {
	return false
}

func (m *mockBashPermissionService) SubscribeNotifications(ctx context.Context) <-chan pubsub.Event[permission.PermissionNotification] {
	return make(<-chan pubsub.Event[permission.PermissionNotification])
}

func TestBashTool_DefaultAutoBackgroundThreshold(t *testing.T) {
	workingDir := t.TempDir()
	tool := newBashToolForTest(workingDir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	resp := runBashTool(t, tool, ctx, BashParams{
		Description: "default threshold",
		Command:     "echo done",
	})

	require.False(t, resp.IsError)
	var meta BashResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.False(t, meta.Background)
	require.Empty(t, meta.ShellID)
	require.Contains(t, meta.Output, "done")
}

func TestBashToolSubagentPolicy(t *testing.T) {
	workingDir := t.TempDir()
	tool, manager := newBashToolAndManagerForTest(workingDir)
	ctx := context.WithValue(permission.WithSubagent(t.Context()), SessionIDContextKey, "child-session")

	t.Run("foreground remains allowed", func(t *testing.T) {
		response := runBashTool(t, tool, ctx, BashParams{Command: "echo allowed"})
		require.False(t, response.IsError)
		require.Contains(t, response.Content, "allowed")
		require.Zero(t, manager.ActiveCount())
	})

	t.Run("background is rejected", func(t *testing.T) {
		response := runBashTool(t, tool, ctx, BashParams{Command: "echo blocked", RunInBackground: true})
		require.True(t, response.IsError)
		require.Equal(t, permission.ErrSubagentBackgroundTask.Error(), response.Content)
		require.Zero(t, manager.ActiveCount())
	})
}

func TestBashTool_TimeoutKillsCommand(t *testing.T) {
	workingDir := t.TempDir()
	tool := newBashToolForTest(workingDir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	resp := runBashTool(t, tool, ctx, BashParams{
		Description: "timeout kill",
		Command:     "sleep 1.5 && echo done",
		Timeout:     1,
	})

	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "timed out")
}

func TestBashTool_ExplicitBackgroundLifecycle(t *testing.T) {
	workingDir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "child-session")
	ctx = managedtask.WithOwnership(ctx, managedtask.Ownership{ParentSessionID: "parent-session", OwnerAgentTaskID: "a12345678", OriginToolCallID: "agent-call"})

	t.Run("quick completion stays synchronous", func(t *testing.T) {
		tool, manager := newBashToolAndManagerForTest(workingDir)
		notifications := manager.SubscribeNotifications(t.Context())
		response := runBashTool(t, tool, ctx, BashParams{
			Description:     "quick background",
			Command:         "echo done",
			RunInBackground: true,
		})
		var metadata BashResponseMetadata
		require.NoError(t, json.Unmarshal([]byte(response.Metadata), &metadata))
		require.True(t, metadata.Background)
		require.Empty(t, metadata.ShellID)
		select {
		case event := <-notifications:
			t.Fatalf("quick synchronous completion emitted notification: %#v", event.Payload)
		default:
		}
	})

	t.Run("running command returns task and notifies", func(t *testing.T) {
		tool, manager := newBashToolAndManagerForTest(workingDir)
		notifications := manager.SubscribeNotifications(t.Context())
		response := runBashTool(t, tool, ctx, BashParams{
			Description:     "long background",
			Command:         "sleep 2.2; exit 7",
			RunInBackground: true,
		})
		var metadata BashResponseMetadata
		require.NoError(t, json.Unmarshal([]byte(response.Metadata), &metadata))
		require.True(t, metadata.Background)
		require.NotEmpty(t, metadata.ShellID)
		backgroundShell, ok := manager.Get(metadata.ShellID)
		require.True(t, ok)
		require.Equal(t, "parent-session", backgroundShell.Ownership.ParentSessionID)
		require.Equal(t, "a12345678", backgroundShell.Ownership.OwnerAgentTaskID)
		require.Equal(t, "test-call", backgroundShell.Ownership.OriginToolCallID)
		select {
		case event := <-notifications:
			require.Equal(t, metadata.ShellID, event.Payload.TaskID)
			require.Equal(t, "failed", string(event.Payload.Status))
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for explicit background completion")
		}
	})
}

func TestBashTool_CtrlBDetachesAndNotifies(t *testing.T) {
	workingDir := t.TempDir()
	tool, manager := newBashToolAndManagerForTest(workingDir)
	notifications := manager.SubscribeNotifications(t.Context())
	parentCtx, cancelParent := context.WithCancel(context.Background())
	ctx := context.WithValue(parentCtx, SessionIDContextKey, "child-session")
	ctx = managedtask.WithOwnership(ctx, managedtask.Ownership{ParentSessionID: "parent-session", OwnerAgentTaskID: "a12345678", OriginToolCallID: "agent-call"})
	input, err := json.Marshal(BashParams{Description: "detach", Command: "sleep 10"})
	require.NoError(t, err)
	type toolResult struct {
		response fantasy.ToolResponse
		err      error
	}
	resultChannel := make(chan toolResult, 1)
	go func() {
		response, runErr := tool.Run(ctx, fantasy.ToolCall{ID: "detach-call", Name: BashToolName, Input: string(input)})
		resultChannel <- toolResult{response: response, err: runErr}
	}()

	require.Eventually(t, func() bool {
		return manager.DetachForeground() == 1
	}, 2*time.Second, 20*time.Millisecond)
	result := <-resultChannel
	require.NoError(t, result.err)
	response := result.response
	var metadata BashResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(response.Metadata), &metadata))
	require.True(t, metadata.Background)
	require.NotEmpty(t, metadata.ShellID)
	backgroundShell, ok := manager.Get(metadata.ShellID)
	require.True(t, ok)
	require.Equal(t, "parent-session", backgroundShell.Ownership.ParentSessionID)
	require.Equal(t, "a12345678", backgroundShell.Ownership.OwnerAgentTaskID)
	require.Equal(t, "detach-call", backgroundShell.Ownership.OriginToolCallID)
	cancelParent()
	time.Sleep(25 * time.Millisecond)
	require.False(t, backgroundShell.State().Status.Terminal())
	_, err = manager.Stop(t.Context(), metadata.ShellID)
	require.NoError(t, err)
	select {
	case event := <-notifications:
		require.Equal(t, metadata.ShellID, event.Payload.TaskID)
		require.Equal(t, "killed", string(event.Payload.Status))
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for detached shell notification")
	}
}

type recordingPermissionService struct {
	*pubsub.Broker[permission.PermissionRequest]
	requestCount int
	lastRequest  permission.CreatePermissionRequest
	allow        bool
}

func (m *recordingPermissionService) Request(ctx context.Context, req permission.CreatePermissionRequest) (bool, error) {
	m.requestCount++
	m.lastRequest = req
	return m.allow, nil
}

func (m *recordingPermissionService) Grant(req permission.PermissionRequest) bool { return true }

func (m *recordingPermissionService) Deny(req permission.PermissionRequest) bool { return true }

func (m *recordingPermissionService) GrantPersistent(req permission.PermissionRequest) bool {
	return true
}

func (m *recordingPermissionService) AutoApproveSession(sessionID string) {}

func (m *recordingPermissionService) SetSkipRequests(skip bool) {}

func (m *recordingPermissionService) SkipRequests() bool {
	return false
}

func (m *recordingPermissionService) SubscribeNotifications(ctx context.Context) <-chan pubsub.Event[permission.PermissionNotification] {
	return make(<-chan pubsub.Event[permission.PermissionNotification])
}

func newBashToolForTest(workingDir string) fantasy.AgentTool {
	tool, _ := newBashToolAndManagerForTest(workingDir)
	return tool
}

func newBashToolAndManagerForTest(workingDir string) (fantasy.AgentTool, *shell.BackgroundShellManager) {
	permissions := &mockBashPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest]()}
	manager := shell.NewBackgroundShellManager(workingDir)
	return NewBashTool(manager, permissions, workingDir), manager
}

func newBashToolWithRecordingPerms(workingDir string, allow bool) (fantasy.AgentTool, *recordingPermissionService) {
	perms := &recordingPermissionService{
		Broker: pubsub.NewBroker[permission.PermissionRequest](),
		allow:  allow,
	}
	return NewBashTool(shell.NewBackgroundShellManager(workingDir), perms, workingDir), perms
}

func TestBashTool_ChainedCommandsRequirePermission(t *testing.T) {
	workingDir := t.TempDir()
	tool, perms := newBashToolWithRecordingPerms(workingDir, true)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	// ls && echo should trigger permission check.
	resp := runBashTool(t, tool, ctx, BashParams{
		Description: "chained ls",
		Command:     "ls && echo done",
	})

	require.False(t, resp.IsError)
	require.Equal(t, 1, perms.requestCount, "chained command should trigger permission request")

	// Plain ls should NOT trigger permission check.
	perms.requestCount = 0
	resp = runBashTool(t, tool, ctx, BashParams{
		Description: "plain ls",
		Command:     "ls -la",
	})

	require.False(t, resp.IsError)
	require.Equal(t, 0, perms.requestCount, "plain ls should not trigger permission request")
}

func TestBashTool_ChainedCommandsDenied(t *testing.T) {
	workingDir := t.TempDir()
	tool, perms := newBashToolWithRecordingPerms(workingDir, false)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	resp := runBashTool(t, tool, ctx, BashParams{
		Description: "chained ls denied",
		Command:     "ls && rm -rf /",
	})

	require.Equal(t, 1, perms.requestCount)
	require.Contains(t, resp.Content, "User denied permission")
}

func TestBashToolUnsafeSyntaxDeniedBeforeExecution(t *testing.T) {
	for _, command := range []string{
		"echo safe\njq . %q",
		"echo safe & jq . %q",
		"echo <(jq . %q)",
		"echo $(jq . %q)",
		"echo replaced > %q",
		"echo-unknown %q",
		"env jq . %q",
		"nice jq . %q",
		"nohup jq . %q",
		"timeout 1 jq . %q",
	} {
		t.Run(command, func(t *testing.T) {
			outside := filepath.Join(t.TempDir(), "private.json")
			original := []byte(`{"value":"private-fixture-content"}`)
			require.NoError(t, os.WriteFile(outside, original, 0o600))
			tool, permissions := newBashToolWithRecordingPerms(t.TempDir(), false)
			ctx := context.WithValue(t.Context(), SessionIDContextKey, "test-session")
			response := runBashTool(t, tool, ctx, BashParams{
				Description: "denied script",
				Command:     fmt.Sprintf(command, outside),
			})
			require.Equal(t, 1, permissions.requestCount)
			require.Contains(t, response.Content, "User denied permission")
			require.NotContains(t, response.Content, "private-fixture-content")
			data, err := os.ReadFile(outside)
			require.NoError(t, err)
			require.Equal(t, original, data)
		})
	}
}

func runBashTool(t *testing.T, tool fantasy.AgentTool, ctx context.Context, params BashParams) fantasy.ToolResponse {
	t.Helper()

	input, err := json.Marshal(params)
	require.NoError(t, err)

	call := fantasy.ToolCall{
		ID:    "test-call",
		Name:  BashToolName,
		Input: string(input),
	}

	resp, err := tool.Run(ctx, call)
	require.NoError(t, err)
	return resp
}

func TestTruncateOutputValidUTF8(t *testing.T) {
	t.Parallel()
	// CJK characters are 2 cells wide; this string is far wider than
	// MaxOutputLength so TruncateOutput must truncate it.
	content := strings.Repeat("你好世界", MaxOutputLength)

	out := TruncateOutput(content)
	require.True(t, utf8.ValidString(out), "truncated output must stay valid UTF-8")
	require.Contains(t, out, "lines truncated")
}

func TestTruncateOutputShortContent(t *testing.T) {
	t.Parallel()
	content := "short output"
	require.Equal(t, content, TruncateOutput(content))
}

func TestTruncateOutputEmoji(t *testing.T) {
	t.Parallel()
	// Emoji with ZWJ sequences should not be split.
	content := strings.Repeat("👨‍👩‍👧‍👦", MaxOutputLength)

	out := TruncateOutput(content)
	require.True(t, utf8.ValidString(out), "truncated output must stay valid UTF-8")
	require.Contains(t, out, "lines truncated")
}
