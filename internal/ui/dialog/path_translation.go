package dialog

import (
	"context"
	"fmt"
	"path/filepath"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/foundation/bubbles/textinput"
	tea "github.com/example-git/crux/foundation/bubbletea"
	uv "github.com/example-git/crux/foundation/ultraviolet"
	"github.com/example-git/crux/internal/pathtranslate"
	"github.com/example-git/crux/internal/ui/common"
)

const PathTranslationID = "path_translation"

const (
	pathTranslationEnabled = iota
	pathTranslationImages
	pathTranslationAdd
	pathTranslationFolders
)

type pathTranslationResultMsg struct {
	owner  *PathTranslation
	policy pathtranslate.Policy
	err    error
	saved  bool
}

type pathTranslationRow struct {
	label string
	field int
}

type pathTranslationHit struct {
	area  uv.Rectangle
	field int
}

type PathTranslation struct {
	com         *common.Common
	scope       pathtranslate.Scope
	policy      pathtranslate.Policy
	focus       int
	initialized bool
	busy        bool
	status      string
	lastError   string
	hits        []pathTranslationHit
	browser     *pathTranslationFoldersDialog
	label       textinput.Model
	editing     int
}

var _ Dialog = (*PathTranslation)(nil)

func NewPathTranslation(com *common.Common, sessionID string) (*PathTranslation, tea.Cmd) {
	d := &PathTranslation{com: com, busy: true, editing: -1}
	d.label = textinput.New()
	d.label.SetVirtualCursor(true)
	d.label.SetStyles(com.Styles.TextInput)
	d.label.Placeholder = "Automatic label (leave blank)"
	d.scope = pathtranslate.Scope{Kind: "session", SessionID: sessionID}
	if sessionID == "" {
		d.busy = false
		d.lastError = "A session is required to configure path translation."
		return d, nil
	}
	return d, d.loadCmd()
}

func (*PathTranslation) ID() string { return PathTranslationID }

func (d *PathTranslation) SessionID() string { return d.scope.SessionID }

func (d *PathTranslation) HandleMsg(msg tea.Msg) Action {
	if d.browser != nil {
		return d.handleFolderMsg(msg)
	}
	if d.editing >= 0 {
		return d.editLabel(msg)
	}
	switch msg := msg.(type) {
	case pathTranslationResultMsg:
		if msg.owner != d {
			return nil
		}
		d.busy = false
		if msg.err != nil {
			d.lastError = msg.err.Error()
			d.status = "Not saved. Ctrl+S retries; Ctrl+R reloads saved settings."
			return nil
		}
		d.policy = msg.policy
		d.policy.Settings.Targets = append([]pathtranslate.Target(nil), msg.policy.Settings.Targets...)
		d.initialized = true
		d.lastError = ""
		d.status = "Changes save automatically."
		if msg.saved {
			d.status = "Saved"
		}
		d.focus = min(d.focus, pathTranslationFolders+2*len(d.policy.Settings.Targets)-1)
	case tea.KeyPressMsg:
		if msg.String() == "esc" || msg.String() == "ctrl+c" {
			return ActionClose{}
		}
		if d.busy || d.scope.SessionID == "" {
			return nil
		}
		count := pathTranslationFolders + 2*len(d.policy.Settings.Targets)
		switch msg.String() {
		case "up", "shift+tab":
			d.focus = (d.focus + count - 1) % count
		case "down", "tab":
			d.focus = (d.focus + 1) % count
		case "enter", "space":
			return d.selectField()
		case "ctrl+s":
			return d.save()
		case "ctrl+r":
			d.busy = true
			return ActionCmd{Cmd: d.loadCmd()}
		}
	case tea.MouseClickMsg:
		if msg.Button == uv.MouseLeft && !d.busy {
			for _, hit := range d.hits {
				if msg.X >= hit.area.Min.X && msg.X < hit.area.Max.X && msg.Y >= hit.area.Min.Y && msg.Y < hit.area.Max.Y {
					d.focus = hit.field
					return d.selectField()
				}
			}
		}
	}
	return nil
}

