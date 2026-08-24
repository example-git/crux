package automemory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func managedTestMemory(t *testing.T) Memory {
	t.Helper()
	directory := t.TempDir()
	return Memory{
		Directory:  directory,
		Entrypoint: filepath.Join(directory, EntrypointName),
		Managed:    true,
	}
}

func TestWorkerExtractsDurableMemoryAndSkipsUnchangedTranscript(t *testing.T) {
	memory := managedTestMemory(t)
	require.NoError(t, os.WriteFile(filepath.Join(memory.Directory, "testing.md"), []byte("---\nname: Focused validation\ndescription: focused tests with bounded execution\ntype: feedback\n---\n\nKeep the existing timeout rationale."), 0o600))
	var calls atomic.Int32
	worker, err := NewWorker(WorkerOptions{
		Memory: memory,
		Generate: func(_ context.Context, purpose, prompt string, maxOutputTokens int64) (string, error) {
			require.Equal(t, "memory_extraction", purpose)
			require.Contains(t, prompt, "user: Always run focused tests")
			require.Contains(t, prompt, "Before proposing any mutation, inspect the target project scope's existing memory manifest")
			require.Contains(t, prompt, "Full content for related memories")
			require.Contains(t, prompt, "Keep the existing timeout rationale.")
			require.Contains(t, prompt, "roughly 30 to 50 memories")
			require.Contains(t, prompt, "soft target, not a hard limit")
			require.EqualValues(t, 4096, maxOutputTokens)
			calls.Add(1)
			return `{"memories":[{"file":"testing.md","action":"upsert","name":"Focused validation","description":"Use bounded focused tests","type":"feedback","content":"Keep the existing timeout rationale. Run focused package tests with explicit timeouts."}]}`, nil
		},
		LoadTranscript: func(context.Context, string) ([]Turn, error) {
			return []Turn{{Role: "user", Text: "Always run focused tests"}, {Role: "assistant", Text: "Understood"}}, nil
		},
		LoadSessions: func(context.Context) ([]SessionInfo, error) { return nil, nil },
	})
	require.NoError(t, err)
	t.Cleanup(worker.Close)

	require.NoError(t, worker.extract(t.Context(), "session"))
	require.NoError(t, worker.extract(t.Context(), "session"))
	require.EqualValues(t, 1, calls.Load())
	topic, err := os.ReadFile(filepath.Join(memory.Directory, "testing.md"))
	require.NoError(t, err)
	require.Contains(t, string(topic), "type: feedback")
	require.Contains(t, string(topic), "Keep the existing timeout rationale.")
	index, err := os.ReadFile(memory.Entrypoint)
	require.NoError(t, err)
	require.Equal(t, "- [Focused validation](testing.md) - Use bounded focused tests\n", string(index))
}

func TestWorkerEnqueueCoalescesToLatestPendingSession(t *testing.T) {
	memory := managedTestMemory(t)
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	generated := make(chan string, 3)
	worker, err := NewWorker(WorkerOptions{
		Memory: memory,
		Generate: func(ctx context.Context, _ string, prompt string, _ int64) (string, error) {
			generated <- prompt
			if len(generated) == 1 {
				close(firstStarted)
				select {
				case <-releaseFirst:
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}
			return `{"memories":[]}`, nil
		},
		LoadTranscript: func(_ context.Context, sessionID string) ([]Turn, error) {
			return []Turn{{Role: "user", Text: "session " + sessionID}}, nil
		},
		LoadSessions: func(context.Context) ([]SessionInfo, error) { return nil, nil },
	})
	require.NoError(t, err)
	t.Cleanup(worker.Close)

	worker.Enqueue("first")
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first extraction did not start")
	}
	worker.Enqueue("second")
	worker.Enqueue("third")
	close(releaseFirst)

	var prompts []string
	for len(prompts) < 2 {
		select {
		case prompt := <-generated:
			prompts = append(prompts, prompt)
		case <-time.After(time.Second):
			t.Fatal("coalesced extraction did not finish")
		}
	}
	require.Contains(t, prompts[0], "session first")
	require.Contains(t, prompts[1], "session third")
	require.NotContains(t, prompts[1], "session second")
}

