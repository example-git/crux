package dialog

import (
	"context"
	"slices"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/example-git/crux/internal/workspace"
)

type codebaseIndexTestWorkspace struct {
	workspace.Workspace
	cfg         *config.Config
	status      proto.CodebaseIndexStatus
	updates     []proto.CodebaseIndexUpdate
	statusCalls int
}

func (w *codebaseIndexTestWorkspace) Config() *config.Config {
	return w.cfg
}

func (w *codebaseIndexTestWorkspace) CodebaseIndexStatus(context.Context) (proto.CodebaseIndexStatus, error) {
	w.statusCalls++
	return w.status, nil
}

func (w *codebaseIndexTestWorkspace) UpdateCodebaseIndex(_ context.Context, update proto.CodebaseIndexUpdate) (proto.CodebaseIndexStatus, error) {
	w.updates = append(w.updates, update)
	w.status = proto.CodebaseIndexStatus{
		Enabled:        update.Enabled,
		State:          "indexing",
		DatabasePath:   update.DatabasePath,
		StoreDirectory: update.StoreDirectory,
		IncludePaths:   update.IncludePaths,
		ExcludePaths:   update.ExcludePaths,
	}
	return w.status, nil
}

func newCodebaseIndexTestDialog(t *testing.T, settings config.ToolCodebaseSearch, status proto.CodebaseIndexStatus) (*CodebaseIndex, tea.Cmd, *codebaseIndexTestWorkspace) {
	t.Helper()
	ws := &codebaseIndexTestWorkspace{
		cfg:    &config.Config{Tools: config.Tools{CodebaseSearch: settings}},
		status: status,
	}
	theme := styles.ThemeForProvider("")
	dialog, cmd := NewCodebaseIndex(&common.Common{Workspace: ws, Styles: &theme})
	return dialog, cmd, ws
}

func TestCodebaseIndexLoadsSettingsAndStatus(t *testing.T) {
	disabled := false
	dialog, cmd, ws := newCodebaseIndexTestDialog(t, config.ToolCodebaseSearch{
		Enabled:        &disabled,
		DatabasePath:   "/indexes/source.db",
		StoreDirectory: "/indexes/store",
		IncludePaths:   []string{"src", "internal"},
		ExcludePaths:   []string{"src/generated"},
	}, proto.CodebaseIndexStatus{Enabled: false, State: "indexing"})

	if dialog.enabled {
		t.Fatal("dialog enabled indexing despite disabled configuration")
	}
	if got := dialog.database.Value(); got != "/indexes/source.db" {
		t.Fatalf("database input = %q", got)
	}
	if got := dialog.store.Value(); got != "/indexes/store" {
		t.Fatalf("store input = %q", got)
	}
	if got := dialog.include.Value(); got != "src, internal" {
		t.Fatalf("include input = %q", got)
	}
	if got := dialog.exclude.Value(); got != "src/generated" {
		t.Fatalf("exclude input = %q", got)
	}
	if ws.statusCalls != 0 {
		t.Fatal("constructor performed status I/O synchronously")
	}

	action := dialog.HandleMsg(cmd())
	if ws.statusCalls != 1 {
		t.Fatalf("status calls = %d, want 1", ws.statusCalls)
	}
	if dialog.status.State != "indexing" {
		t.Fatalf("status state = %q", dialog.status.State)
	}
	if _, ok := action.(ActionCmd); !ok {
		t.Fatalf("indexing result action = %T, want ActionCmd poll", action)
	}
}

func TestCodebaseIndexSaveRunsAsynchronously(t *testing.T) {
	dialog, _, ws := newCodebaseIndexTestDialog(t, config.ToolCodebaseSearch{}, proto.CodebaseIndexStatus{})
	dialog.enabled = true
	dialog.database.SetValue(" /indexes/source.db ")
	dialog.store.SetValue(" /indexes/store ")
	dialog.include.SetValue("src, ./internal, cmd")
	dialog.exclude.SetValue("vendor, generated")

	action, ok := dialog.save(false).(ActionCmd)
	if !ok || action.Cmd == nil {
		t.Fatalf("save action = %#v", action)
	}
	if len(ws.updates) != 0 {
		t.Fatal("save performed workspace I/O synchronously")
	}

	result := action.Cmd()
	if len(ws.updates) != 1 {
		t.Fatalf("updates = %d, want 1", len(ws.updates))
	}
	update := ws.updates[0]
	if !update.Enabled {
		t.Fatal("saved update disabled indexing")
	}
	if update.Reindex {
		t.Fatal("saving settings unexpectedly requested reindex")
	}
	if update.DatabasePath != "/indexes/source.db" || update.StoreDirectory != "/indexes/store" {
		t.Fatalf("saved paths = %q, %q", update.DatabasePath, update.StoreDirectory)
	}
	if !slices.Equal(update.IncludePaths, []string{"src", "./internal", "cmd"}) {
		t.Fatalf("include paths = %#v", update.IncludePaths)
	}
	if !slices.Equal(update.ExcludePaths, []string{"vendor", "generated"}) {
		t.Fatalf("exclude paths = %#v", update.ExcludePaths)
	}

	dialog.HandleMsg(result)
	if dialog.busy {
		t.Fatal("dialog remained busy after save result")
	}
	if dialog.status.State != "indexing" {
		t.Fatalf("status state = %q", dialog.status.State)
	}
}

