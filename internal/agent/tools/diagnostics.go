package tools

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/lsp"
)

type DiagnosticsParams struct {
	FilePath string `json:"file_path,omitempty" description:"The path to the file to get diagnostics for (leave empty for project diagnostics)"`
}

const DiagnosticsToolName = "lsp_diagnostics"

//go:embed diagnostics.md
var diagnosticsDescription string

func NewDiagnosticsTool(lspManager *lsp.Manager) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		DiagnosticsToolName,
		diagnosticsDescription,
		func(ctx context.Context, params DiagnosticsParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			notifyLSPs(ctx, lspManager, params.FilePath)
			output := getDiagnostics(params.FilePath, lspManager)
			return fantasy.NewTextResponse(output), nil
		},
	)
}

// notifyLSPs notifies LSP servers that a file has changed and waits for
// updated diagnostics. Use this after edit/multiedit operations.
// When filepath is empty, refreshes all open files across all LSP clients
// and sends a workspace-level change notification for full re-analysis.
func notifyLSPs(
	ctx context.Context,
	manager *lsp.Manager,
	filepath string,
) {
	if manager == nil {
		return
	}
	if filepath == "" {
		// No specific file — refresh all open files for all clients.
		var wg sync.WaitGroup
		for client := range manager.Clients().Seq() {
			wg.Go(func() {
				client.RefreshOpenFiles(ctx)
				if err := client.NotifyWorkspaceChange(ctx); err != nil {
					slog.WarnContext(ctx, "Failed to notify workspace change", "error", err)
				}
				client.WaitForDiagnostics(ctx, 5*time.Second)
			})
		}
		wg.Wait()
		return
	}

	manager.Start(ctx, filepath)

	var wg sync.WaitGroup
	for client := range manager.Clients().Seq() {
		if !client.HandlesFile(filepath) {
			continue
		}
		_ = client.OpenFileOnDemand(ctx, filepath)
		_ = client.NotifyChange(ctx, filepath)
		wg.Go(func() {
			client.WaitForDiagnostics(ctx, 5*time.Second)
		})
	}
	wg.Wait()
}

func queueLSPChange(manager *lsp.Manager, path string) {
	if manager != nil {
		manager.QueueChange(path)
	}
}

type diagnosticEntry struct {
	path       string
	source     string
	diagnostic protocol.Diagnostic
}

type diagnosticWindow struct {
	entries  []diagnosticEntry
	total    int
	errors   int
	warnings int
}

func (w *diagnosticWindow) add(entry diagnosticEntry) {
	w.total++
	if entry.diagnostic.Severity == protocol.SeverityError {
		w.errors++
	}
	if entry.diagnostic.Severity == protocol.SeverityWarning {
		w.warnings++
	}
	w.entries = append(w.entries, entry)
	sort.Slice(w.entries, func(i, j int) bool {
		a, b := w.entries[i], w.entries[j]
		aError := a.diagnostic.Severity == protocol.SeverityError
		bError := b.diagnostic.Severity == protocol.SeverityError
		if aError != bError {
			return aError
		}
		if a.path != b.path {
			return a.path < b.path
		}
		if a.diagnostic.Range.Start.Line != b.diagnostic.Range.Start.Line {
			return a.diagnostic.Range.Start.Line < b.diagnostic.Range.Start.Line
		}
		if a.diagnostic.Range.Start.Character != b.diagnostic.Range.Start.Character {
			return a.diagnostic.Range.Start.Character < b.diagnostic.Range.Start.Character
		}
		if a.source != b.source {
			return a.source < b.source
		}
		return a.diagnostic.Message < b.diagnostic.Message
	})
	if len(w.entries) > 10 {
		w.entries[10] = diagnosticEntry{}
		w.entries = w.entries[:10]
	}
}

func (w *diagnosticWindow) write(output *strings.Builder, tag string) {
	if w.total == 0 {
		return
	}
	output.WriteString("\n<" + tag + ">\n")
	for i, entry := range w.entries {
		if i > 0 {
			output.WriteByte('\n')
		}
		output.WriteString(formatDiagnostic(entry.path, entry.diagnostic, entry.source))
	}
	if w.total > len(w.entries) {
		fmt.Fprintf(output, "\n... and %d more diagnostics", w.total-len(w.entries))
	}
	output.WriteString("\n</" + tag + ">\n")
}

func getDiagnostics(filePath string, manager *lsp.Manager) string {
	if manager == nil {
		return ""
	}
	var fileDiagnostics, projectDiagnostics diagnosticWindow
	for name, client := range manager.Clients().Seq2() {
		client.RangeDiagnostics(func(location protocol.DocumentURI, diagnostic protocol.Diagnostic) {
			path, err := location.Path()
			if err != nil {
				return
			}
			entry := diagnosticEntry{path: path, source: name, diagnostic: diagnostic}
			if path == filePath {
				fileDiagnostics.add(entry)
			} else {
				projectDiagnostics.add(entry)
			}
		})
	}
	var output strings.Builder
	fileDiagnostics.write(&output, "file_diagnostics")
	projectDiagnostics.write(&output, "project_diagnostics")
	if fileDiagnostics.total > 0 || projectDiagnostics.total > 0 {
		output.WriteString("\n<diagnostic_summary>\n")
		fmt.Fprintf(&output, "Current file: %d errors, %d warnings\n", fileDiagnostics.errors, fileDiagnostics.warnings)
		fmt.Fprintf(&output, "Project: %d errors, %d warnings\n", projectDiagnostics.errors, projectDiagnostics.warnings)
		output.WriteString("</diagnostic_summary>\n")
	}
	return output.String()
}

func formatDiagnostic(pth string, diagnostic protocol.Diagnostic, source string) string {
	severity := "Info"
	switch diagnostic.Severity {
	case protocol.SeverityError:
		severity = "Error"
	case protocol.SeverityWarning:
		severity = "Warn"
	case protocol.SeverityHint:
		severity = "Hint"
	}

	location := fmt.Sprintf("%s:%d:%d", pth, diagnostic.Range.Start.Line+1, diagnostic.Range.Start.Character+1)

	sourceInfo := source
	if diagnostic.Source != "" {
		sourceInfo += " " + diagnostic.Source
	}

	codeInfo := ""
	if diagnostic.Code != nil {
		codeInfo = fmt.Sprintf("[%v]", diagnostic.Code)
	}

	tagsInfo := ""
	if len(diagnostic.Tags) > 0 {
		var tags []string
		for _, tag := range diagnostic.Tags {
			switch tag {
			case protocol.Unnecessary:
				tags = append(tags, "unnecessary")
			case protocol.Deprecated:
				tags = append(tags, "deprecated")
			}
		}
		if len(tags) > 0 {
			tagsInfo = fmt.Sprintf(" (%s)", strings.Join(tags, ", "))
		}
	}

	return fmt.Sprintf("%s: %s [%s]%s%s %s",
		severity,
		location,
		sourceInfo,
		codeInfo,
		tagsInfo,
		diagnostic.Message)
}
