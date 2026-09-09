package model

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/agent"
	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/diff"
	"github.com/example-git/crux/internal/fsext"
	"github.com/example-git/crux/internal/history"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/session"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/example-git/crux/internal/workspace"
)

// loadSessionMsg is a message indicating that a session and its files have
// been loaded.
type loadSessionMsg struct {
	source      workspace.Workspace
	workspaceID string
	generation  uint64
	messages    []message.Message
	nested      map[string][]message.Message
	err         error
	session     *session.Session
	files       []SessionFile
	readFiles   []string
}

// lspFilePaths returns deduplicated file paths from both modified and read
// files for starting LSP servers.
func (msg loadSessionMsg) lspFilePaths() []string {
	seen := make(map[string]struct{}, len(msg.files)+len(msg.readFiles))
	paths := make([]string, 0, len(msg.files)+len(msg.readFiles))
	for _, f := range msg.files {
		p := f.LatestVersion.Path
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		paths = append(paths, p)
	}
	for _, p := range msg.readFiles {
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		paths = append(paths, p)
	}
	return paths
}

// SessionFile tracks the first and latest versions of a file in a session,
// along with the total additions and deletions.
type SessionFile struct {
	FirstVersion  history.File
	LatestVersion history.File
	Created       bool
	Additions     int
	Deletions     int
}

// loadSession loads the session along with its associated files and computes
// the diff statistics (additions and deletions) for each file in the session.
// It returns a tea.Cmd that, when executed, fetches the session data and
// returns a loadSessionMsg containing the session and its history.
//
// Presence is reported only after Update accepts this captured load. An old
// completion cannot change presence or install history in another workspace.
func (m *UI) loadSession(sessionID string) tea.Cmd {
	ws := m.com.Workspace
	id := ws.AuthenticationWorkspaceID()
	m.sessionLoadGeneration++
	generation := m.sessionLoadGeneration
	return loadSessionCommand(ws, id, generation, sessionID)
}

func loadSessionCommand(ws workspace.Workspace, id string, generation uint64, sessionID string) tea.Cmd {
	return func() tea.Msg {
		result := loadSessionMsg{source: ws, workspaceID: id, generation: generation}
		ctx := workspace.ContextWithSessionWorkspace(context.Background(), id)
		value, err := ws.GetSession(ctx, sessionID)
		if err != nil {
			result.err = err
			return result
		}
		result.session = &value
		result.files, err = loadSessionFiles(ctx, ws, sessionID)
		if err != nil {
			result.err = err
			return result
		}
		result.readFiles, err = ws.FileTrackerListReadFiles(ctx, sessionID)
		if err != nil {
			slog.Error("Failed to load read files for session", "error", err)
		}
		result.messages, result.err = ws.ListMessages(ctx, sessionID)
		if result.err != nil {
			return result
		}
		result.nested = loadNestedSessionMessages(ctx, ws, result.messages)
		return result
	}
}

// Preload nested history under the same captured Workspace ID. Rendering only
// consumes these returned messages; it performs no Workspace IO in Update.
func loadNestedSessionMessages(ctx context.Context, ws workspace.Workspace, messages []message.Message) map[string][]message.Message {
	result := map[string][]message.Message{}
	queue := append([]message.Message(nil), messages...)
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, call := range current.ToolCalls() {
			if call.Name != agent.AgentToolName && call.Name != tools.AgenticFetchToolName {
				continue
			}
			id := ws.CreateAgentToolSessionID(current.ID, call.ID)
			if _, seen := result[id]; seen {
				continue
			}
			result[id] = nil
			nested, err := ws.ListMessages(ctx, id)
			if err != nil {
				continue
			}
			result[id] = nested
			queue = append(queue, nested...)
		}
	}
	return result
}

// reportCurrentSession returns a fire-and-forget tea.Cmd that
// informs the workspace which session this client is currently
// viewing. Errors are logged at debug only; the call is a hint
// for server-side presence tracking, not correctness-critical
// state.
func (m *UI) reportCurrentSession(sessionID string) tea.Cmd {
	ws := m.com.Workspace
	id := ws.AuthenticationWorkspaceID()
	return func() tea.Msg {
		ctx := workspace.ContextWithSessionWorkspace(context.Background(), id)
		if err := ws.SetCurrentSession(ctx, sessionID); err != nil {
			slog.Debug("Failed to report current session", "session_id", sessionID, "error", err)
		}
		return nil
	}
}