func TestWorkerInterruptCancelsBackgroundGeneration(t *testing.T) {
	memory := managedTestMemory(t)
	started := make(chan struct{})
	cancelled := make(chan struct{})
	worker, err := NewWorker(WorkerOptions{
		Memory: memory,
		Generate: func(ctx context.Context, _ string, _ string, _ int64) (string, error) {
			close(started)
			<-ctx.Done()
			close(cancelled)
			return "", ctx.Err()
		},
		LoadTranscript: func(context.Context, string) ([]Turn, error) {
			return []Turn{{Role: "user", Text: "remember this"}}, nil
		},
		LoadSessions: func(context.Context) ([]SessionInfo, error) { return nil, nil },
	})
	require.NoError(t, err)
	t.Cleanup(worker.Close)

	worker.Enqueue("session")
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("background generation did not start")
	}
	require.Equal(t, "updating", worker.Activity())
	worker.Interrupt()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("background generation was not cancelled")
	}
	require.Eventually(t, func() bool {
		return worker.Activity() == ""
	}, time.Second, 10*time.Millisecond)
}

func TestWorkerRejectsAllMutationsBeforeWriting(t *testing.T) {
	memory := managedTestMemory(t)
	worker := &Worker{memory: memory}
	err := worker.apply([]memoryMutation{
		{File: "valid.md", Action: "upsert", Name: "Valid", Description: "valid memory", Type: "user", Content: "body"},
		{File: "../escape.md", Action: "delete"},
	})
	require.ErrorContains(t, err, "invalid memory topic name")
	require.NoFileExists(t, filepath.Join(memory.Directory, "valid.md"))
	require.NoFileExists(t, filepath.Join(filepath.Dir(memory.Directory), "escape.md"))
}

func TestWorkerDreamConsolidatesAfterFiveNewSessionsAndThrottles(t *testing.T) {
	memory := managedTestMemory(t)
	now := time.Date(2026, time.August, 21, 12, 0, 0, 0, time.UTC)
	var consolidationCalls atomic.Int32
	sessions := make([]SessionInfo, 0, 6)
	for index := range 5 {
		sessions = append(sessions, SessionInfo{ID: string(rune('a' + index)), UpdatedAt: now.Add(-time.Duration(index) * time.Hour)})
	}
	sessions = append(sessions, SessionInfo{ID: "current", UpdatedAt: now})
	worker, err := NewWorker(WorkerOptions{
		Memory: memory,
		Generate: func(_ context.Context, purpose, prompt string, maxOutputTokens int64) (string, error) {
			require.Equal(t, "memory_consolidation", purpose)
			require.Contains(t, prompt, "First inspect the current target-scope memories")
			require.Contains(t, prompt, "remove memories that are no longer relevant")
			require.Contains(t, prompt, "roughly 30 to 50 memories")
			require.Contains(t, prompt, "soft target, not a hard limit")
			require.Contains(t, prompt, "Recent session excerpts")
			require.EqualValues(t, 8192, maxOutputTokens)
			consolidationCalls.Add(1)
			return `{"memories":[{"file":"shared.md","action":"upsert","name":"Cross-session preference","description":"Preference repeated across sessions","type":"feedback","content":"Keep background work bounded."}]}`, nil
		},
		LoadTranscript: func(_ context.Context, sessionID string) ([]Turn, error) {
			return []Turn{{Role: "user", Text: "Session " + sessionID + " says keep work bounded"}}, nil
		},
		LoadSessions: func(context.Context) ([]SessionInfo, error) { return sessions, nil },
		Now:          func() time.Time { return now },
	})
	require.NoError(t, err)
	t.Cleanup(worker.Close)

	require.NoError(t, worker.maybeDream(t.Context(), "current"))
	require.EqualValues(t, 1, consolidationCalls.Load())
	require.FileExists(t, filepath.Join(memory.Directory, "shared.md"))
	lock := filepath.Join(memory.Directory, ".consolidate-lock")
	info, err := os.Stat(lock)
	require.NoError(t, err)
	require.WithinDuration(t, now, info.ModTime(), time.Second)
	require.NoFileExists(t, lock+".active")

	require.NoError(t, worker.maybeDream(t.Context(), "current"))
	require.EqualValues(t, 1, consolidationCalls.Load())
}

