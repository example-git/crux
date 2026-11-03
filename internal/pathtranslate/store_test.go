package pathtranslate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/db"
	"github.com/stretchr/testify/require"
)

func testStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	conn, err := db.Connect(context.Background(), dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dir)) })
	return NewStore(conn), conn
}

func testPolicy(t *testing.T, store *Store, scope Scope) Policy {
	t.Helper()
	policy, err := store.Save(context.Background(), Policy{Scope: scope, Settings: Settings{
		Enabled: true,
		Targets: []Target{{Path: "/private/library", Recursive: true, CensorImages: true}},
	}})
	require.NoError(t, err)
	return policy
}

func TestNamesStablePrivateScopedAndPersistent(t *testing.T) {
	ctx := context.Background()
	store, conn := testStore(t)
	policy := testPolicy(t, store, Scope{Kind: "session", SessionID: "one"})
	names, err := store.Names(ctx)
	require.NoError(t, err)
	first, err := names.Register(ctx, "/private/library/a/item.txt")
	require.NoError(t, err)
	require.True(t, IsAlias(first.Alias))
	require.NotContains(t, first.Alias, "item")
	second, err := names.Register(ctx, "/private/library/b/item.txt")
	require.NoError(t, err)
	require.NotEqual(t, first.Identity, second.Identity)
	reopened, err := NewStore(conn).Names(ctx)
	require.NoError(t, err)
	again, err := reopened.Register(ctx, first.Path)
	require.NoError(t, err)
	require.Equal(t, first, again)
	original, err := reopened.Resolve(ctx, first.Alias)
	require.NoError(t, err)
	require.Equal(t, first.Path, original)
	testPolicy(t, store, Scope{Kind: "session", SessionID: "two"})
	otherNames, err := store.Names(ctx)
	require.NoError(t, err)
	otherName, err := otherNames.Register(ctx, first.Path)
	require.NoError(t, err)
	require.Equal(t, first.Identity, otherName.Identity)
	_, err = otherNames.Resolve(ctx, first.Alias)
	require.NoError(t, err)
	policy.Settings.Targets[0].CensorImages = false
	_, err = store.Save(ctx, policy)
	require.NoError(t, err)
	afterSave, err := store.Names(ctx)
	require.NoError(t, err)
	unchanged, err := afterSave.Register(ctx, first.Path)
	require.NoError(t, err)
	require.Equal(t, first, unchanged)
}

func TestSessionPolicyIgnoresLegacyBroaderScopes(t *testing.T) {
	ctx := context.Background()
	store, conn := testStore(t)
	for _, scope := range []Scope{{Kind: "provider", Provider: "provider"}, {Kind: "model", Provider: "provider", Model: "model"}, {Kind: "project"}} {
		_, err := store.Save(ctx, Policy{Scope: scope})
		require.ErrorContains(t, err, "only session scope")
		key, err := json.Marshal(scope)
		require.NoError(t, err)
		_, err = conn.ExecContext(ctx, `INSERT INTO path_translation_scopes(scope_key, settings) VALUES (?, ?)`, string(key), `{"enabled":true,"targets":[{"path":"/private/library","recursive":true}]}`)
		require.NoError(t, err)
	}
	protected := testPolicy(t, store, Scope{Kind: "session", SessionID: "one"})
	current, found, err := store.Effective(ctx, "one")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, protected, current)
	for _, sessionID := range []string{"two", ""} {
		current, found, err = store.Effective(ctx, sessionID)
		require.NoError(t, err)
		require.False(t, found)
		require.False(t, current.Settings.Enabled)
	}
	protected.Settings.Enabled = false
	disabled, err := store.Save(ctx, protected)
	require.NoError(t, err)
	current, found, err = store.Effective(ctx, "one")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, disabled, current)
	_, err = store.Save(ctx, protected)
	require.ErrorContains(t, err, "changed")
}

