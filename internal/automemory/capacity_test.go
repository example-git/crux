package automemory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func seedMemorySlots(t *testing.T, memory Memory, count int) {
	t.Helper()
	for i := range count {
		content := fmt.Sprintf("---\nname: Topic %d\ndescription: Durable requirement %d\ntype: project\n---\n\nImportant detail %d.", i, i, i)
		require.NoError(t, os.WriteFile(filepath.Join(memory.Directory, fmt.Sprintf("topic-%03d.md", i)), []byte(content), 0o600))
	}
}

func TestProjectMemoryCapacityAndIndependentScopes(t *testing.T) {
	t.Setenv("CRUX_GLOBAL_DATA", t.TempDir())
	t.Setenv("CRUX_AUTO_MEMORY_DIR", "")
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "")
	service := NewService(t.TempDir())
	memory, err := service.resolve(t.Context(), ScopeProject, true)
	require.NoError(t, err)
	seedMemorySlots(t, memory, ProjectMemorySlots-1)
	entry := Entry{File: "last", Name: "Last", Description: "Relevant requirement", Type: "project", Content: "Preserve this detail."}
	_, err = service.Upsert(t.Context(), ScopeProject, entry)
	require.NoError(t, err)
	entry.File = "overflow"
	_, err = service.Upsert(t.Context(), ScopeProject, entry)
	require.ErrorContains(t, err, "50/50 slots occupied")
	require.NoFileExists(t, filepath.Join(memory.Directory, "overflow.md"))
	entry.File = "last"
	entry.Content = "Updated detail."
	_, err = service.Upsert(t.Context(), ScopeProject, entry)
	require.NoError(t, err)
	require.NoError(t, service.Remove(t.Context(), ScopeProject, "last"))
	entry.File = "replacement"
	_, err = service.Upsert(t.Context(), ScopeProject, entry)
	require.NoError(t, err)
	_, err = NewService(t.TempDir()).Upsert(t.Context(), ScopeProject, entry)
	require.NoError(t, err)
	userMemory, err := service.resolve(t.Context(), ScopeUser, true)
	require.NoError(t, err)
	seedMemorySlots(t, userMemory, ProjectMemorySlots)
	_, err = service.Upsert(t.Context(), ScopeUser, entry)
	require.NoError(t, err)
}

func TestMemoryCapacityPreflightsBatchAndPreservesLegacyTopics(t *testing.T) {
	memory := managedTestMemory(t)
	seedMemorySlots(t, memory, ProjectMemorySlots+2)
	worker := &Worker{memory: memory}
	update := memoryMutation{File: "topic-000.md", Action: "upsert", Name: "Retained", Description: "Important requirements", Type: "project", Content: "Merged useful details."}
	before, err := os.ReadFile(filepath.Join(memory.Directory, update.File))
	require.NoError(t, err)
	newTopic := update
	newTopic.File = "extra.md"
	require.ErrorContains(t, worker.apply([]memoryMutation{update, newTopic}), "52/50 slots occupied")
	after, err := os.ReadFile(filepath.Join(memory.Directory, update.File))
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.NoFileExists(t, filepath.Join(memory.Directory, newTopic.File))
	require.NoError(t, worker.apply([]memoryMutation{update, {File: "topic-001.md", Action: "delete"}}))
	topics, err := scanTopics(memory.Directory)
	require.NoError(t, err)
	require.Len(t, topics, ProjectMemorySlots+1)
	require.NoError(t, worker.apply([]memoryMutation{newTopic, {File: "topic-002.md", Action: "delete"}, {File: "topic-003.md", Action: "delete"}}))
	topics, err = scanTopics(memory.Directory)
	require.NoError(t, err)
	require.Len(t, topics, ProjectMemorySlots)
	index, err := os.ReadFile(memory.Entrypoint)
	require.NoError(t, err)
	require.Equal(t, ProjectMemorySlots, strings.Count(string(index), "]("))
	require.NotContains(t, string(index), "Merged useful details")
}

func TestConcurrentMemoryWritersCannotExceedCapacity(t *testing.T) {
	memory := managedTestMemory(t)
	seedMemorySlots(t, memory, ProjectMemorySlots-1)
	start := make(chan struct{})
	results := make(chan error, 8)
	var workers sync.WaitGroup
	for i := range 8 {
		workers.Go(func() {
			<-start
			results <- applyMemoryMutations(memory, []memoryMutation{{File: fmt.Sprintf("new-%d.md", i), Action: "upsert", Name: "New", Description: "Important", Type: "project", Content: "Detail"}})
		})
	}
	close(start)
	workers.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else {
			require.ErrorContains(t, err, "slots occupied")
		}
	}
	require.Equal(t, 1, success)
	topics, err := scanTopics(memory.Directory)
	require.NoError(t, err)
	require.Len(t, topics, ProjectMemorySlots)
}
