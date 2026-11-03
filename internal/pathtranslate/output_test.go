package pathtranslate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestShellOutputReferencesResolveEncounteredNames(t *testing.T) {
	store, conn := testStore(t)
	policy := testPolicy(t, store, Scope{Kind: "session", SessionID: "output"})
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	policy.Settings.Targets = []Target{{Path: root, Recursive: true}}
	names, err := store.Names(t.Context())
	require.NoError(t, err)
	translator, err := New(names, policy.Settings, root)
	require.NoError(t, err)
	filename := "confidential \"name\" 日本.txt"
	file := filepath.Join(root, filename)
	require.NoError(t, os.WriteFile(file, []byte("unchanged"), 0o600))
	for _, output := range []string{filename + "\n", "-rw------- 1 owner group 9 Oct 31 " + filename + "\n", "result\t" + filename + "\n"} {
		references, err := translator.OutputReferences(output, []string{root})
		require.NoError(t, err)
		require.Len(t, references, 1)
		require.Equal(t, file, references[0].Path)
		translated, err := translator.OutputText(t.Context(), output, []string{root})
		require.NoError(t, err)
		require.NotContains(t, translated, "confidential")
		require.NotContains(t, translated, "name")
	}
	encoded, err := json.MarshalIndent(map[string]any{"task": map[string]string{"status": "completed"}, "output": filename + "\n"}, "", "  ")
	require.NoError(t, err)
	references, err := translator.OutputJSONReferences(string(encoded), []string{root})
	require.NoError(t, err)
	require.Len(t, references, 1)
	metadata, err := json.Marshal(PathReferences{Paths: references})
	require.NoError(t, err)
	translated, err := translator.ReferencedText(t.Context(), string(encoded), string(metadata))
	require.NoError(t, err)
	var result map[string]any
	require.NoError(t, json.Unmarshal([]byte(translated), &result))
	alias := strings.TrimSpace(result["output"].(string))
	resolved, err := translator.ResolvePath(t.Context(), alias)
	require.NoError(t, err)
	require.Equal(t, file, resolved)
	var count int
	require.NoError(t, conn.QueryRow(`SELECT count(*) FROM path_translation_names`).Scan(&count))
	require.Equal(t, 2, count)
}

func TestTextTranslatesExistingUnquotedDescendantWithSpaces(t *testing.T) {
	store, _ := testStore(t)
	policy := testPolicy(t, store, Scope{Kind: "session", SessionID: "unquoted"})
	root := t.TempDir()
	policy.Settings.Targets = []Target{{Path: root, Recursive: true}}
	names, err := store.Names(t.Context())
	require.NoError(t, err)
	translator, err := New(names, policy.Settings, filepath.Dir(root))
	require.NoError(t, err)
	file := filepath.Join(root, "private name 日本.txt")
	require.NoError(t, os.WriteFile(file, []byte("original"), 0o600))
	text, err := translator.Text(t.Context(), "Read "+file+" next.")
	require.NoError(t, err)
	require.NotContains(t, text, "private name")
	require.NotContains(t, text, "日本")
	require.True(t, strings.HasSuffix(text, " next."))
	decoded, err := translator.ResolveText(t.Context(), text)
	require.NoError(t, err)
	require.Equal(t, "Read "+file+" next.", decoded)
}

func TestTextSelectedFilenameOutsideWorkingDirectory(t *testing.T) {
	store, _ := testStore(t)
	policy := testPolicy(t, store, Scope{Kind: "session", SessionID: "selected-file"})
	policy.Settings.Targets = []Target{{Path: "/private/elsewhere/selected-file.txt"}}
	names, err := store.Names(t.Context())
	require.NoError(t, err)
	translator, err := New(names, policy.Settings, t.TempDir())
	require.NoError(t, err)
	translated, err := translator.Text(t.Context(), "Read selected-file.txt next.")
	require.NoError(t, err)
	require.NotContains(t, translated, "selected-file.txt")
	alias := strings.Fields(translated)[1]
	resolved, err := translator.ResolvePath(t.Context(), alias)
	require.NoError(t, err)
	require.Equal(t, "/private/elsewhere/selected-file.txt", resolved)
}

func TestTextSelectedSpacedFilenameOutsideWorkingDirectory(t *testing.T) {
	store, _ := testStore(t)
	policy := testPolicy(t, store, Scope{Kind: "session", SessionID: "selected-spaced-file"})
	selected := "/private/elsewhere/selected name 日本.txt"
	policy.Settings.Targets = []Target{{Path: selected}}
	names, err := store.Names(t.Context())
	require.NoError(t, err)
	translator, err := New(names, policy.Settings, t.TempDir())
	require.NoError(t, err)
	translated, err := translator.Text(t.Context(), "Read selected name 日本.txt next.")
	require.NoError(t, err)
	require.NotContains(t, translated, "selected name 日本.txt")
	alias := strings.Fields(translated)[1]
	resolved, err := translator.ResolvePath(t.Context(), alias)
	require.NoError(t, err)
	require.Equal(t, selected, resolved)
	unprotected := "/public/selected name 日本.txt"
	translated, err = translator.Text(t.Context(), "Read "+unprotected+" next.")
	require.NoError(t, err)
	require.Equal(t, "Read "+unprotected+" next.", translated)
}

func TestTextSelectedBasenameBoundariesAndAmbiguity(t *testing.T) {
	store, _ := testStore(t)
	policy := testPolicy(t, store, Scope{Kind: "session", SessionID: "basename-boundaries"})
	selected := "/private/elsewhere/selected name 日本.txt"
	policy.Settings.Targets = []Target{{Path: selected}}
	names, err := store.Names(t.Context())
	require.NoError(t, err)
	translator, err := New(names, policy.Settings, t.TempDir())
	require.NoError(t, err)
	for _, input := range []string{"Read selected name 日本.txt.", "Read `selected name 日本.txt`.", "Read (selected name 日本.txt)."} {
		translated, err := translator.Text(t.Context(), input)
		require.NoError(t, err)
		require.NotContains(t, translated, "selected name 日本.txt")
		require.Contains(t, translated, AliasPrefix)
	}
	for _, input := range []string{"Read selected name 日本.txt.backup", "Read /public/selected name 日本.txt", "Read /public/selected name 日本.txt/child"} {
		translated, err := translator.Text(t.Context(), input)
		require.NoError(t, err)
		require.Equal(t, input, translated)
	}
	policy.Settings.Targets = append(policy.Settings.Targets, Target{Path: "/private/another/selected name 日本.txt"})
	translator, err = New(names, policy.Settings, t.TempDir())
	require.NoError(t, err)
	_, err = translator.Text(t.Context(), "Read selected name 日本.txt next.")
	require.ErrorContains(t, err, "ambiguous")
	translated, err := translator.Text(t.Context(), "Read "+selected+" next.")
	require.NoError(t, err)
	require.NotContains(t, translated, "selected name 日本.txt")
}
