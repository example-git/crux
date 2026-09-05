package task

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStoreRejectsOperationsAfterClose(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "metadata"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	record := Record{
		ID:        "a12345678",
		Type:      TypeAgent,
		Ownership: Ownership{WorkspaceID: "workspace", ParentSessionID: "parent"},
		State:     StateToRecord(State{Status: StatusPending}),
		Agent:     &AgentRecord{Prompt: "first", AgentType: "task"},
	}
	require.NoError(t, store.Put(record))
	require.NoError(t, store.Close())
	require.ErrorIs(t, store.Put(record), os.ErrClosed)
	_, err = store.Get(record.ID)
	require.ErrorIs(t, err, os.ErrClosed)
	_, err = store.List()
	require.ErrorIs(t, err, os.ErrClosed)
	_, err = store.MarkNotificationRead("notification")
	require.ErrorIs(t, err, os.ErrClosed)
	_, err = store.MarkNotificationDelivered("notification")
	require.ErrorIs(t, err, os.ErrClosed)
	require.ErrorIs(t, store.Remove(record.ID), os.ErrClosed)
	require.FileExists(t, filepath.Join(store.root, recordName(record.ID)))
}

func TestStoreRoundTripAndOrdering(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "metadata"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	endedAt := time.Now().Truncate(time.Millisecond)
	exitCode := 7
	shell := Record{
		ID:          "b12345678",
		Type:        TypeShell,
		Description: "shell",
		Ownership:   Ownership{WorkspaceID: "workspace", ParentSessionID: "parent", OriginToolCallID: "call"},
		State:       StateToRecord(State{Status: StatusFailed, EndedAt: endedAt, ExitCode: &exitCode, ErrorCode: "execution_failed"}),
		OutputRef:   "task-output:b12345678",
		Shell:       &ShellRecord{Command: "exit 7", WorkingDirectory: "/workspace", Backgrounded: true},
	}
	agent := Record{
		ID:          "a12345678",
		Type:        TypeAgent,
		Description: "agent",
		Ownership:   Ownership{WorkspaceID: "workspace", ParentSessionID: "parent", OriginToolCallID: "call"},
		State:       StateToRecord(State{Status: StatusCompleted, EndedAt: endedAt}),
		OutputRef:   "session:child",
		Agent: &AgentRecord{
			Prompt:         "review",
			AgentType:      "reviewer",
			ChildSessionID: "child",
			ContinuationOf: "a87654321",
			FinalOutput:    "done",
			UsageBaseline:  AgentUsage{PromptTokens: 1, CompletionTokens: 1, Cost: 0.02},
			Usage:          AgentUsage{PromptTokens: 3, CompletionTokens: 2, Cost: 0.1, ToolUseCount: 1},
		},
	}
	image := Record{
		ID:          "i12345678",
		Type:        TypeImage,
		Description: "image",
		Ownership:   Ownership{WorkspaceID: "workspace", ParentSessionID: "parent", OriginToolCallID: "image-call"},
		State:       StateToRecord(State{Status: StatusCompleted, EndedAt: endedAt}),
		OutputRef:   "file:/workspace/image.png",
		Image: &ImageRecord{
			Mode:        "generate",
			Backend:     "flow",
			Prompt:      "draw a fox",
			Count:       1,
			OutputPaths: []string{"/workspace/image.png"},
			FinalOutput: `{"success":true}`,
		},
	}
	require.NoError(t, store.Put(shell))
	require.NoError(t, store.Put(agent))
	require.NoError(t, store.Put(image))

	recovered, err := store.Get(shell.ID)
	require.NoError(t, err)
	require.Equal(t, shell.ID, recovered.ID)
	require.Equal(t, shell.Shell, recovered.Shell)
	require.Equal(t, StateFromRecord(shell.State), StateFromRecord(recovered.State))
	recoveredImage, err := store.Get(image.ID)
	require.NoError(t, err)
	require.Equal(t, "flow", recoveredImage.Image.Backend)
	records, err := store.List()
	require.NoError(t, err)
	require.Equal(t, []string{agent.ID, shell.ID, image.ID}, []string{records[0].ID, records[1].ID, records[2].ID})
}