func loadSessionFiles(ctx context.Context, ws workspace.Workspace, sessionID string) ([]SessionFile, error) {
	files, err := ws.ListSessionHistory(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	filesByPath := make(map[string][]history.File)
	for _, f := range files {
		filesByPath[f.Path] = append(filesByPath[f.Path], f)
	}
	sessionFiles := make([]SessionFile, 0, len(filesByPath))
	for _, versions := range filesByPath {
		if len(versions) == 0 {
			continue
		}

		first := versions[0]
		last := versions[0]
		for _, v := range versions {
			if v.Version < first.Version {
				first = v
			}
			if v.Version > last.Version {
				last = v
			}
		}

		_, additions, deletions := diff.GenerateDiff(first.Content, last.Content, first.Path)

		sessionFiles = append(sessionFiles, SessionFile{
			FirstVersion:  first,
			LatestVersion: last,
			Created:       !first.Exists && last.Exists,
			Additions:     additions,
			Deletions:     deletions,
		})
	}

	slices.SortFunc(sessionFiles, func(a, b SessionFile) int {
		if a.LatestVersion.UpdatedAt > b.LatestVersion.UpdatedAt {
			return -1
		}
		if a.LatestVersion.UpdatedAt < b.LatestVersion.UpdatedAt {
			return 1
		}
		return 0
	})
	return sessionFiles, nil
}

// handleFileEvent processes file change events and updates the session file
// list with new or updated file information.
func (m *UI) handleFileEvent(file history.File) tea.Cmd {
	if m.session == nil || file.SessionID != m.session.ID {
		return nil
	}

	sessionID := m.session.ID
	m.sessionFilesFetchGen++
	generation := m.sessionFilesFetchGen
	ws := m.com.Workspace
	id := ws.AuthenticationWorkspaceID()
	return func() tea.Msg {
		ctx := workspace.ContextWithSessionWorkspace(context.Background(), id)
		sessionFiles, err := loadSessionFiles(ctx, ws, sessionID)
		return sessionFilesUpdatesMsg{
			err:    err,
			source: ws, workspaceID: id,
			sessionID:    sessionID,
			generation:   generation,
			sessionFiles: sessionFiles,
		}
	}
}

// filesInfo renders the modified files section for the sidebar, showing files
// with their addition/deletion counts.
func (m *UI) filesInfo(cwd string, width, maxItems int, isSection bool) string {
	t := m.com.Styles

	title := t.Files.SectionTitle.Render("Modified Files")
	if isSection {
		indicator := "▾"
		if m.sidebarFilesCollapsed {
			indicator = "▸"
		}
		title = common.Section(t, fmt.Sprintf("%s Modified Files", indicator), width, fmt.Sprintf("%d", fileChangeCount(m.sessionFiles)))
		if m.sidebarFilesCollapsed {
			return lipgloss.NewStyle().Width(width).Render(title)
		}
	}
	list := t.Files.EmptyMessage.Render("None")
	var filesWithChanges []SessionFile
	for _, f := range m.sessionFiles {
		if !f.Created && f.Additions == 0 && f.Deletions == 0 {
			continue
		}
		filesWithChanges = append(filesWithChanges, f)
	}
	if len(filesWithChanges) > 0 {
		list = fileList(t, cwd, filesWithChanges, width, maxItems)
	}

	return lipgloss.NewStyle().Width(width).Render(fmt.Sprintf("%s\n\n%s", title, list))
}

// fileList renders a list of files with their diff statistics, truncating to
// maxItems and showing a "...and N more" message if needed.
func fileList(t *styles.Styles, cwd string, filesWithChanges []SessionFile, width, maxItems int) string {
	if maxItems <= 0 {
		return ""
	}
	var renderedFiles []string
	filesShown := 0

	for _, f := range filesWithChanges {
		// Skip files with no changes
		if filesShown >= maxItems {
			break
		}

		// Build stats string with colors
		var statusParts []string
		if f.Created {
			statusParts = append(statusParts, t.Files.Additions.Render("new"))
		}
		if f.Additions > 0 {
			statusParts = append(statusParts, t.Files.Additions.Render(fmt.Sprintf("+%d", f.Additions)))
		}
		if f.Deletions > 0 {
			statusParts = append(statusParts, t.Files.Deletions.Render(fmt.Sprintf("-%d", f.Deletions)))
		}
		extraContent := strings.Join(statusParts, " ")

		// Format file path
		filePath := f.FirstVersion.Path
		if rel, err := filepath.Rel(cwd, filePath); err == nil {
			filePath = rel
		}
		filePath = fsext.DirTrim(filePath, 2)
		suffix := ""
		if extraContent != "" {
			suffix = " " + extraContent
		}
		maxPathWidth := max(width-lipgloss.Width(suffix), 0)
		filePath = ansi.Truncate(filePath, maxPathWidth, "…")

		line := t.Files.Path.Render(filePath)
		if extraContent != "" {
			line = fmt.Sprintf("%s %s", line, extraContent)
		}

		renderedFiles = append(renderedFiles, line)
		filesShown++
	}

	if len(filesWithChanges) > maxItems {
		remaining := len(filesWithChanges) - maxItems
		renderedFiles = append(renderedFiles, t.Files.TruncationHint.Render(fmt.Sprintf("…and %d more", remaining)))
	}

	return lipgloss.JoinVertical(lipgloss.Left, renderedFiles...)
}

// startLSPs starts LSP servers for the given file paths.
func (m *UI) startLSPs(paths []string) tea.Cmd {
	if len(paths) == 0 {
		return nil
	}

	return func() tea.Msg {
		ctx := context.Background()
		for _, path := range paths {
			m.com.Workspace.LSPStart(ctx, path)
		}
		return nil
	}
}
