package session

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/example-git/crux/internal/db"
	"github.com/stretchr/testify/require"
)

func TestCatalogPaginationAndSessionVisibility(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), ".crux")
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.ReleaseConnection(conn)) })
	service := NewService(db.New(conn), conn)
	for index := range 101 {
		_, err := service.Create(t.Context(), fmt.Sprintf("Session %03d", index))
		require.NoError(t, err)
	}
	parent, err := service.Create(t.Context(), "Archived")
	require.NoError(t, err)
	_, err = service.CreateTaskSession(t.Context(), "tool", parent.ID, "Child")
	require.NoError(t, err)
	_, err = service.Archive(t.Context(), parent.ID)
	require.NoError(t, err)
	first, err := ReadCatalog(t.Context(), dataDir, 0)
	require.NoError(t, err)
	require.Len(t, first.Sessions, 100)
	require.True(t, first.More)
	last, err := ReadCatalog(t.Context(), dataDir, 100)
	require.NoError(t, err)
	require.Len(t, last.Sessions, 1)
	require.False(t, last.More)
	seen := map[string]bool{}
	for _, entry := range append(first.Sessions, last.Sessions...) {
		require.False(t, seen[entry.ID])
		seen[entry.ID] = true
		require.NotEqual(t, "Archived", entry.Title)
		require.NotEqual(t, "Child", entry.Title)
	}
}

func TestCatalogMissingDatabaseAndInvalidOffset(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), ".crux")
	result, err := ReadCatalog(t.Context(), dataDir, 0)
	require.NoError(t, err)
	require.NotNil(t, result.Sessions)
	require.Empty(t, result.Sessions)
	require.NoDirExists(t, dataDir)
	_, err = ReadCatalog(t.Context(), dataDir, -1)
	require.ErrorContains(t, err, "offset")
}
