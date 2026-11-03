package dialog

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	tea "github.com/example-git/crux/foundation/bubbletea"
	uv "github.com/example-git/crux/foundation/ultraviolet"
	"github.com/example-git/crux/internal/db"
	"github.com/example-git/crux/internal/pathtranslate"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

type pathTranslationTestWorkspace struct {
	workspace.Workspace
	store     *pathtranslate.Store
	directory string
	loads     int
	saves     int
}

func (w *pathTranslationTestWorkspace) WorkingDir() string { return w.directory }

func (w *pathTranslationTestWorkspace) LoadPathTranslation(ctx context.Context, scope pathtranslate.Scope) (pathtranslate.Policy, error) {
	w.loads++
	policy, _, err := w.store.Load(ctx, scope)
	return policy, err
}

func (w *pathTranslationTestWorkspace) SavePathTranslation(ctx context.Context, policy pathtranslate.Policy) (pathtranslate.Policy, error) {
	w.saves++
	return w.store.Save(ctx, policy)
}

func newPathTranslationTestDialog(t *testing.T) (*PathTranslation, *pathTranslationTestWorkspace) {
	t.Helper()
	dir := t.TempDir()
	conn, err := db.Connect(t.Context(), dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dir)) })
	ws := &pathTranslationTestWorkspace{store: pathtranslate.NewStore(conn), directory: t.TempDir()}
	theme := styles.ThemeForProvider("")
	d, cmd := NewPathTranslation(&common.Common{Workspace: ws, Styles: &theme}, "one")
	d.HandleMsg(cmd())
	return d, ws
}

func pathTranslationSend(d *PathTranslation, msg tea.Msg) {
	action := d.HandleMsg(msg)
	if command, ok := action.(ActionCmd); ok && command.Cmd != nil {
		d.HandleMsg(command.Cmd())
	}
}

func pathTranslationKey(d *PathTranslation, code rune) {
	pathTranslationSend(d, tea.KeyPressMsg{Code: code})
}

func TestPathTranslationTwoMenusPersistFolderAndSettings(t *testing.T) {
	d, ws := newPathTranslationTestDialog(t)
	child := filepath.Join(ws.directory, "space and comma, 日本語")
	leaf := filepath.Join(child, "empty")
	require.NoError(t, os.MkdirAll(leaf, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(ws.directory, "not-a-folder.txt"), nil, 0o600))
	pathTranslationKey(d, tea.KeyEnter)
	require.True(t, d.policy.Settings.Enabled)
	require.Equal(t, 1, ws.saves)
	pathTranslationKey(d, tea.KeyDown)
	pathTranslationKey(d, tea.KeyEnter)
	require.False(t, d.censorImages())
	for range 1 {
		pathTranslationKey(d, tea.KeyDown)
	}
	pathTranslationKey(d, tea.KeyEnter)
	require.NotNil(t, d.browser)
	require.Equal(t, []string{child}, d.browser.paths)
	pathTranslationKey(d, tea.KeyDown)
	pathTranslationKey(d, tea.KeyDown)
	pathTranslationKey(d, tea.KeyEnter)
	require.Equal(t, child, d.browser.directory)
	require.Equal(t, []string{leaf}, d.browser.paths)
	pathTranslationKey(d, tea.KeyDown)
	pathTranslationKey(d, tea.KeyDown)
	pathTranslationKey(d, tea.KeyRight)
	require.Equal(t, leaf, d.browser.directory)
	require.Empty(t, d.browser.paths)
	screen := uv.NewScreenBuffer(80, 24)
	d.Draw(screen, screen.Bounds())
	view := ansi.Strip(screen.Render())
	require.Contains(t, view, "Current folder: empty")
	require.Contains(t, view, "No subfolders in this directory.")
	require.Contains(t, view, "› [Add this folder]")
	pathTranslationKey(d, tea.KeyEnter)
	require.Nil(t, d.browser)
	require.Equal(t, []pathtranslate.Target{{Path: leaf, Recursive: true, CensorImages: false}}, d.policy.Settings.Targets)
	pathTranslationKey(d, tea.KeyDown)
	pathTranslationKey(d, tea.KeyEnter)
	require.Equal(t, 0, d.editing)
	pathTranslationSend(d, tea.PasteMsg{Content: "My library 日本語"})
	pathTranslationKey(d, tea.KeyEnter)
	require.Equal(t, "My library 日本語", d.policy.Settings.Targets[0].Label)
	require.Equal(t, -1, d.editing)
	reopened, cmd := NewPathTranslation(d.com, "one")
	reopened.HandleMsg(cmd())
	require.Equal(t, d.policy, reopened.policy)
	require.False(t, reopened.censorImages())
	other, cmd := NewPathTranslation(d.com, "two")
	other.HandleMsg(cmd())
	require.False(t, other.policy.Settings.Enabled)
	require.Empty(t, other.policy.Settings.Targets)
}

