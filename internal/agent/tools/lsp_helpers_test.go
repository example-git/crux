package tools

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
	"github.com/stretchr/testify/require"
)

func TestWorkspaceEditCanonicalizesEveryAffectedTarget(t *testing.T) {
	workspace := t.TempDir()
	outside, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	target := filepath.Join(outside, "target.go")
	link := filepath.Join(workspace, "link.go")
	require.NoError(t, os.Symlink(target, link))
	uri := func(path string) protocol.DocumentURI { return protocol.DocumentURI(protocol.URIFromPath(path)) }
	created := filepath.Join(outside, "created.go")
	deleted := filepath.Join(outside, "deleted.go")
	edit := &protocol.WorkspaceEdit{
		Changes: map[protocol.DocumentURI][]protocol.TextEdit{uri(link): {}},
		DocumentChanges: []protocol.DocumentChange{{
			CreateFile: &protocol.CreateFile{URI: uri(created)},
			DeleteFile: &protocol.DeleteFile{URI: uri(deleted)},
		}},
	}
	require.NoError(t, canonicalizeWorkspaceEdit(edit, workspace))
	require.Contains(t, edit.Changes, uri(target))
	require.NotContains(t, edit.Changes, uri(link))
	require.ElementsMatch(t, []string{target, created, deleted}, collectAffectedFiles(edit))
	require.NoFileExists(t, target)
	collision := &protocol.WorkspaceEdit{Changes: map[protocol.DocumentURI][]protocol.TextEdit{uri(link): {}, uri(target): {}}}
	require.ErrorContains(t, canonicalizeWorkspaceEdit(collision, workspace), "multiple rename paths")
}

func TestGetSymbolOffset(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		symbol string
		want   int
	}{
		{"bare symbol", "Bar", 0},
		{"dot qualified", "foo.Bar", 4},
		{"double colon qualified", "Class::method", 7},
		{"backslash qualified", `ns\Func`, 3},
		{"nested dots", "a.b.C", 4},
		{"empty", "", 0},
		{"single char", "x", 0},
		{"dot at end", "foo.", 4},
		{"colon at end", "foo::", 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := getSymbolOffset(tt.symbol)
			require.Equal(t, tt.want, got, "getSymbolOffset(%q)", tt.symbol)
		})
	}
}

// TestGetSymbolOffset_DoesNotOvershoot verifies that the offset lands
// on the start of the final component, never past it.
func TestGetSymbolOffset_DoesNotOvershoot(t *testing.T) {
	t.Parallel()

	cases := []struct {
		symbol   string
		expected string // the substring starting at the offset
	}{
		{"foo.Bar", "Bar"},
		{"Class::method", "method"},
		{`ns\Func`, "Func"},
		{"a.b.c.D", "D"},
		{"Bar", "Bar"},
	}

	for _, tc := range cases {
		offset := getSymbolOffset(tc.symbol)
		require.LessOrEqual(t, offset, len(tc.symbol),
			"offset %d exceeds symbol length %d for %q", offset, len(tc.symbol), tc.symbol)
		got := tc.symbol[offset:]
		require.Equal(t, tc.expected, got,
			"getSymbolOffset(%q) = %d, remainder = %q, want %q",
			tc.symbol, offset, got, tc.expected)
	}
}