func (d *PathTranslation) censorImages() bool {
	if d.policy.Settings.CensorImages != nil {
		return *d.policy.Settings.CensorImages
	}
	for _, target := range d.policy.Settings.Targets {
		if !target.CensorImages {
			return false
		}
	}
	return true
}

func (d *PathTranslation) selectField() Action {
	if !d.initialized {
		return nil
	}
	switch d.focus {
	case pathTranslationEnabled:
		d.policy.Settings.Enabled = !d.policy.Settings.Enabled
		d.policy.Settings.Inherit = false
	case pathTranslationImages:
		value := !d.censorImages()
		d.policy.Settings.CensorImages = &value
		d.policy.Settings.Inherit = false
		for index := range d.policy.Settings.Targets {
			d.policy.Settings.Targets[index].CensorImages = value
		}
	case pathTranslationAdd:
		d.browser = &pathTranslationFoldersDialog{com: d.com, directory: filepath.Clean(d.com.Workspace.WorkingDir()), busy: true}
		return ActionCmd{Cmd: d.browser.loadCmd()}
	default:
		index := (d.focus - pathTranslationFolders) / 2
		if index < 0 || index >= len(d.policy.Settings.Targets) {
			return nil
		}
		if (d.focus-pathTranslationFolders)%2 == 0 {
			d.editing = index
			d.label.SetValue(d.policy.Settings.Targets[index].Label)
			d.label.Focus()
			return nil
		}
		d.policy.Settings.Targets = append(d.policy.Settings.Targets[:index], d.policy.Settings.Targets[index+1:]...)
		d.focus = min(d.focus, pathTranslationFolders+2*len(d.policy.Settings.Targets)-1)
	}
	return d.save()
}

func (d *PathTranslation) editLabel(msg tea.Msg) Action {
	if press, ok := msg.(tea.KeyPressMsg); ok {
		switch press.String() {
		case "esc":
			d.editing = -1
			d.label.Blur()
			d.lastError = ""
			return nil
		case "enter":
			settings := d.policy.Settings
			settings.Targets = append([]pathtranslate.Target(nil), settings.Targets...)
			settings.Targets[d.editing].Label = d.label.Value()
			if err := settings.Validate(); err != nil {
				d.lastError = err.Error()
				return nil
			}
			d.policy.Settings = settings
			d.editing = -1
			d.label.Blur()
			return d.save()
		}
	}
	var cmd tea.Cmd
	d.label, cmd = d.label.Update(msg)
	return ActionCmd{Cmd: cmd}
}

func (d *PathTranslation) save() Action {
	if !d.initialized || d.busy {
		return nil
	}
	policy := d.policy
	policy.Settings.Targets = append([]pathtranslate.Target(nil), policy.Settings.Targets...)
	if err := policy.Settings.Validate(); err != nil {
		d.lastError = err.Error()
		return nil
	}
	d.busy = true
	d.lastError = ""
	ws := d.com.Workspace
	return ActionCmd{Cmd: func() tea.Msg {
		saved, err := ws.SavePathTranslation(context.Background(), policy)
		return pathTranslationResultMsg{owner: d, policy: saved, err: err, saved: err == nil}
	}}
}

func (d *PathTranslation) loadCmd() tea.Cmd {
	scope, ws := d.scope, d.com.Workspace
	return func() tea.Msg {
		policy, err := ws.LoadPathTranslation(context.Background(), scope)
		return pathTranslationResultMsg{owner: d, policy: policy, err: err}
	}
}

