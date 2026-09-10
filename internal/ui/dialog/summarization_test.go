package dialog

import (
	"errors"
	"testing"

	tea "github.com/example-git/crux/foundation/bubbletea"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

type summarizationWorkspace struct {
	codebaseIndexTestWorkspace
	scope config.Scope
	field string
	value any
	err   error
}

func (w *summarizationWorkspace) SetConfigField(scope config.Scope, field string, value any) error {
	w.scope, w.field, w.value = scope, field, value
	return w.err
}

func TestSummarizationFastModeToggle(t *testing.T) {
	ws := &summarizationWorkspace{codebaseIndexTestWorkspace: codebaseIndexTestWorkspace{cfg: &config.Config{Options: &config.Options{SummarizationFastMode: true}}}}
	theme := styles.ThemeForProvider("")
	d := NewSummarization(&common.Common{Workspace: ws, Styles: &theme})
	require.True(t, d.fastMode)
	d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyUp})
	d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyUp})
	require.Equal(t, 3, d.cursor)
	d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.False(t, d.fastMode)
	require.False(t, d.editing)
	require.Equal(t, config.ScopeWorkspace, ws.scope)
	require.Equal(t, "options.summarization_fast_mode", ws.field)
	require.Equal(t, false, ws.value)
	ws.err = errors.New("save failed")
	d.HandleMsg(tea.KeyPressMsg{Code: tea.KeySpace})
	require.False(t, d.fastMode)
	require.Equal(t, "save failed", d.status)
	ws.err = nil
	d.HandleMsg(tea.KeyPressMsg{Code: tea.KeySpace})
	require.True(t, d.fastMode)
	require.Equal(t, true, ws.value)
	d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyDown})
	require.Equal(t, 4, d.cursor)
	d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyDown})
	require.Zero(t, d.cursor)
}

func TestSummarizationCompactionV2Toggle(t *testing.T) {
	ws := &summarizationWorkspace{codebaseIndexTestWorkspace: codebaseIndexTestWorkspace{cfg: &config.Config{Options: &config.Options{}}}}
	theme := styles.ThemeForProvider("")
	d := NewSummarization(&common.Common{Workspace: ws, Styles: &theme})
	require.False(t, d.compactionV2)
	d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyUp})
	require.Equal(t, 4, d.cursor)
	d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.True(t, d.compactionV2)
	require.False(t, d.editing)
	require.Equal(t, config.ScopeWorkspace, ws.scope)
	require.Equal(t, "options.codex_compaction_v2", ws.field)
	require.Equal(t, true, ws.value)
	ws.err = errors.New("save failed")
	d.HandleMsg(tea.KeyPressMsg{Code: tea.KeySpace})
	require.True(t, d.compactionV2)
	require.Equal(t, "save failed", d.status)
	ws.err = nil
	d.HandleMsg(tea.KeyPressMsg{Code: tea.KeySpace})
	require.False(t, d.compactionV2)
	require.Equal(t, false, ws.value)
	ws.cfg.Options.CodexCompactionV2 = true
	require.True(t, NewSummarization(&common.Common{Workspace: ws, Styles: &theme}).compactionV2)
}

func TestSummarizationSettingsSaveAndValidation(t *testing.T) {
	ws := &summarizationWorkspace{codebaseIndexTestWorkspace: codebaseIndexTestWorkspace{cfg: &config.Config{Options: &config.Options{}}}}
	theme := styles.ThemeForProvider("")
	d := NewSummarization(&common.Common{Workspace: ws, Styles: &theme})
	enter := tea.KeyPressMsg{Code: tea.KeyEnter}
	d.HandleMsg(enter)
	require.True(t, d.disabled)
	require.Equal(t, config.ScopeWorkspace, ws.scope)
	require.Equal(t, "options.disable_auto_summarize", ws.field)
	d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyDown})
	d.HandleMsg(enter)
	d.input.SetValue("-1")
	d.HandleMsg(enter)
	require.True(t, d.editing)
	require.Zero(t, d.contextCap)
	d.input.SetValue("258000")
	ws.err = errors.New("save failed")
	d.HandleMsg(enter)
	require.Equal(t, "save failed", d.status)
	require.True(t, d.editing)
	ws.err = nil
	d.HandleMsg(enter)
	require.False(t, d.editing)
	require.EqualValues(t, 258000, d.contextCap)
	require.Equal(t, "options.summarization_context_cap", ws.field)
	d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyDown})
	d.HandleMsg(enter)
	d.input.SetValue("32000")
	d.HandleMsg(enter)
	require.EqualValues(t, 32000, d.maxTokens)
	require.Equal(t, "options.summarization_max_tokens", ws.field)
	d.HandleMsg(enter)
	d.input.SetValue("0")
	d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.EqualValues(t, 32000, d.maxTokens)
}
