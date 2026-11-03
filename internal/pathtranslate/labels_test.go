package pathtranslate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCustomFolderLabelRoundTripAndStableIdentity(t *testing.T) {
	store, _ := testStore(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	root := filepath.Join(base, "original private name")
	require.NoError(t, os.MkdirAll(root, 0o700))
	file := filepath.Join(root, "secret.txt")
	require.NoError(t, os.WriteFile(file, []byte("original"), 0o600))
	policy, err := store.Save(t.Context(), Policy{Scope: Scope{Kind: "session", SessionID: "labels"}, Settings: Settings{Enabled: true, Targets: []Target{{Path: root, Label: "My library 日本語", Recursive: true, CensorImages: true}}}})
	require.NoError(t, err)
	names, err := store.Names(t.Context())
	require.NoError(t, err)
	translator, err := New(names, policy.Settings, filepath.Dir(root))
	require.NoError(t, err)
	translated, err := translator.Path(t.Context(), file)
	require.NoError(t, err)
	require.Contains(t, translated, "/My library 日本語/")
	require.NotContains(t, translated, "secret")
	require.NotContains(t, translated, "original private name")
	resolved, err := translator.ResolvePath(t.Context(), translated)
	require.NoError(t, err)
	require.Equal(t, file, resolved)
	resolved, err = translator.ResolveText(t.Context(), translated)
	require.NoError(t, err)
	require.Equal(t, file, resolved)
	rootAlias, err := translator.Path(t.Context(), root)
	require.NoError(t, err)
	resolved, err = translator.ResolveText(t.Context(), "Read '"+rootAlias+"' next.")
	require.NoError(t, err)
	require.Equal(t, "Read '"+root+"' next.", resolved)
	command, err := translator.ResolveShell(t.Context(), "ls '"+rootAlias+"'")
	require.NoError(t, err)
	require.Contains(t, command, root)
	command, err = translator.ResolveShell(t.Context(), "ls \""+rootAlias+"\"")
	require.NoError(t, err)
	require.Contains(t, command, root)
	text, err := translator.Text(t.Context(), "Read "+root+" next.")
	require.NoError(t, err)
	require.Contains(t, text, "My library 日本語")
	require.NotContains(t, text, "original private name")
	first, err := names.Register(t.Context(), root)
	require.NoError(t, err)
	policy.Settings.Targets[0].Label = "Replacement"
	policy, err = store.Save(t.Context(), policy)
	require.NoError(t, err)
	again, err := names.Register(t.Context(), root)
	require.NoError(t, err)
	require.Equal(t, first, again)
	translator, err = New(names, policy.Settings, filepath.Dir(root))
	require.NoError(t, err)
	translated, err = translator.Path(t.Context(), root)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(filepath.Dir(root), "Replacement"), translated)
	policy.Settings.Targets[0].Label = ""
	translator, err = New(names, policy.Settings, filepath.Dir(root))
	require.NoError(t, err)
	translated, err = translator.Path(t.Context(), root)
	require.NoError(t, err)
	require.Equal(t, first.Alias, filepath.Base(translated))
	policy.Settings.Enabled = false
	translator, err = New(names, policy.Settings, filepath.Dir(root))
	require.NoError(t, err)
	translated, err = translator.Path(t.Context(), file)
	require.NoError(t, err)
	require.Equal(t, file, translated)
}

func TestFolderLabelValidationAndImageDefault(t *testing.T) {
	for _, label := range []string{".", "..", "x/y", "x\\y", " spaced ", "new\nline", "pt_" + strings.Repeat("a", 64)} {
		require.Error(t, (Settings{Targets: []Target{{Path: "/protected", Label: label}}}).Validate())
	}
	require.Error(t, (Settings{Targets: []Target{{Path: "/one", Label: "Same"}, {Path: "/two", Label: "Same"}}}).Validate())
	store, _ := testStore(t)
	censor := false
	policy, err := store.Save(t.Context(), Policy{Scope: Scope{Kind: "session", SessionID: "empty"}, Settings: Settings{Enabled: true, CensorImages: &censor}})
	require.NoError(t, err)
	names, err := store.Names(t.Context())
	require.NoError(t, err)
	translator, err := New(names, policy.Settings, "/tmp")
	require.NoError(t, err)
	require.False(t, translator.Protected("/tmp/anything"))
	policy.Settings.Targets = []Target{{Path: "/protected", Recursive: true, CensorImages: true}}
	translator, err = New(names, policy.Settings, "/tmp")
	require.NoError(t, err)
	require.False(t, translator.CensorImage("/protected/picture.png"))
	censor = true
	translator, err = New(names, policy.Settings, "/tmp")
	require.NoError(t, err)
	require.True(t, translator.CensorImage("/protected/picture.png"))
	require.False(t, translator.CensorImage("/outside/picture.png"))
}
