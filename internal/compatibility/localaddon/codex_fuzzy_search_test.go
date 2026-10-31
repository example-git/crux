package localaddon

import (
	"testing"

	"github.com/example-git/crux/internal/proto"
	"github.com/stretchr/testify/require"
)

func TestCodexFuzzyFilesProjectsNativeMatches(t *testing.T) {
	indices := []uint32{1, 3}
	files := codexFuzzyFiles([]proto.WorkspaceFileSearchMatch{{
		Root: "root", Path: "src/needle.txt", MatchType: "file", FileName: "needle.txt", Score: 42, Indices: indices,
	}})
	require.Equal(t, []codexFuzzyFile{{
		Root: "root", Path: "src/needle.txt", MatchType: "file", FileName: "needle.txt", Score: 42, Indices: []uint32{1, 3},
	}}, files)
	indices[0] = 9
	require.Equal(t, uint32(1), files[0].Indices[0])
}
