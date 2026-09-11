package tools

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
	"github.com/stretchr/testify/require"
)

func TestDiagnosticWindowBoundsEntriesAndPreservesCounts(t *testing.T) {
	var window diagnosticWindow
	for i := range 100 {
		severity := protocol.SeverityWarning
		if i%2 == 0 {
			severity = protocol.SeverityError
		}
		window.add(diagnosticEntry{path: fmt.Sprintf("file-%03d.go", i), source: "test", diagnostic: protocol.Diagnostic{Severity: severity, Message: "diagnostic"}})
	}
	require.Len(t, window.entries, 10)
	require.Equal(t, 100, window.total)
	require.Equal(t, 50, window.errors)
	require.Equal(t, 50, window.warnings)
	for _, entry := range window.entries {
		require.Equal(t, protocol.SeverityError, entry.diagnostic.Severity)
	}
	var output strings.Builder
	window.write(&output, "project_diagnostics")
	require.Contains(t, output.String(), "... and 90 more diagnostics")
	require.Equal(t, 10, strings.Count(output.String(), "Error:"))
}
