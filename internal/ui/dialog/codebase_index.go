package dialog

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/ui/common"
)

const CodebaseIndexID = "codebase_index"

const (
	codebaseIndexEnabled = iota
	codebaseIndexDatabase
	codebaseIndexStore
	codebaseIndexInclude
	codebaseIndexExclude
	codebaseIndexSave
	codebaseIndexNow
	codebaseIndexRefresh
	codebaseIndexFieldCount
)

type codebaseIndexResultMsg struct {
	status proto.CodebaseIndexStatus
	err    error
	saved  bool
}

type codebaseIndexPollMsg struct{}

type CodebaseIndex struct {
	com       *common.Common
	help      help.Model
	database  textinput.Model
	store     textinput.Model
	include   textinput.Model
	exclude   textinput.Model
	status    proto.CodebaseIndexStatus
	enabled   bool
	focus     int
	busy      bool
	lastError string
	keyMap    struct {
		Up      key.Binding
		Down    key.Binding
		Select  key.Binding
		Save    key.Binding
		Index   key.Binding
		Refresh key.Binding
		Close   key.Binding
	}
}

var _ Dialog = (*CodebaseIndex)(nil)

func NewCodebaseIndex(com *common.Common) (*CodebaseIndex, tea.Cmd) {
	index := &CodebaseIndex{com: com}
	index.help = help.New()
	index.help.Styles = com.Styles.DialogHelpStyles()
	index.database = codebaseIndexInput(com, "default per project")
	index.store = codebaseIndexInput(com, "default global store")
	index.include = codebaseIndexInput(com, "all project paths")
	index.exclude = codebaseIndexInput(com, "none")
	settings := com.Config().Tools.CodebaseSearch
	index.enabled = settings.IsEnabled()
	index.database.SetValue(settings.DatabasePath)
	index.store.SetValue(settings.GetStoreDirectory())
	index.include.SetValue(strings.Join(settings.IncludePaths, ", "))
	index.exclude.SetValue(strings.Join(settings.ExcludePaths, ", "))
	index.keyMap.Up = key.NewBinding(key.WithKeys("up", "shift+tab"), key.WithHelp("↑", "previous"))
	index.keyMap.Down = key.NewBinding(key.WithKeys("down", "tab"), key.WithHelp("↓", "next"))
	index.keyMap.Select = key.NewBinding(key.WithKeys("enter", "space"), key.WithHelp("enter", "select"))
	index.keyMap.Save = key.NewBinding(key.WithKeys("ctrl+s"), key.WithHelp("ctrl+s", "save"))
	index.keyMap.Index = key.NewBinding(key.WithKeys("ctrl+i"), key.WithHelp("ctrl+i", "index now"))
	index.keyMap.Refresh = key.NewBinding(key.WithKeys("ctrl+r"), key.WithHelp("ctrl+r", "check status"))
	index.keyMap.Close = CloseKey
	index.setFocus(codebaseIndexEnabled)
	return index, index.refreshCmd()
}

func codebaseIndexInput(com *common.Common, placeholder string) textinput.Model {
	input := textinput.New()
	input.SetVirtualCursor(true)
	input.Placeholder = placeholder
	input.SetStyles(com.Styles.TextInput)
	return input
}

func (*CodebaseIndex) ID() string {
	return CodebaseIndexID
}

func (d *CodebaseIndex) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case codebaseIndexResultMsg:
		d.busy = false
		if msg.err != nil {
			d.lastError = msg.err.Error()
			return nil
		}
		d.status = msg.status
		d.lastError = msg.status.Error
		if msg.saved {
			d.enabled = msg.status.Enabled
			d.include.SetValue(strings.Join(msg.status.IncludePaths, ", "))
			d.exclude.SetValue(strings.Join(msg.status.ExcludePaths, ", "))
		}
		if msg.status.State == "indexing" {
			return ActionCmd{Cmd: tea.Tick(1500*time.Millisecond, func(time.Time) tea.Msg {
				return codebaseIndexPollMsg{}
			})}
		}
	case codebaseIndexPollMsg:
		return ActionCmd{Cmd: d.refreshCmd()}
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, d.keyMap.Close):
			return ActionClose{}
		case key.Matches(msg, d.keyMap.Save):
			return d.save(false)
		case key.Matches(msg, d.keyMap.Index):
			return d.save(true)
		case key.Matches(msg, d.keyMap.Refresh):
			return d.refresh()
		case key.Matches(msg, d.keyMap.Up):
			d.setFocus((d.focus - 1 + codebaseIndexFieldCount) % codebaseIndexFieldCount)
			return nil
		case key.Matches(msg, d.keyMap.Down):
			d.setFocus((d.focus + 1) % codebaseIndexFieldCount)
			return nil
		case msg.String() == "enter":
			switch d.focus {
			case codebaseIndexEnabled:
				d.enabled = !d.enabled
			case codebaseIndexDatabase, codebaseIndexStore, codebaseIndexInclude, codebaseIndexExclude:
				d.setFocus((d.focus + 1) % codebaseIndexFieldCount)
			case codebaseIndexSave:
				return d.save(false)
			case codebaseIndexNow:
				return d.save(true)
			case codebaseIndexRefresh:
				return d.refresh()
			}
			return nil
		case key.Matches(msg, d.keyMap.Select) && d.focus == codebaseIndexEnabled:
			d.enabled = !d.enabled
			return nil
		}
		var cmd tea.Cmd
		switch d.focus {
		case codebaseIndexDatabase:
			d.database, cmd = d.database.Update(msg)
		case codebaseIndexStore:
			d.store, cmd = d.store.Update(msg)
		case codebaseIndexInclude:
			d.include, cmd = d.include.Update(msg)
		case codebaseIndexExclude:
			d.exclude, cmd = d.exclude.Update(msg)
		}
		return ActionCmd{Cmd: cmd}
	}
	return nil
}

