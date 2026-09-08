package automemory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWorkerMaintainsSingleProjectUnderPressureBeforeExtraction(t *testing.T) {
	memory := managedTestMemory(t)
	seedMemorySlots(t, memory, ProjectMemorySlots)
	calls := make(chan string, 4)
	worker, err := NewWorker(WorkerOptions{
		Memory: memory,
		Generate: func(ctx context.Context, purpose, prompt string, _ int64) (string, error) {
			calls <- purpose
			if purpose == "memory_consolidation" {
				require.Contains(t, prompt, "50/50 occupied")
				require.Contains(t, prompt, "actively merge overlapping topics toward 40")
				return `{"memories":[{"file":"topic-000.md","action":"upsert","name":"Consolidated","description":"Combined important requirements","type":"project","content":"Important detail 0. Important detail 1."},{"file":"topic-001.md","action":"delete"}]}`, nil
			}
			return `{"memories":[{"file":"new.md","action":"upsert","name":"New requirement","description":"Important new requirement","type":"project","content":"New durable detail."}]}`, nil
		},
		LoadTranscript: func(context.Context, string) ([]Turn, error) {
			return []Turn{{Role: "user", Text: "Retain this requirement."}}, nil
		},
		LoadSessions: func(context.Context) ([]SessionInfo, error) { return nil, nil },
	})
	require.NoError(t, err)
	t.Cleanup(worker.Close)
	worker.Enqueue("single-session")
	for _, expected := range []string{"memory_consolidation", "memory_extraction"} {
		select {
		case purpose := <-calls:
			require.Equal(t, expected, purpose)
		case <-time.After(5 * time.Second):
			t.Fatal("background maintenance cycle did not run")
		}
	}
	require.Eventually(t, func() bool { _, err := os.Stat(filepath.Join(memory.Directory, "new.md")); return err == nil }, 5*time.Second, 10*time.Millisecond)
	worker.Close()
	require.NoFileExists(t, filepath.Join(memory.Directory, "topic-001.md"))
	content, err := os.ReadFile(filepath.Join(memory.Directory, "topic-000.md"))
	require.NoError(t, err)
	require.Contains(t, string(content), "Important detail 0. Important detail 1.")
}

func TestMemoryMaintenanceRotatesWholeFilesAndRejectsStaleWrites(t *testing.T) {
	memory := managedTestMemory(t)
	for i := range 6 {
		body := fmt.Sprintf("---\nname: Topic %d\ndescription: Important details\ntype: project\n---\n\n%sFINAL-%d", i, strings.Repeat("detail ", 1500), i)
		require.NoError(t, os.WriteFile(filepath.Join(memory.Directory, fmt.Sprintf("topic-%d.md", i)), []byte(body), 0o600))
	}
	worker := &Worker{memory: memory}
	first, reviewed, next, err := worker.memoryContext(25_000)
	require.NoError(t, err)
	require.Len(t, reviewed, 2)
	require.Contains(t, first, "FINAL-0")
	require.Contains(t, first, "FINAL-1")
	require.NotContains(t, first, "topic-2.md")
	require.Equal(t, "topic-2.md", next)
	require.NoError(t, os.WriteFile(filepath.Join(memory.Directory, ".consolidate-next"), []byte(next), 0o600))
	second, _, next, err := worker.memoryContext(25_000)
	require.NoError(t, err)
	require.Contains(t, second, "FINAL-2")
	require.Contains(t, second, "FINAL-3")
	require.Equal(t, "topic-4.md", next)
	require.ErrorContains(t, applyMemoryMutations(memory, []memoryMutation{{File: "topic-5.md", Action: "delete"}}, reviewed), "not fully reviewed")
	require.NoError(t, os.WriteFile(filepath.Join(memory.Directory, "topic-0.md"), []byte("newer user update"), 0o600))
	require.ErrorContains(t, applyMemoryMutations(memory, []memoryMutation{{File: "topic-0.md", Action: "delete"}}, reviewed), "changed during maintenance")
	require.FileExists(t, filepath.Join(memory.Directory, "topic-0.md"))
}

func TestMemoryMaintenancePressureCadence(t *testing.T) {
	for _, count := range []int{5, 40, 51} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			memory := managedTestMemory(t)
			seedMemorySlots(t, memory, count)
			now := time.Now().Add(time.Hour)
			calls := 0
			worker, err := NewWorker(WorkerOptions{
				Memory: memory, Now: func() time.Time { return now },
				Generate:       func(context.Context, string, string, int64) (string, error) { calls++; return `{"memories":[]}`, nil },
				LoadTranscript: func(context.Context, string) ([]Turn, error) { return nil, nil },
				LoadSessions:   func(context.Context) ([]SessionInfo, error) { return nil, nil },
			})
			require.NoError(t, err)
			t.Cleanup(worker.Close)
			require.NoError(t, worker.maybeDream(t.Context(), "only-session"))
			require.Equal(t, 1, calls)
			require.NoError(t, worker.maybeDream(t.Context(), "only-session"))
			require.Equal(t, 1, calls)
			if count >= 40 {
				interval := time.Hour
				if count > ProjectMemorySlots {
					interval = 10 * time.Minute
				}
				now = now.Add(interval)
				require.NoError(t, worker.maybeDream(t.Context(), "only-session"))
				require.Equal(t, 2, calls)
			}
		})
	}
}
