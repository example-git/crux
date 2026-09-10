package projects

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	noteIndexPageSize = 50
	noteReadPageSize  = 6000
)

type Note struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Content string `json:"content,omitempty"`
}

type NotePage struct {
	Project    string `json:"project"`
	Entries    []Note `json:"entries"`
	Total      int    `json:"total"`
	NextOffset *int   `json:"next_offset,omitempty"`
}

type NoteDetail struct {
	Project string `json:"project"`
	Note
	NextOffset *int `json:"next_offset,omitempty"`
}

func (d Document) noteEntries() []Note {
	var entries []Note
	var chunk []string
	seen := make(map[string]int)
	flush := func() {
		content := strings.TrimSpace(strings.Join(chunk, "\n"))
		chunk = nil
		if content == "" {
			return
		}
		title := "Project note"
		for _, line := range strings.Split(content, "\n") {
			line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#"))
			if line == "" {
				continue
			}
			if _, err := time.Parse(time.RFC3339, line); err == nil {
				continue
			}
			title = strings.Join(strings.Fields(line), " ")
			break
		}
		runes := []rune(title)
		if len(runes) > 100 {
			title = string(runes[:99]) + "…"
		}
		digest := sha256.Sum256([]byte(d.Metadata.Slug + "\n" + content))
		id := fmt.Sprintf("%s:%x", d.Metadata.Slug, digest[:8])
		seen[id]++
		if seen[id] > 1 {
			id = fmt.Sprintf("%s-%d", id, seen[id])
		}
		entries = append(entries, Note{ID: id, Title: title, Content: content})
	}
	fence := ""
	for index, line := range strings.Split(strings.ReplaceAll(d.Notes, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if index == 0 && strings.HasPrefix(trimmed, "# ") {
			continue
		}
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			marker := trimmed[:3]
			switch fence {
			case "":
				fence = marker
			case marker:
				fence = ""
			}
		}
		boundary := strings.HasPrefix(line, "## ")
		if strings.HasPrefix(line, "- ") {
			fields := strings.Fields(line)
			if len(fields) > 1 {
				_, err := time.Parse(time.RFC3339, fields[1])
				boundary = err == nil
			}
		}
		if fence == "" && boundary {
			pending := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(strings.Join(chunk, "\n")), "## "))
			if _, err := time.Parse(time.RFC3339, pending); err != nil {
				flush()
			}
		}
		chunk = append(chunk, line)
	}
	flush()
	return entries
}

func (d Document) NotesIndex(offset int) (NotePage, error) {
	entries := d.noteEntries()
	page := NotePage{Project: d.Metadata.Slug, Entries: []Note{}, Total: len(entries)}
	if offset < 0 || offset > len(entries) {
		return NotePage{}, errors.New("project note index offset is out of range")
	}
	end := min(offset+noteIndexPageSize, len(entries))
	for index := offset; index < end; index++ {
		entry := entries[len(entries)-1-index]
		entry.Content = ""
		page.Entries = append(page.Entries, entry)
	}
	if end < len(entries) {
		page.NextOffset = &end
	}
	return page, nil
}

func (s *Service) ListNotes(workingDir string, offset int) (NotePage, error) {
	document, ok, err := s.Active(workingDir)
	if err != nil {
		return NotePage{}, err
	}
	if !ok {
		return NotePage{}, errors.New("no active project matches the current workspace")
	}
	return document.NotesIndex(offset)
}

func (s *Service) ReadNote(workingDir, topic string, offset int) (NoteDetail, error) {
	document, ok, err := s.Active(workingDir)
	if err != nil {
		return NoteDetail{}, err
	}
	if !ok {
		return NoteDetail{}, errors.New("no active project matches the current workspace")
	}
	for _, entry := range document.noteEntries() {
		if entry.ID != topic {
			continue
		}
		content := []rune(entry.Content)
		if offset < 0 || offset > len(content) {
			return NoteDetail{}, errors.New("project note read offset is out of range")
		}
		end := min(offset+noteReadPageSize, len(content))
		entry.Content = string(content[offset:end])
		result := NoteDetail{Project: document.Metadata.Slug, Note: entry}
		if end < len(content) {
			result.NextOffset = &end
		}
		return result, nil
	}
	return NoteDetail{}, fmt.Errorf("note %q does not exist in active project %q; list the current index", topic, document.Metadata.Slug)
}