func TestCodebaseIndexNowRequestsReindexAsynchronously(t *testing.T) {
	dialog, _, ws := newCodebaseIndexTestDialog(t, config.ToolCodebaseSearch{}, proto.CodebaseIndexStatus{State: "failed"})
	action, ok := dialog.save(true).(ActionCmd)
	if !ok || action.Cmd == nil {
		t.Fatalf("index action = %#v", action)
	}
	if len(ws.updates) != 0 {
		t.Fatal("index action performed workspace I/O synchronously")
	}
	action.Cmd()
	if len(ws.updates) != 1 || !ws.updates[0].Reindex {
		t.Fatalf("updates = %#v, want one reindex request", ws.updates)
	}
}

func TestCodebaseIndexCheckStatusDoesNotReindex(t *testing.T) {
	dialog, _, ws := newCodebaseIndexTestDialog(t, config.ToolCodebaseSearch{}, proto.CodebaseIndexStatus{State: "stale"})
	action, ok := dialog.refresh().(ActionCmd)
	if !ok || action.Cmd == nil {
		t.Fatalf("refresh action = %#v", action)
	}
	action.Cmd()
	if ws.statusCalls != 1 {
		t.Fatalf("status calls = %d, want 1", ws.statusCalls)
	}
	if len(ws.updates) != 0 {
		t.Fatalf("check status updates = %#v", ws.updates)
	}
}

func TestCodebaseIndexToggleAndPathParsing(t *testing.T) {
	dialog, _, _ := newCodebaseIndexTestDialog(t, config.ToolCodebaseSearch{}, proto.CodebaseIndexStatus{})
	if dialog.enabled {
		t.Fatal("default configuration should disable indexing")
	}
	dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	if !dialog.enabled {
		t.Fatal("space did not enable indexing")
	}

	paths := splitCodebaseIndexPaths(" src, internal/test\n\n cmd ")
	if !slices.Equal(paths, []string{"src", "internal/test", "cmd"}) {
		t.Fatalf("paths = %#v", paths)
	}
}

func TestCodebaseIndexStatusLabels(t *testing.T) {
	now := time.Date(2026, time.August, 20, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		status   proto.CodebaseIndexStatus
		expected string
	}{
		{proto.CodebaseIndexStatus{State: "disabled"}, "Disabled"},
		{proto.CodebaseIndexStatus{State: "missing"}, "Index not built"},
		{proto.CodebaseIndexStatus{State: "indexing", Stage: "Preparing files"}, "Preparing files…"},
		{proto.CodebaseIndexStatus{State: "indexing", FilesProcessed: 3, FilesTotal: 10, ChunksCreated: 24, FilesSkipped: 1}, "Indexing: 3/10 files, 24 chunks, 1 skipped"},
		{proto.CodebaseIndexStatus{State: "indexing", Serving: true, FilesProcessed: 3, FilesTotal: 10, ChunksCreated: 24}, "Refreshing: 3/10 files, 24 chunks"},
		{proto.CodebaseIndexStatus{State: "indexing", Serving: true, Stage: "Preparing files"}, "Refreshing: Preparing files…"},
		{proto.CodebaseIndexStatus{State: "ready", FilesTotal: 10, ChunksCreated: 24, FinishedAt: now.Add(-5 * time.Minute)}, "Ready: 10 files, 24 chunks (5m ago)"},
		{proto.CodebaseIndexStatus{State: "ready", FilesTotal: 10, FilesProcessed: 10, ChunksCreated: 24, FilesSkipped: 2}, "Ready: 10 files, 24 chunks, 2 skipped"},
		{proto.CodebaseIndexStatus{State: "stale"}, "Index changed; update recommended"},
		{proto.CodebaseIndexStatus{State: "stale", Serving: true}, "Serving current index; refresh pending"},
		{proto.CodebaseIndexStatus{State: "failed"}, "Index failed; retry available"},
		{proto.CodebaseIndexStatus{State: "failed", Serving: true}, "Serving current index; refresh failed"},
		{proto.CodebaseIndexStatus{}, "Checking index…"},
	}
	for _, test := range cases {
		if got := codebaseIndexStatusLabel(test.status, now); got != test.expected {
			t.Fatalf("status %#v label = %q, want %q", test.status, got, test.expected)
		}
	}
}

func TestCodebaseIndexCredentialLabels(t *testing.T) {
	if got := codebaseIndexCredentialLabel("signed-in"); got != "Signed in" {
		t.Fatalf("signed-in label = %q", got)
	}
	if got := codebaseIndexCredentialLabel("missing"); got != "Not signed in" {
		t.Fatalf("missing label = %q", got)
	}
	if got := codebaseIndexCredentialLabel("invalid"); got != "Credential invalid" {
		t.Fatalf("invalid label = %q", got)
	}
}
