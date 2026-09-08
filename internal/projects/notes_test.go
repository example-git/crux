package projects

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProjectNoteIndexAndSelectiveReads(t *testing.T) {
	service := NewServiceAt(t.TempDir())
	root := t.TempDir()
	document, err := service.Create(testDefinition("notes-first"), root)
	require.NoError(t, err)
	body := "## Rendering decision\n\n" + strings.Repeat("Detailed evidence Ω. ", 1500)
	_, err = service.AppendNotes(root, body)
	require.NoError(t, err)
	before, err := os.ReadFile(document.NotesPath)
	require.NoError(t, err)
	page, err := service.ListNotes(root, 0)
	require.NoError(t, err)
	require.NotEmpty(t, page.Entries)
	entry := page.Entries[0]
	require.Equal(t, "Rendering decision", entry.Title)
	require.Empty(t, entry.Content)
	var restored strings.Builder
	offset := 0
	for {
		detail, err := service.ReadNote(root, entry.ID, offset)
		require.NoError(t, err)
		restored.WriteString(detail.Content)
		if detail.NextOffset == nil {
			break
		}
		offset = *detail.NextOffset
	}
	require.Contains(t, restored.String(), strings.TrimSpace(body))
	after, err := os.ReadFile(document.NotesPath)
	require.NoError(t, err)
	require.Equal(t, before, after)
	_, err = service.AppendNotes(root, "A later observation")
	require.NoError(t, err)
	_, err = service.ReadNote(root, entry.ID, 0)
	require.NoError(t, err)
	_, err = service.Create(testDefinition("notes-second"), root)
	require.NoError(t, err)
	_, err = service.ReadNote(root, entry.ID, 0)
	require.ErrorContains(t, err, "active project")
	page, err = service.ListNotes(root, 0)
	require.NoError(t, err)
	require.Empty(t, page.Entries)
	_, err = service.Activate("notes-first", root)
	require.NoError(t, err)
	_, err = service.ReadNote(root, entry.ID, 0)
	require.NoError(t, err)
	require.NoError(t, service.Disable(root))
	_, err = service.ListNotes(root, 0)
	require.ErrorContains(t, err, "no active project")
}

func TestProjectNoteIndexPaginationAndLegacyContent(t *testing.T) {
	document := Document{Metadata: Metadata{Slug: "legacy"}, Notes: "# Legacy Notes\n"}
	for index := 0; index < 125; index++ {
		document.Notes += fmt.Sprintf("\n## Topic %d\n\nBody %d\n", index, index)
	}
	seen := make(map[string]bool)
	offset := 0
	for {
		page, err := document.NotesIndex(offset)
		require.NoError(t, err)
		require.Equal(t, 125, page.Total)
		require.LessOrEqual(t, len(page.Entries), 50)
		for _, entry := range page.Entries {
			require.False(t, seen[entry.ID])
			seen[entry.ID] = true
			require.Empty(t, entry.Content)
		}
		if page.NextOffset == nil {
			break
		}
		offset = *page.NextOffset
	}
	require.Len(t, seen, 125)
	_, err := document.NotesIndex(-1)
	require.Error(t, err)
	_, err = document.NotesIndex(126)
	require.Error(t, err)
	document.Notes = "# Legacy Notes\n\n## Heading\n\n```markdown\n## Code heading\n```\n\n- 2026-09-08T10:00:00Z `T1`: Task evidence\n"
	entries := document.noteEntries()
	require.Len(t, entries, 2)
	require.Contains(t, entries[0].Content, "## Code heading")
	require.Contains(t, entries[1].Content, "Task evidence")
}
