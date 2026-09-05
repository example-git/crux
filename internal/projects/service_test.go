package projects

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func testDefinition(slug string) Definition {
	return Definition{
		Name:            "Project " + slug,
		Slug:            slug,
		Goal:            "Ship the project",
		SuccessCriteria: []string{"All focused tests pass"},
		Tasks: []DefinitionTask{
			{ID: "T1", Content: "Implement the feature"},
			{ID: "T1.1", Content: "Add durable storage", ParentID: "T1"},
			{ID: "T2", Content: "Validate the feature"},
		},
	}
}

func TestServiceCreatePersistsProjectAndNotesAndSelectsIt(t *testing.T) {
	service := NewServiceAt(t.TempDir())
	workingDir := t.TempDir()

	document, err := service.Create(testDefinition("durable-project"), workingDir)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(service.Directory(), "durable-project.md"), document.Path)
	require.Equal(t, filepath.Join(service.Directory(), "durable-project.notes.md"), document.NotesPath)
	require.FileExists(t, document.Path)
	require.FileExists(t, document.NotesPath)
	require.Contains(t, document.Content, "name: Project durable-project")
	require.Contains(t, document.Content, "slug: durable-project")
	require.Contains(t, document.Content, "  - [ ] `T1.1` Add durable storage")
	require.Equal(t, "# Project durable-project Notes\n", document.Notes)

	active, ok, err := service.Active(workingDir)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "durable-project", active.Metadata.Slug)
	goal, subtasks, ok := active.CurrentGoal()
	require.True(t, ok)
	require.Equal(t, "T1", goal.ID)
	require.Len(t, subtasks, 1)
	require.Equal(t, "T1.1", subtasks[0].ID)
}

func TestServiceTracksProgressNotesAndCompletion(t *testing.T) {
	service := NewServiceAt(t.TempDir())
	workingDir := t.TempDir()
	_, err := service.Create(testDefinition("progress"), workingDir)
	require.NoError(t, err)

	_, err = service.UpdateTask(workingDir, "T1", true, "parent too early")
	require.ErrorContains(t, err, "before subtask")

	document, err := service.UpdateTask(workingDir, "T1.1", true, "Storage is covered by tests")
	require.NoError(t, err)
	require.Contains(t, document.Content, "  - [x] `T1.1` Add durable storage")
	require.Contains(t, document.Notes, "`T1.1`: Storage is covered by tests")

	document, err = service.UpdateTask(workingDir, "T1", true, "Feature implementation complete")
	require.NoError(t, err)
	_, err = service.UpdateTask(workingDir, "T1.1", false, "")
	require.ErrorContains(t, err, "parent task")

	_, err = service.AppendNotes(workingDir, "A durable project observation.")
	require.NoError(t, err)
	document, err = service.Get("progress")
	require.NoError(t, err)
	require.Contains(t, document.Notes, "A durable project observation.")

	_, err = service.Complete(workingDir)
	require.ErrorContains(t, err, "incomplete items")
	for _, id := range []string{"C1", "T2"} {
		_, err = service.UpdateTask(workingDir, id, true, "")
		require.NoError(t, err)
	}
	completed, err := service.Complete(workingDir)
	require.NoError(t, err)
	require.Equal(t, StatusCompleted, completed.Metadata.Status)
	_, ok, err := service.Active(workingDir)
	require.NoError(t, err)
	require.False(t, ok)
	_, err = service.Activate("progress", workingDir)
	require.ErrorContains(t, err, "completed")
}

func TestServiceSelectionIsExplicitPerWorkspace(t *testing.T) {
	service := NewServiceAt(t.TempDir())
	firstWorkspace := t.TempDir()
	secondWorkspace := t.TempDir()
	_, err := service.Create(testDefinition("first"), firstWorkspace)
	require.NoError(t, err)
	_, err = service.Create(testDefinition("second"), secondWorkspace)
	require.NoError(t, err)

	active, ok, err := service.Active(firstWorkspace)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "first", active.Metadata.Slug)

	_, err = service.Activate("second", firstWorkspace)
	require.NoError(t, err)
	active, ok, err = service.Active(firstWorkspace)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "second", active.Metadata.Slug)

	require.NoError(t, service.Disable(firstWorkspace))
	_, ok, err = service.Active(firstWorkspace)
	require.NoError(t, err)
	require.False(t, ok)
	active, ok, err = service.Active(secondWorkspace)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "second", active.Metadata.Slug)
}

func TestServiceRejectsInvalidDefinitionsAndSelections(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Definition)
		message string
	}{
		{name: "name", mutate: func(definition *Definition) { definition.Name = "" }, message: "name is required"},
		{name: "slug", mutate: func(definition *Definition) { definition.Slug = "Not Safe" }, message: "invalid project slug"},
		{name: "goal", mutate: func(definition *Definition) { definition.Goal = "" }, message: "goal is required"},
		{name: "criteria", mutate: func(definition *Definition) { definition.SuccessCriteria = nil }, message: "at least one success criterion"},
		{name: "tasks", mutate: func(definition *Definition) { definition.Tasks = nil }, message: "at least one task"},
		{name: "duplicate task", mutate: func(definition *Definition) { definition.Tasks[1].ID = "T1" }, message: "duplicate task"},
		{name: "unknown parent", mutate: func(definition *Definition) { definition.Tasks[1].ParentID = "missing" }, message: "must appear first"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := NewServiceAt(t.TempDir())
			definition := testDefinition("valid-slug")
			test.mutate(&definition)
			_, err := service.Create(definition, t.TempDir())
			require.ErrorContains(t, err, test.message)
		})
	}

	service := NewServiceAt(t.TempDir())
	_, err := service.Activate("missing", t.TempDir())
	require.Error(t, err)
	_, err = service.Create(testDefinition("duplicate"), t.TempDir())
	require.NoError(t, err)
	_, err = service.Create(testDefinition("duplicate"), t.TempDir())
	require.ErrorContains(t, err, "already exists")
}

func TestServiceRejectsMalformedProjectFiles(t *testing.T) {
	directory := t.TempDir()
	service := NewServiceAt(directory)
	require.NoError(t, os.WriteFile(filepath.Join(directory, "broken.md"), []byte("# no frontmatter\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(directory, "broken.notes.md"), []byte("# Notes\n"), 0o600))

	_, err := service.Get("broken")
	require.ErrorContains(t, err, "no YAML frontmatter")
	_, err = service.List()
	require.ErrorContains(t, err, "no YAML frontmatter")
}

func TestServiceListIgnoresNoncanonicalMarkdownFilenames(t *testing.T) {
	directory := t.TempDir()
	service := NewServiceAt(directory)
	_, err := service.Create(testDefinition("valid-project"), t.TempDir())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(directory, "valid-project.handoff.md"), []byte("# no frontmatter\n"), 0o600))

	documents, err := service.List()
	require.NoError(t, err)
	require.Len(t, documents, 1)
	require.Equal(t, "valid-project", documents[0].Metadata.Slug)
}