func (d *CodebaseIndex) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := d.com.Styles
	width := max(0, min(88, area.Dx()-t.Dialog.View.GetHorizontalBorderSize()))
	innerWidth := max(0, width-t.Dialog.View.GetHorizontalFrameSize())
	labelWidth := 11
	inputWidth := max(1, innerWidth-labelWidth)
	d.database.SetWidth(inputWidth)
	d.store.SetWidth(inputWidth)
	d.include.SetWidth(inputWidth)
	d.exclude.SetWidth(inputWidth)

	statusLine := t.Dialog.PrimaryText.Render("Status: ") + t.Dialog.SecondaryText.Render(codebaseIndexStatusLabel(d.status, time.Now()))
	if d.busy {
		statusLine += t.Dialog.SecondaryText.Render(" …")
	}
	check := " "
	if d.enabled {
		check = "✓"
	}
	enabledLine := d.rowStyle(codebaseIndexEnabled).Render(" [" + check + "] Enabled")
	databaseLine := d.inputLine(codebaseIndexDatabase, "Database", d.database.View())
	storeLine := d.inputLine(codebaseIndexStore, "Store", d.store.View())
	includeLine := d.inputLine(codebaseIndexInclude, "Include", d.include.View())
	excludeLine := d.inputLine(codebaseIndexExclude, "Exclude", d.exclude.View())
	saveLine := d.rowStyle(codebaseIndexSave).Render(" Save settings")
	indexLine := d.rowStyle(codebaseIndexNow).Render(" Index now / retry")
	refreshLine := d.rowStyle(codebaseIndexRefresh).Render(" Check index status")

	rows := []string{statusLine}
	if d.status.ProjectRoot != "" {
		rows = append(rows, t.Dialog.SecondaryText.Render(ansi.Truncate("Project: "+d.status.ProjectRoot, innerWidth, "…")))
	}
	if d.status.DatabasePath != "" {
		rows = append(rows, t.Dialog.SecondaryText.Render(ansi.Truncate("Database: "+d.status.DatabasePath, innerWidth, "…")))
	}
	if d.status.StoreDirectory != "" {
		rows = append(rows, t.Dialog.SecondaryText.Render(ansi.Truncate("Store: "+d.status.StoreDirectory, innerWidth, "…")))
	}
	if credential := codebaseIndexCredentialLabel(d.status.CredentialStatus); credential != "" {
		rows = append(rows, t.Dialog.SecondaryText.Render("GitHub indexing: "+credential))
	}
	if d.status.SourceMode != "" {
		source := "Source: " + d.status.SourceMode
		if d.status.Model != "" {
			source += " · " + d.status.Model
		}
		rows = append(rows, t.Dialog.SecondaryText.Render(ansi.Truncate(source, innerWidth, "…")))
	}
	if d.status.CurrentPath != "" {
		rows = append(rows, t.Dialog.SecondaryText.Render(ansi.Truncate("Current: "+d.status.CurrentPath, innerWidth, "…")))
	}
	rows = append(rows, "", enabledLine, databaseLine, storeLine, includeLine, excludeLine, "", saveLine, indexLine, refreshLine)
	if d.lastError != "" {
		rows = append(rows, "", t.Dialog.TitleError.Render(ansi.Truncate(d.lastError, innerWidth, "…")))
	}

	rc := NewRenderContext(t, width)
	rc.Title = "Codebase Index"
	rc.AddPart(lipgloss.JoinVertical(lipgloss.Left, rows...))
	rc.Help = renderDialogHelp(t, &d.help, d, innerWidth)
	DrawCenter(scr, area, rc.Render())
	return nil
}