func TestPathTranslationWithoutFilesystemDiscovery(t *testing.T) {
	ctx := context.Background()
	store, conn := testStore(t)
	policy := testPolicy(t, store, Scope{Kind: "session", SessionID: "one"})
	names, err := store.Names(ctx)
	require.NoError(t, err)
	translator, err := New(names, policy.Settings, "/private")
	require.NoError(t, err)
	var count int
	require.NoError(t, conn.QueryRow(`SELECT count(*) FROM path_translation_names`).Scan(&count))
	require.Zero(t, count)
	for _, path := range []string{"/private/library/a/item.txt", "library/a/item.txt", "/private/library/spaces and 日本語.txt"} {
		translated, err := translator.Path(ctx, path)
		require.NoError(t, err)
		require.NotContains(t, translated, "library")
		require.NotContains(t, translated, "item.txt")
		real, err := translator.ResolvePath(ctx, translated)
		require.NoError(t, err)
		require.Equal(t, translator.absolute(path), real)
		leaf, err := translator.ResolvePath(ctx, filepath.Base(translated))
		require.NoError(t, err)
		require.Equal(t, real, leaf)
	}
	plain, err := translator.Path(ctx, "/private/library-other/file")
	require.NoError(t, err)
	require.Equal(t, "/private/library-other/file", plain)
	require.True(t, translator.CensorImage("library/a/image.png"))
	require.False(t, translator.CensorImage("library-other/image.png"))
	require.Error(t, translator.CheckMove("library", "other"))
	require.Error(t, translator.CheckMove("/private", "/elsewhere"))
	require.NoError(t, translator.CheckMove("/unrelated/a", "/unrelated/b"))
	_, err = translator.ResolvePath(ctx, AliasPrefix+strings.Repeat("0", 64))
	require.ErrorContains(t, err, "unknown")
	require.NoError(t, conn.QueryRow(`SELECT count(*) FROM path_translation_names`).Scan(&count))
	require.Equal(t, 4, count)
}

func TestJSONTranslationPreservesNumbersAndEscapes(t *testing.T) {
	ctx := context.Background()
	store, _ := testStore(t)
	policy := testPolicy(t, store, Scope{Kind: "session", SessionID: "one"})
	names, err := store.Names(ctx)
	require.NoError(t, err)
	translator, err := New(names, policy.Settings, "/private")
	require.NoError(t, err)
	input := `{"path":"/private/library/report.txt","number":9007199254740993,"content":"quoted \"/private/library/report.txt\""}`
	encoded, err := translator.JSON(ctx, input, false)
	require.NoError(t, err)
	require.NotContains(t, encoded, "library")
	decoded, err := translator.JSON(ctx, encoded, true)
	require.NoError(t, err)
	require.JSONEq(t, input, decoded)
	_, err = translator.JSON(ctx, `{}`+`{}`, false)
	require.Error(t, err)
}

func TestProtectedWorkingDirectoryDoesNotTranslateProse(t *testing.T) {
	ctx := context.Background()
	store, _ := testStore(t)
	policy := testPolicy(t, store, Scope{Kind: "session", SessionID: "one"})
	names, err := store.Names(ctx)
	require.NoError(t, err)
	translator, err := New(names, policy.Settings, "/private/library")
	require.NoError(t, err)
	text, err := translator.Text(ctx, "Please read the file and explain it.")
	require.NoError(t, err)
	require.Equal(t, "Please read the file and explain it.", text)
	input := `{"file_path":"spaces and 日本語.txt","content":"Hello world","description":"Read a file"}`
	encoded, err := translator.JSON(ctx, input, false)
	require.NoError(t, err)
	require.NotContains(t, encoded, "spaces and")
	require.Contains(t, encoded, "Hello world")
	require.Contains(t, encoded, "Read a file")
	decoded, err := translator.JSON(ctx, encoded, true)
	require.NoError(t, err)
	require.Contains(t, decoded, "/private/library/spaces and 日本語.txt")
}