func TestPathTranslationFolderMouseCancelEmptyAndRemoval(t *testing.T) {
	d, ws := newPathTranslationTestDialog(t)
	click := func(field int, browser bool) {
		t.Helper()
		screen := uv.NewScreenBuffer(100, 30)
		d.Draw(screen, screen.Bounds())
		hits := d.hits
		if browser {
			hits = d.browser.hits
		}
		for _, hit := range hits {
			if hit.field == field {
				pathTranslationSend(d, tea.MouseClickMsg{X: hit.area.Min.X, Y: hit.area.Min.Y, Button: uv.MouseLeft})
				return
			}
		}
		t.Fatalf("missing clickable field %d", field)
	}
	click(pathTranslationAdd, false)
	require.Empty(t, d.browser.paths)
	pathTranslationKey(d, tea.KeyEscape)
	require.Nil(t, d.browser)
	require.Zero(t, ws.saves)
	click(pathTranslationAdd, false)
	click(0, true)
	require.Equal(t, filepath.Clean(ws.directory), d.policy.Settings.Targets[0].Path)
	click(pathTranslationAdd, false)
	click(0, true)
	require.Len(t, d.policy.Settings.Targets, 1)
	require.Equal(t, 1, ws.saves)
	click(pathTranslationFolders, false)
	pathTranslationSend(d, tea.PasteMsg{Content: "invalid/name"})
	pathTranslationKey(d, tea.KeyEnter)
	require.Contains(t, d.lastError, "single name")
	require.Equal(t, 0, d.editing)
	pathTranslationKey(d, tea.KeyEscape)
	require.Empty(t, d.policy.Settings.Targets[0].Label)
	click(pathTranslationFolders+1, false)
	require.Empty(t, d.policy.Settings.Targets)
	for _, size := range [][2]int{{100, 30}, {50, 20}, {20, 8}} {
		screen := uv.NewScreenBuffer(size[0], size[1])
		d.Draw(screen, screen.Bounds())
		for _, hit := range d.hits {
			require.GreaterOrEqual(t, hit.area.Min.X, 0)
			require.LessOrEqual(t, hit.area.Max.Y, size[1])
		}
	}
}

func TestPathTranslationIgnoresClosedInstancesAndRejectsInvalidPath(t *testing.T) {
	d, _ := newPathTranslationTestDialog(t)
	d.HandleMsg(pathTranslationResultMsg{owner: &PathTranslation{}, policy: pathtranslate.Policy{Revision: 100}})
	require.Zero(t, d.policy.Revision)
	d.policy.Settings = pathtranslate.Settings{Enabled: true, Targets: []pathtranslate.Target{{Path: "relative"}}}
	require.Nil(t, d.save())
	require.Contains(t, d.lastError, "absolute paths")
}

func TestPathTranslationLongFolderNamesAndAutomaticLabel(t *testing.T) {
	d, ws := newPathTranslationTestDialog(t)
	longPath := "/" + strings.Repeat("parent/", 30) + "selected folder"
	require.Contains(t, pathTranslationDisplayPath(longPath, 35), "selected folder")
	require.LessOrEqual(t, ansi.StringWidth(pathTranslationDisplayPath(longPath, 35)), 35)
	for range 2 {
		pathTranslationKey(d, tea.KeyDown)
	}
	pathTranslationKey(d, tea.KeyEnter)
	pathTranslationKey(d, tea.KeyEnter)
	pathTranslationKey(d, tea.KeyDown)
	pathTranslationKey(d, tea.KeyEnter)
	pathTranslationSend(d, tea.PasteMsg{Content: "Custom"})
	pathTranslationKey(d, tea.KeyEnter)
	pathTranslationKey(d, tea.KeyEnter)
	pathTranslationSend(d, tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl})
	pathTranslationSend(d, tea.KeyPressMsg{Code: 'k', Mod: tea.ModCtrl})
	pathTranslationKey(d, tea.KeyEnter)
	require.Empty(t, d.policy.Settings.Targets[0].Label)
	loaded, _, err := ws.store.Load(t.Context(), d.policy.Scope)
	require.NoError(t, err)
	require.Empty(t, loaded.Settings.Targets[0].Label)
}
