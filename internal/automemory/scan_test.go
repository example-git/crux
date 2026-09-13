package automemory

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestRelevantSelectsBoundedMatchingTopicsAndMarksStaleContent(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "")
	t.Setenv("CRUX_AUTO_MEMORY_DIR", directory)
	now := time.Date(2026, time.August, 21, 12, 0, 0, 0, time.UTC)
	for index := range 7 {
		path := filepath.Join(directory, "testing-"+string(rune('a'+index))+".md")
		content := "---\nname: Testing preference\ndescription: focused testing workflow\ntype: feedback\n---\n\n" + strings.Repeat("界", 2000)
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		require.NoError(t, os.Chtimes(path, now.Add(-time.Duration(index+2)*24*time.Hour), now.Add(-time.Duration(index+2)*24*time.Hour)))
	}
	require.NoError(t, os.WriteFile(filepath.Join(directory, "unrelated.md"), []byte("---\nname: Cooking\ndescription: recipes\ntype: user\n---\n\nignore"), 0o600))

	result, err := Relevant(t.Context(), t.TempDir(), "testing workflow", now)
	require.NoError(t, err)
	require.Equal(t, maxRelevantMemories, strings.Count(result, "<memory path="))
	require.Contains(t, result, "point-in-time observation")
	require.NotContains(t, result, "unrelated.md")
	require.Contains(t, result, "memory truncated")
	require.True(t, utf8.ValidString(result))
}

func TestRelevantIncludesUserScopeTopics(t *testing.T) {
	projectDirectory := t.TempDir()
	globalData := t.TempDir()
	t.Setenv("CRUX_GLOBAL_DATA", globalData)
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "")
	t.Setenv("CRUX_AUTO_MEMORY_DIR", projectDirectory)
	userDirectory := UserDirectory()
	require.NoError(t, os.MkdirAll(userDirectory, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(userDirectory, "validation.md"), []byte("---\nname: Validation preference\ndescription: focused validation commands\ntype: feedback\n---\n\nUse focused tests."), 0o600))

	result, err := Relevant(t.Context(), t.TempDir(), "focused validation", time.Now())
	require.NoError(t, err)
	require.Contains(t, result, "path="+strconv.Quote(filepath.Join(userDirectory, "validation.md")))
	require.Contains(t, result, "Use focused tests.")
}

func TestRelevantSkipsSymlinkTopics(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "")
	t.Setenv("CRUX_AUTO_MEMORY_DIR", directory)
	target := filepath.Join(t.TempDir(), "outside.md")
	require.NoError(t, os.WriteFile(target, []byte("---\nname: Secret testing\ndescription: testing secret\ntype: user\n---\n\nsecret"), 0o600))
	if err := os.Symlink(target, filepath.Join(directory, "linked.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	result, err := Relevant(t.Context(), t.TempDir(), "testing secret", time.Now())
	require.NoError(t, err)
	require.Empty(t, result)
}

func TestTopicReadRejectsParentReplacementAfterScan(t *testing.T) {
	directory := t.TempDir()
	nested := filepath.Join(directory, "nested")
	require.NoError(t, os.Mkdir(nested, 0o700))
	content := "---\nname: Testing\ndescription: Nested testing memory\ntype: project\n---\n\nInside content."
	require.NoError(t, os.WriteFile(filepath.Join(nested, "topic.md"), []byte(content), 0o600))
	topics, err := scanTopics(directory)
	require.NoError(t, err)
	require.Len(t, topics, 1)
	actual, err := readTopic(topics[0])
	require.NoError(t, err)
	require.Equal(t, content, actual)
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "topic.md"), []byte("Outside fixture."), 0o600))
	require.NoError(t, os.Rename(nested, filepath.Join(directory, "original")))
	require.NoError(t, os.Symlink(outside, nested))
	actual, err = readTopic(topics[0])
	require.Error(t, err)
	require.Empty(t, actual)
}

func TestMemoryReadersRejectExternalControlFiles(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("CRUX_GLOBAL_DATA", t.TempDir())
	t.Setenv("CRUX_AUTO_MEMORY_DIR", directory)
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "")
	outside := filepath.Join(t.TempDir(), "outside.md")
	require.NoError(t, os.WriteFile(outside, []byte("Outside fixture."), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(directory, EntrypointName)))
	memory, err := Load(t.Context(), t.TempDir())
	require.Error(t, err)
	require.Empty(t, memory.Content)
	require.NoError(t, os.Remove(filepath.Join(directory, EntrypointName)))
	require.NoError(t, os.Symlink(outside, filepath.Join(directory, ".consolidate-next")))
	worker := &Worker{memory: Memory{Directory: directory}}
	content, reviewed, _, err := worker.memoryContext(1000)
	require.Error(t, err)
	require.Empty(t, content)
	require.Empty(t, reviewed)
}

func TestReadPrefixBoundsBytesAndLines(t *testing.T) {
	directory := t.TempDir()
	root, err := os.OpenRoot(directory)
	require.NoError(t, err)
	defer root.Close()
	for _, test := range []struct {
		name      string
		content   string
		lines     int
		bytes     int
		want      string
		truncated bool
	}{
		{name: "exact bytes", content: "abcd", lines: 2, bytes: 4, want: "abcd"},
		{name: "excess bytes", content: "abcdef", lines: 2, bytes: 4, want: "abcd", truncated: true},
		{name: "excess lines", content: "ab\ncd\nef", lines: 2, bytes: 100, want: "ab\ncd", truncated: true},
		{name: "utf8 boundary", content: "界界", lines: 2, bytes: 4, want: "界", truncated: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(filepath.Join(directory, "topic.md"), []byte(test.content), 0o600))
			content, truncated, err := readPrefix(root, "topic.md", test.lines, test.bytes)
			require.NoError(t, err)
			require.Equal(t, test.want, string(content))
			require.Equal(t, test.truncated, truncated)
		})
	}
}

func TestRelevantReturnsEmptyWhenNoTopicMatches(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "")
	t.Setenv("CRUX_AUTO_MEMORY_DIR", directory)
	require.NoError(t, os.WriteFile(filepath.Join(directory, "preferences.md"), []byte("---\nname: Editor\ndescription: formatting preferences\ntype: feedback\n---\n\nUse tabs."), 0o600))

	result, err := Relevant(t.Context(), t.TempDir(), "database migration", time.Now())
	require.NoError(t, err)
	require.Empty(t, result)
}