func TestStoreReplacesAtomicallyAndRejectsInvalidRecords(t *testing.T) {
	root := filepath.Join(t.TempDir(), "metadata")
	store, err := NewStore(root)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	record := Record{
		ID:        "a12345678",
		Type:      TypeAgent,
		Ownership: Ownership{WorkspaceID: "workspace", ParentSessionID: "parent"},
		State:     StateToRecord(State{Status: StatusPending}),
		Agent:     &AgentRecord{Prompt: "first", AgentType: "task"},
	}
	require.NoError(t, store.Put(record))
	record.Agent.Prompt = "second"
	require.NoError(t, store.Put(record))
	recovered, err := store.Get(record.ID)
	require.NoError(t, err)
	require.Equal(t, "second", recovered.Agent.Prompt)

	invalid := record
	invalid.Type = TypeShell
	require.ErrorContains(t, store.Put(invalid), "does not match")
	invalid = record
	invalid.Ownership.ParentSessionID = ""
	require.ErrorContains(t, store.Put(invalid), "requires workspace and parent session")
	invalid = record
	invalid.Agent.ContinuationOf = "b12345678"
	require.ErrorContains(t, store.Put(invalid), "invalid agent continuation task ID")

	require.NoError(t, os.WriteFile(filepath.Join(root, "a87654321.task.json"), []byte("{"), 0o600))
	_, err = store.List()
	require.ErrorContains(t, err, "decoding task metadata")
}

func TestStoreNotificationFiltersAndDurableMarkers(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "metadata"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	endedAt := time.Now().Truncate(time.Millisecond)
	record := Record{
		ID:        "a12345678",
		Type:      TypeAgent,
		Ownership: Ownership{WorkspaceID: "workspace", ParentSessionID: "parent"},
		State:     StateToRecord(State{Status: StatusCompleted, EndedAt: endedAt}),
		OutputRef: "session:child",
		Agent:     &AgentRecord{Prompt: "review", AgentType: "task", ChildSessionID: "child"},
		Notification: &Notification{
			ID:              "notification",
			TaskID:          "a12345678",
			TaskType:        TypeAgent,
			WorkspaceID:     "workspace",
			ParentSessionID: "parent",
			Status:          StatusCompleted,
			EndedAt:         endedAt,
			OutputRef:       "session:child",
		},
	}
	require.NoError(t, store.Put(record))

	notifications, err := store.ListNotifications("workspace", "parent", true, true)
	require.NoError(t, err)
	require.Len(t, notifications, 1)
	require.Empty(t, notifications[0].ReadAt)
	require.Empty(t, notifications[0].ModelDeliveredAt)

	read, err := store.MarkNotificationRead("notification")
	require.NoError(t, err)
	require.False(t, read.ReadAt.IsZero())
	notifications, err = store.ListNotifications("workspace", "parent", true, false)
	require.NoError(t, err)
	require.Empty(t, notifications)

	delivered, err := store.MarkNotificationDelivered("notification")
	require.NoError(t, err)
	require.False(t, delivered.ModelDeliveredAt.IsZero())
	notifications, err = store.ListNotifications("workspace", "parent", false, true)
	require.NoError(t, err)
	require.Empty(t, notifications)

	_, err = store.MarkNotificationRead("missing")
	require.ErrorContains(t, err, "not found")
}

func TestStoreRejectsSymlinkRecord(t *testing.T) {
	root := filepath.Join(t.TempDir(), "metadata")
	store, err := NewStore(root)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	target := filepath.Join(t.TempDir(), "target")
	require.NoError(t, os.WriteFile(target, []byte("{}"), 0o600))
	if err := os.Symlink(target, filepath.Join(root, "a12345678.task.json")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_, err = store.Get("a12345678")
	require.Error(t, err)
}
