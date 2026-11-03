package dialog

import (
	"fmt"
	"os"
	"path/filepath"

	tea "github.com/example-git/crux/foundation/bubbletea"
	uv "github.com/example-git/crux/foundation/ultraviolet"
	"github.com/example-git/crux/internal/pathtranslate"
	"github.com/example-git/crux/internal/ui/common"
)

type pathTranslationDirectoryMsg struct {
	owner *pathTranslationFoldersDialog
	paths []string
	err   error
}

type pathTranslationFoldersDialog struct {
	com       *common.Common
	directory string
	paths     []string
	focus     int
	busy      bool
	lastError string
	hits      []pathTranslationHit
}

func (b *pathTranslationFoldersDialog) loadCmd() tea.Cmd {
	directory := b.directory
	return func() tea.Msg {
		if !filepath.IsAbs(directory) {
			return pathTranslationDirectoryMsg{owner: b, err: fmt.Errorf("workspace directory is not an absolute path: %s", directory)}
		}
		entries, err := os.ReadDir(directory)
		if err != nil {
			return pathTranslationDirectoryMsg{owner: b, err: err}
		}
		var paths []string
		for _, entry := range entries {
			isDir := entry.IsDir()
			if entry.Type()&os.ModeSymlink != 0 {
				info, err := os.Stat(filepath.Join(directory, entry.Name()))
				isDir = err == nil && info.IsDir()
			}
			if isDir {
				paths = append(paths, filepath.Join(directory, entry.Name()))
			}
		}
		return pathTranslationDirectoryMsg{owner: b, paths: paths}
	}
}

func (d *PathTranslation) handleFolderMsg(msg tea.Msg) Action {
	b := d.browser
	switch msg := msg.(type) {
	case pathTranslationDirectoryMsg:
		if msg.owner != b {
			return nil
		}
		b.busy = false
		b.paths = msg.paths
		b.lastError = ""
		if msg.err != nil {
			b.lastError = msg.err.Error()
		}
		b.focus = 0
	case tea.KeyPressMsg:
		if msg.String() == "esc" {
			d.browser = nil
			return nil
		}
		if b.busy {
			return nil
		}
		switch msg.String() {
		case "up", "shift+tab":
			b.focus = (b.focus + len(b.paths) + 1) % (len(b.paths) + 2)
		case "down", "tab":
			b.focus = (b.focus + 1) % (len(b.paths) + 2)
		case "left", "backspace":
			return b.open(filepath.Dir(b.directory))
		case "right":
			if b.focus >= 2 {
				return b.open(b.paths[b.focus-2])
			}
		case "enter":
			return d.selectFolderRow()
		case "space":
			if b.focus >= 2 {
				return d.addFolder(b.paths[b.focus-2])
			}
			if b.focus == 0 {
				return d.addFolder(b.directory)
			}
		case "ctrl+r":
			return b.open(b.directory)
		}
	case tea.MouseClickMsg:
		if msg.Button == uv.MouseLeft && !b.busy {
			for _, hit := range b.hits {
				if msg.X >= hit.area.Min.X && msg.X < hit.area.Max.X && msg.Y >= hit.area.Min.Y && msg.Y < hit.area.Max.Y {
					b.focus = hit.field
					return d.selectFolderRow()
				}
			}
		}
	}
	return nil
}

func (b *pathTranslationFoldersDialog) open(path string) Action {
	b.directory = filepath.Clean(path)
	b.paths = nil
	b.busy = true
	b.lastError = ""
	return ActionCmd{Cmd: b.loadCmd()}
}

func (d *PathTranslation) selectFolderRow() Action {
	b := d.browser
	switch b.focus {
	case 0:
		return d.addFolder(b.directory)
	case 1:
		return b.open(filepath.Dir(b.directory))
	default:
		return b.open(b.paths[b.focus-2])
	}
}

func (d *PathTranslation) addFolder(path string) Action {
	b := d.browser
	if b.lastError != "" {
		return nil
	}
	path = filepath.Clean(path)
	for _, target := range d.policy.Settings.Targets {
		if target.Path == path {
			d.browser = nil
			d.status = "Folder already added"
			return nil
		}
	}
	target := pathtranslate.Target{Path: path, Recursive: true, CensorImages: d.censorImages()}
	settings := d.policy.Settings
	settings.Inherit = false
	settings.Targets = append(append([]pathtranslate.Target(nil), settings.Targets...), target)
	if err := settings.Validate(); err != nil {
		b.lastError = err.Error()
		return nil
	}
	d.policy.Settings = settings
	d.browser = nil
	d.focus = pathTranslationAdd
	return d.save()
}

func (b *pathTranslationFoldersDialog) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	rows := []pathTranslationRow{
		{"Current folder: " + filepath.Base(b.directory), -1},
		{pathTranslationDisplayPath(b.directory, min(88, area.Dx())-8), -1},
		{"[Add this folder]", 0},
		{"[.. Parent folder]", 1},
	}
	for index, path := range b.paths {
		rows = append(rows, pathTranslationRow{filepath.Base(path) + "/  ›", index + 2})
	}
	status := "Enter opens a folder. Space adds the highlighted folder."
	if b.busy {
		status = "Reading this directory…"
	} else if b.lastError != "" {
		status = b.lastError
	} else if len(b.paths) == 0 {
		rows = append(rows, pathTranslationRow{"No subfolders in this directory.", -1})
	}
	b.hits = drawPathTranslationRows(b.com, scr, area, "Add a folder", rows, b.focus, status, "↑↓ move • enter open/add • space add • ← parent • esc cancel")
	return nil
}
