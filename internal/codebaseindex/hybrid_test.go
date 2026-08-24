package codebaseindex

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSupplementRelatedDefinitionsAddsNamedCallee(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "permission", "service.go")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	content := `package permission

type Service struct{}

func (s *Service) Request(operation string) error {
	return enforceWorkspacePolicy(operation)
}
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	info, err := os.Stat(path)
	require.NoError(t, err)
	reader := &Reader{catalog: &storeCatalog{NativeFiles: []nativeProjectFile{{Path: "permission/service.go", Size: info.Size(), ModTime: info.ModTime().UnixNano()}}}}
	results := []SearchResult{
		{Chunk: Chunk{Path: "tools/view.go", Content: "permissions.Request(ctx, readOperation) approves workspace reads"}, Score: 0.8},
		{Chunk: Chunk{Path: "tools/write.go", Content: "permissions.Request(ctx, writeOperation) approves workspace writes"}, Score: 0.79},
	}

	supplemented, err := reader.SupplementRelatedDefinitions(t.Context(), root, "central permission request governing workspace reads and writes", "", results, 5)

	require.NoError(t, err)
	require.Len(t, supplemented, 3)
	require.Equal(t, "permission/service.go", supplemented[2].Chunk.Path)
	require.Equal(t, "request", supplemented[2].Symbol)
	require.Contains(t, supplemented[2].Chunk.Content, "func (s *Service) Request")
}

func TestSupplementDocumentationAddsHighCoverageSections(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "docs", "delivery.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	content := `# Overview

General task behavior.

# Durable delivery

Persisted pending notifications are delivered to the parent session exactly once. Concurrent delivery is deduplicated, successful injection is marked delivered, and restart retries undelivered records.

# Unrelated

Theme configuration and colors.
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	info, err := os.Stat(path)
	require.NoError(t, err)
	reader := &Reader{catalog: &storeCatalog{NativeFiles: []nativeProjectFile{{Path: "docs/delivery.md", Size: info.Size(), ModTime: info.ModTime().UnixNano()}}}}
	results := []SearchResult{{Chunk: Chunk{Path: "runtime/task.go", StartLine: 1, EndLine: 20, Content: "complete task"}, Score: 0.8}}

	supplemented, err := reader.SupplementDocumentation(t.Context(), root, "durable notification delivery parent session exactly once deduplicated persisted restart retry injection", "", results, 5)

	require.NoError(t, err)
	require.Len(t, supplemented, 2)
	require.Equal(t, "docs/delivery.md", supplemented[1].Chunk.Path)
	require.Equal(t, 5, supplemented[1].Chunk.StartLine)
	require.Contains(t, supplemented[1].Chunk.Content, "Concurrent delivery is deduplicated")
}

func TestSupplementDocumentationRespectsPathPrefix(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "docs", "contract.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("# Contract\n\ndurable notification delivery"), 0o600))
	info, err := os.Stat(path)
	require.NoError(t, err)
	reader := &Reader{catalog: &storeCatalog{NativeFiles: []nativeProjectFile{{Path: "docs/contract.md", Size: info.Size(), ModTime: info.ModTime().UnixNano()}}}}

	results, err := reader.SupplementDocumentation(t.Context(), root, "durable notification delivery", "src/", nil, 5)

	require.NoError(t, err)
	require.Empty(t, results)
}