func (d *PathTranslation) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	if d.browser != nil {
		return d.browser.Draw(scr, area)
	}
	check := func(value bool) string {
		if value {
			return "[x] "
		}
		return "[ ] "
	}
	rows := []pathTranslationRow{
		{check(d.policy.Settings.Enabled) + "Enable path translation", pathTranslationEnabled},
		{check(d.censorImages()) + "Censor image copies sent to the agent", pathTranslationImages},
		{"Only this session: " + d.scope.SessionID, -1},
		{"Folder labels: custom or automatic. Contents: unique IDs.", -1},
		{"", -1},
		{"[+ Add a folder]", pathTranslationAdd},
	}
	if len(d.policy.Settings.Targets) == 0 {
		rows = append(rows, pathTranslationRow{"No folders selected.", -1})
	}
	for index, target := range d.policy.Settings.Targets {
		label := target.Label
		if label == "" {
			label = "Automatic (pt_…)"
		}
		if d.editing == index {
			d.label.SetWidth(max(1, min(88, area.Dx())-20))
			label = d.label.View()
		}
		rows = append(rows, pathTranslationRow{pathTranslationDisplayPath(target.Path, min(88, area.Dx())-8), -1}, pathTranslationRow{"Agent sees: " + label + "  [Edit label]", pathTranslationFolders + 2*index}, pathTranslationRow{"[Remove folder]", pathTranslationFolders + 2*index + 1})
	}
	status := d.status
	if d.editing >= 0 {
		status = "Type a label, or leave blank for automatic. Enter saves; Esc cancels."
	}
	if d.busy {
		status = "Saving / loading…"
	}
	if d.lastError != "" {
		status = d.lastError + "  Ctrl+S retry / Ctrl+R reload"
	}
	d.hits = drawPathTranslationRows(d.com, scr, area, "Path Translation", rows, d.focus, status, "↑↓/tab move • enter/space select • esc close")
	return nil
}

func drawPathTranslationRows(com *common.Common, scr uv.Screen, area uv.Rectangle, title string, rows []pathTranslationRow, focus int, status, hint string) []pathTranslationHit {
	t := com.Styles
	width := max(0, min(88, area.Dx()))
	inner := max(0, width-t.Dialog.View.GetHorizontalFrameSize())
	titleHeight := lipgloss.Height(t.Dialog.Title.Render("x"))
	available := max(1, area.Dy()-t.Dialog.View.GetVerticalFrameSize()-titleHeight-2)
	selected := 0
	for index, row := range rows {
		if row.field == focus {
			selected = index
			break
		}
	}
	start := max(0, min(selected-available+1, len(rows)-available))
	end := min(len(rows), start+available)
	parts := make([]string, 0, end-start+2)
	for _, row := range rows[start:end] {
		style := t.Dialog.NormalItem
		if row.field < 0 {
			style = t.Dialog.SecondaryText
		} else if row.field == focus {
			style = t.Dialog.SelectedItem
		}
		marker := "  "
		if row.field >= 0 && row.field == focus {
			marker = "› "
		}
		parts = append(parts, style.Render(ansi.Truncate(marker+row.label, max(0, inner-style.GetHorizontalFrameSize()), "…")))
	}
	if start > 0 || end < len(rows) {
		status = fmt.Sprintf("%d–%d/%d · %s", start+1, end, len(rows), status)
	}
	parts = append(parts, t.Dialog.SecondaryText.Render(ansi.Truncate(status, inner, "…")), t.Dialog.SecondaryText.Render(ansi.Truncate(hint, inner, "…")))
	rc := NewRenderContext(t, width)
	rc.Title = title
	rc.AddPart(lipgloss.JoinVertical(lipgloss.Left, parts...))
	view := rc.Render()
	w, h := lipgloss.Size(view)
	rect := common.CenterRect(area, min(w, area.Dx()), min(h, area.Dy()))
	uv.NewStyledString(view).Draw(scr, rect)
	y := rect.Min.Y + t.Dialog.View.GetBorderTopSize() + t.Dialog.View.GetPaddingTop() + titleHeight
	x := rect.Min.X + t.Dialog.View.GetBorderLeftSize() + t.Dialog.View.GetPaddingLeft()
	var hits []pathTranslationHit
	for index, row := range rows[start:end] {
		height := lipgloss.Height(parts[index])
		if row.field >= 0 {
			hits = append(hits, pathTranslationHit{area: uv.Rect(x, y, inner, height), field: row.field})
		}
		y += height
	}
	return hits
}

func pathTranslationDisplayPath(path string, width int) string {
	width = max(1, width)
	if cells := ansi.StringWidth(path); cells > width {
		return ansi.TruncateLeft(path, cells-width+1, "…")
	}
	return path
}