func TestMillionStoredNamesUseIndexedLookup(t *testing.T) {
	ctx := context.Background()
	store, conn := testStore(t)
	policy := testPolicy(t, store, Scope{Kind: "session", SessionID: "one"})
	_, err := conn.ExecContext(ctx, `WITH RECURSIVE numbers(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM numbers WHERE n < 1000000)
		INSERT INTO path_translation_names(identity, alias, path)
		SELECT printf('%064x', n), 'pt_' || printf('%064x', n), '/unrelated/' || n FROM numbers`)
	require.NoError(t, err)
	names, err := store.Names(ctx)
	require.NoError(t, err)
	translator, err := New(names, policy.Settings, "/private")
	require.NoError(t, err)
	plain, err := translator.Text(ctx, "An ordinary conversation without paths.")
	require.NoError(t, err)
	require.Equal(t, "An ordinary conversation without paths.", plain)
	encoded, err := translator.Path(ctx, "/private/library/one.txt")
	require.NoError(t, err)
	decoded, err := translator.ResolvePath(ctx, encoded)
	require.NoError(t, err)
	require.Equal(t, "/private/library/one.txt", decoded)
	for _, query := range []string{
		`SELECT path FROM path_translation_names WHERE alias = ?`,
		`SELECT alias, path FROM path_translation_names WHERE identity = ?`,
	} {
		rows, err := conn.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, "unused")
		require.NoError(t, err)
		for rows.Next() {
			var id, parent, unused int
			var detail string
			require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
			require.Contains(t, detail, "SEARCH")
			require.NotContains(t, detail, "SCAN")
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
	}
	var count int
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT count(*) FROM path_translation_names`).Scan(&count))
	require.Equal(t, 1000002, count)
}

func TestPromptTranslatesPointerToolOutputs(t *testing.T) {
	ctx := t.Context()
	store, _ := testStore(t)
	policy := testPolicy(t, store, Scope{Kind: "session", SessionID: "one"})
	names, err := store.Names(ctx)
	require.NoError(t, err)
	translator, err := New(names, policy.Settings, "/private")
	require.NoError(t, err)
	for _, output := range []fantasy.ToolResultOutputContent{
		&fantasy.ToolResultOutputContentText{Text: "/private/library/a.txt"},
		&fantasy.ToolResultOutputContentError{Error: errors.New("read /private/library/a.txt failed")},
		&fantasy.ToolResultOutputContentMedia{Text: "/private/library/a.txt", MediaType: "audio/wav"},
	} {
		prompt := fantasy.Prompt{{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
			&fantasy.ToolResultPart{ToolCallID: "read", Output: output},
		}}}
		translated, err := translator.Prompt(ctx, prompt)
		require.NoError(t, err)
		encoded, err := json.Marshal(translated)
		require.NoError(t, err)
		require.NotContains(t, string(encoded), "library")
		require.NotContains(t, string(encoded), "a.txt")
	}
}

func TestReferencedTextTranslatesClippedNamesAndPreservesOrdinarySpans(t *testing.T) {
	store, _ := testStore(t)
	policy := testPolicy(t, store, Scope{Kind: "session", SessionID: "one"})
	names, err := store.Names(t.Context())
	require.NoError(t, err)
	translator, err := New(names, policy.Settings, "/private")
	require.NoError(t, err)
	text := "secret na[truncated]me.txt ordinary"
	metadata, err := json.Marshal(PathReferences{Paths: []PathReference{
		{Start: 0, End: 9, Path: "/private/library/secret name.txt"},
		{Start: 20, End: 26, Path: "/private/library/secret name.txt"},
		{Start: 27, End: len(text), Path: "/public/ordinary"},
	}})
	require.NoError(t, err)
	translated, err := translator.ReferencedText(t.Context(), text, string(metadata))
	require.NoError(t, err)
	require.NotContains(t, translated, "secret")
	require.NotContains(t, translated, "me.txt")
	require.Contains(t, translated, "[truncated]")
	require.True(t, strings.HasSuffix(translated, " ordinary"))
	require.NotContains(t, translated, "/public")
	_, err = translator.ReferencedText(t.Context(), text, `{"path_references":[{"start":0,"end":999,"path":"/private/library/a"}]}`)
	require.ErrorContains(t, err, "invalid tool path reference")
}

func TestPromptTranslatesHistoricalSearchPaths(t *testing.T) {
	store, _ := testStore(t)
	policy := testPolicy(t, store, Scope{Kind: "session", SessionID: "history"})
	names, err := store.Names(t.Context())
	require.NoError(t, err)
	translator, err := New(names, policy.Settings, "/private")
	require.NoError(t, err)
	for _, test := range []struct{ tool, metadata, text string }{
		{"search", `{"mode":"files"}`, "/private/library/秘密 name.txt\n/public/ordinary name.txt"},
		{"search", `{"mode":"content"}`, "Found 1 matches\n/private/library/秘密 name.txt:\n  Line 1: content"},
		{"codebase_search", "", "Found 1 semantic matches:\n\nRelated\n1. library/秘密 name.txt:1-2 (score 0.9)\ncontent"},
	} {
		prompt := fantasy.Prompt{
			{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.ToolCallPart{ToolCallID: "search", ToolName: test.tool, Input: `{}`}}},
			{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "search", ClientMetadata: test.metadata, Output: fantasy.ToolResultOutputContentText{Text: test.text}}}},
		}
		translated, err := translator.Prompt(t.Context(), prompt)
		require.NoError(t, err)
		encoded, err := json.Marshal(translated)
		require.NoError(t, err)
		require.NotContains(t, string(encoded), "秘密")
		require.NotContains(t, string(encoded), "library/")
	}
}

func TestSelectedSymlinkProtectsCanonicalResultsWithoutInventory(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	realRoot := filepath.Join(root, "real library")
	require.NoError(t, os.Mkdir(realRoot, 0o700))
	selected := filepath.Join(root, "selected library")
	require.NoError(t, os.Symlink(realRoot, selected))
	store, conn := testStore(t)
	settings := Settings{Enabled: true, Targets: []Target{{Path: selected, Recursive: true, CensorImages: true}}}
	_, err = store.Save(t.Context(), Policy{Scope: Scope{Kind: "session", SessionID: "symlink"}, Settings: settings})
	require.NoError(t, err)
	names, err := store.Names(t.Context())
	require.NoError(t, err)
	translator, err := New(names, settings, root)
	require.NoError(t, err)
	var count int
	require.NoError(t, conn.QueryRow(`SELECT count(*) FROM path_translation_names`).Scan(&count))
	require.Zero(t, count)
	for _, path := range []string{filepath.Join(realRoot, "new file.png"), filepath.Join(selected, "new file.png")} {
		require.True(t, translator.Protected(path))
		require.True(t, translator.CensorImage(path))
		require.Error(t, translator.CheckMove(path, filepath.Join(root, "elsewhere")))
		alias, err := translator.Path(t.Context(), path)
		require.NoError(t, err)
		require.NotContains(t, alias, "library")
		require.NotContains(t, alias, "new file")
		resolved, err := translator.ResolvePath(t.Context(), alias)
		require.NoError(t, err)
		require.Equal(t, path, resolved)
	}
	require.Equal(t, []Target{{Path: selected, Recursive: true, CensorImages: true}}, settings.Targets)
}

func TestTextTranslatesSelectedRootWithSpacesWithoutInventory(t *testing.T) {
	store, conn := testStore(t)
	settings := Settings{Enabled: true, Targets: []Target{{Path: "/private/秘密 library", Recursive: true}}}
	_, err := store.Save(t.Context(), Policy{Scope: Scope{Kind: "session", SessionID: "spaces"}, Settings: settings})
	require.NoError(t, err)
	names, err := store.Names(t.Context())
	require.NoError(t, err)
	translator, err := New(names, settings, "/private")
	require.NoError(t, err)
	for _, text := range []string{
		"List /private/秘密 library and answer this question.",
		"Read 秘密 library/child.txt then explain it.",
		"Read `/private/秘密 library/space child.txt`.",
	} {
		translated, err := translator.Text(t.Context(), text)
		require.NoError(t, err)
		require.NotContains(t, translated, "秘密")
		require.NotContains(t, translated, "library")
		require.NotContains(t, translated, "child.txt")
		resolved, err := translator.ResolveText(t.Context(), translated)
		require.NoError(t, err)
		require.Contains(t, resolved, "/private/秘密 library")
	}
	ordinary := "/private/秘密 library-other is not the selected root."
	translated, err := translator.Text(t.Context(), ordinary)
	require.NoError(t, err)
	require.Equal(t, ordinary, translated)
	var mappings int
	require.NoError(t, conn.QueryRow(`SELECT count(*) FROM path_translation_names`).Scan(&mappings))
	require.Equal(t, 3, mappings)
}

func TestPromptUsesOnlyAssociatedToolPathForAcknowledgments(t *testing.T) {
	store, _ := testStore(t)
	policy := testPolicy(t, store, Scope{Kind: "session", SessionID: "acknowledgment"})
	names, err := store.Names(t.Context())
	require.NoError(t, err)
	translator, err := New(names, policy.Settings, "/private")
	require.NoError(t, err)
	path := "/private/library/name with spaces.txt"
	input, err := json.Marshal(map[string]string{"file_path": path, "content": "ordinary text"})
	require.NoError(t, err)
	prompt := fantasy.Prompt{
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.ToolCallPart{ToolCallID: "write", ToolName: "write", Input: string(input)}}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "write", Output: fantasy.ToolResultOutputContentText{Text: "File successfully written: " + path}}}},
	}
	translated, err := translator.Prompt(t.Context(), prompt)
	require.NoError(t, err)
	payload, err := json.Marshal(translated)
	require.NoError(t, err)
	require.NotContains(t, string(payload), "name with spaces")
	require.Contains(t, string(payload), "File successfully written:")
	require.Equal(t, "File successfully written: "+path, prompt[1].Content[0].(fantasy.ToolResultPart).Output.(fantasy.ToolResultOutputContentText).Text)
	ordinary, err := translator.Text(t.Context(), "name with spaces.txt")
	require.NoError(t, err)
	require.Equal(t, "name with spaces.txt", ordinary)
}