func (d *CodebaseIndex) ShortHelp() []key.Binding {
	return []key.Binding{d.keyMap.Up, d.keyMap.Down, d.keyMap.Select, d.keyMap.Save, d.keyMap.Index, d.keyMap.Refresh, d.keyMap.Close}
}

func (d *CodebaseIndex) FullHelp() [][]key.Binding {
	return [][]key.Binding{d.ShortHelp()}
}

func (d *CodebaseIndex) setFocus(focus int) {
	d.focus = focus
	d.database.Blur()
	d.store.Blur()
	d.include.Blur()
	d.exclude.Blur()
	switch focus {
	case codebaseIndexDatabase:
		d.database.Focus()
	case codebaseIndexStore:
		d.store.Focus()
	case codebaseIndexInclude:
		d.include.Focus()
	case codebaseIndexExclude:
		d.exclude.Focus()
	}
}

func (d *CodebaseIndex) rowStyle(row int) lipgloss.Style {
	if d.focus == row {
		return d.com.Styles.Dialog.SelectedItem
	}
	return d.com.Styles.Dialog.NormalItem
}

func (d *CodebaseIndex) inputLine(row int, label, value string) string {
	styles := d.com.Styles.Dialog.Arguments
	labelStyle := styles.InputLabelBlurred
	if d.focus == row {
		labelStyle = styles.InputLabelFocused
	}
	return labelStyle.Width(11).Render(label+":") + value
}

func (d *CodebaseIndex) save(reindex bool) Action {
	if d.busy {
		return nil
	}
	d.busy = true
	d.lastError = ""
	update := proto.CodebaseIndexUpdate{
		Enabled:        d.enabled,
		Reindex:        reindex,
		DatabasePath:   strings.TrimSpace(d.database.Value()),
		StoreDirectory: strings.TrimSpace(d.store.Value()),
		IncludePaths:   splitCodebaseIndexPaths(d.include.Value()),
		ExcludePaths:   splitCodebaseIndexPaths(d.exclude.Value()),
	}
	return ActionCmd{Cmd: d.updateCmd(update)}
}

func (d *CodebaseIndex) refresh() Action {
	if d.busy {
		return nil
	}
	d.busy = true
	d.lastError = ""
	return ActionCmd{Cmd: d.refreshCmd()}
}

func (d *CodebaseIndex) refreshCmd() tea.Cmd {
	return func() tea.Msg {
		status, err := d.com.Workspace.CodebaseIndexStatus(context.Background())
		return codebaseIndexResultMsg{status: status, err: err}
	}
}

func (d *CodebaseIndex) updateCmd(update proto.CodebaseIndexUpdate) tea.Cmd {
	return func() tea.Msg {
		status, err := d.com.Workspace.UpdateCodebaseIndex(context.Background(), update)
		return codebaseIndexResultMsg{status: status, err: err, saved: err == nil}
	}
}

func splitCodebaseIndexPaths(value string) []string {
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == '\n'
	})
	paths := make([]string, 0, len(parts))
	for _, part := range parts {
		if path := strings.TrimSpace(part); path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}

func codebaseIndexStatusLabel(status proto.CodebaseIndexStatus, now time.Time) string {
	switch status.State {
	case "disabled":
		return "Disabled"
	case "missing":
		return "Index not built"
	case "indexing":
		if status.FilesTotal > 0 {
			label := fmt.Sprintf("Indexing: %d/%d files, %d chunks", status.FilesProcessed, status.FilesTotal, status.ChunksCreated)
			if status.FilesSkipped > 0 {
				label += fmt.Sprintf(", %d skipped", status.FilesSkipped)
			}
			return label
		}
		if status.Stage != "" {
			return status.Stage + "…"
		}
		return "Indexing…"
	case "ready":
		label := "Ready"
		if status.FilesTotal > 0 {
			label = fmt.Sprintf("Ready: %d files, %d chunks", status.FilesTotal, status.ChunksCreated)
			if status.FilesSkipped > 0 {
				label += fmt.Sprintf(", %d skipped", status.FilesSkipped)
			}
		} else if status.ChunksCreated > 0 {
			label = fmt.Sprintf("Ready: %d chunks", status.ChunksCreated)
		}
		if !status.FinishedAt.IsZero() {
			age := max(0, int(now.Sub(status.FinishedAt).Minutes()))
			label += fmt.Sprintf(" (%dm ago)", age)
		}
		return label
	case "stale":
		return "Index changed; update recommended"
	case "failed":
		return "Index failed; retry available"
	default:
		return "Checking index…"
	}
}

func codebaseIndexCredentialLabel(status string) string {
	switch status {
	case "signed-in":
		return "Signed in"
	case "missing":
		return "Not signed in"
	case "invalid":
		return "Credential invalid"
	default:
		return ""
	}
}