func TestWorkerDreamWaitsForFiveOtherSessions(t *testing.T) {
	memory := managedTestMemory(t)
	var calls atomic.Int32
	worker, err := NewWorker(WorkerOptions{
		Memory: memory,
		Generate: func(context.Context, string, string, int64) (string, error) {
			calls.Add(1)
			return `{"memories":[]}`, nil
		},
		LoadTranscript: func(context.Context, string) ([]Turn, error) {
			return []Turn{{Role: "user", Text: "durable preference"}}, nil
		},
		LoadSessions: func(context.Context) ([]SessionInfo, error) {
			return []SessionInfo{
				{ID: "a", UpdatedAt: time.Now()},
				{ID: "b", UpdatedAt: time.Now()},
				{ID: "c", UpdatedAt: time.Now()},
				{ID: "d", UpdatedAt: time.Now()},
				{ID: "current", UpdatedAt: time.Now()},
			}, nil
		},
	})
	require.NoError(t, err)
	t.Cleanup(worker.Close)

	require.NoError(t, worker.maybeDream(t.Context(), "current"))
	require.Zero(t, calls.Load())
	require.NoFileExists(t, filepath.Join(memory.Directory, ".consolidate-lock"))
}

func TestWorkerDreamRollsBackTimestampAfterFailure(t *testing.T) {
	memory := managedTestMemory(t)
	now := time.Date(2026, time.August, 21, 12, 0, 0, 0, time.UTC)
	previous := now.Add(-48 * time.Hour)
	lock := filepath.Join(memory.Directory, ".consolidate-lock")
	require.NoError(t, os.WriteFile(lock, []byte("previous"), 0o600))
	require.NoError(t, os.Chtimes(lock, previous, previous))
	sessions := make([]SessionInfo, 5)
	for index := range sessions {
		sessions[index] = SessionInfo{ID: string(rune('a' + index)), UpdatedAt: now.Add(-time.Duration(index) * time.Hour)}
	}
	worker, err := NewWorker(WorkerOptions{
		Memory: memory,
		Generate: func(context.Context, string, string, int64) (string, error) {
			return "", errors.New("generation failed")
		},
		LoadTranscript: func(context.Context, string) ([]Turn, error) {
			return []Turn{{Role: "user", Text: "durable preference"}}, nil
		},
		LoadSessions: func(context.Context) ([]SessionInfo, error) { return sessions, nil },
		Now:          func() time.Time { return now },
	})
	require.NoError(t, err)
	t.Cleanup(worker.Close)

	require.ErrorContains(t, worker.maybeDream(t.Context(), "current"), "generation failed")
	info, err := os.Stat(lock)
	require.NoError(t, err)
	require.WithinDuration(t, previous, info.ModTime(), time.Second)
	require.NoFileExists(t, lock+".active")
}

func TestDreamLockSerializesConcurrentConsolidation(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, ".consolidate-lock")
	now := time.Now()
	_, acquired, err := acquireDreamLock(path, time.Time{}, now)
	require.NoError(t, err)
	require.True(t, acquired)
	t.Cleanup(func() { _ = os.Remove(path + ".active") })

	_, acquired, err = acquireDreamLock(path, now.Add(-48*time.Hour), now)
	require.NoError(t, err)
	require.False(t, acquired)
}

func TestNewWorkerDoesNotWriteAutomaticallyToCustomMemoryDirectory(t *testing.T) {
	worker, err := NewWorker(WorkerOptions{
		Memory: Memory{Directory: t.TempDir(), Managed: false},
		Generate: func(context.Context, string, string, int64) (string, error) {
			return "", nil
		},
		LoadTranscript: func(context.Context, string) ([]Turn, error) { return nil, nil },
		LoadSessions:   func(context.Context) ([]SessionInfo, error) { return nil, nil },
	})
	require.NoError(t, err)
	require.Nil(t, worker)
}
