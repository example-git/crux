package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/codebaseindex"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/permission"
	"github.com/stretchr/testify/require"
)

func TestCodebaseSearchDescriptionPrefersReadyIndexForConceptualDiscovery(t *testing.T) {
	require.Contains(t, codebaseSearchDescription, "Prefer this over search")
	require.Contains(t, codebaseSearchDescription, "first repository-discovery tool")
	require.Contains(t, codebaseSearchDescription, "when a completed index is available and the relevant files are indexed")
	require.Contains(t, codebaseSearchDescription, "Background refreshes keep serving the last completed index")
	require.Contains(t, codebaseSearchDescription, "Use LSP or search in content mode for known exact symbols and literals")
}

func TestCodebaseSearchToolRejectsSubagentsBeforeOpeningIndex(t *testing.T) {
	tool := NewCodebaseSearchTool(t.TempDir(), config.ToolCodebaseSearch{}, nil, nil)
	input, err := json.Marshal(CodebaseSearchParams{Query: "query"})
	require.NoError(t, err)
	response, err := tool.Run(permission.WithSubagent(t.Context()), fantasy.ToolCall{
		ID:    "test-call",
		Name:  CodebaseSearchToolName,
		Input: string(input),
	})
	require.NoError(t, err)
	require.True(t, response.IsError)
	require.Equal(t, permission.ErrSubagentCodebaseSearch.Error(), response.Content)
}

func TestCodebaseSearchToolValidatesInput(t *testing.T) {
	tool := NewCodebaseSearchTool(t.TempDir(), config.ToolCodebaseSearch{}, nil, nil)

	response := runCodebaseSearchTool(t, tool, CodebaseSearchParams{})
	require.True(t, response.IsError)
	require.Equal(t, "query is required", response.Content)

	response = runCodebaseSearchTool(t, tool, CodebaseSearchParams{Query: "query", Count: 21})
	require.True(t, response.IsError)
	require.Equal(t, "count must be between 1 and 20", response.Content)

	invalidMinScore := 2.0
	response = runCodebaseSearchTool(t, tool, CodebaseSearchParams{Query: "query", MinScore: &invalidMinScore})
	require.True(t, response.IsError)
	require.Equal(t, "min_score must be between -1 and 1", response.Content)

	response = runCodebaseSearchTool(t, tool, CodebaseSearchParams{Query: "query"})
	require.True(t, response.IsError)
	require.Equal(t, "Semantic code indexing and search are disabled for this project.", response.Content)
}

func TestCodebaseSearchCandidateLimitOversamplesSupportingEvidence(t *testing.T) {
	require.Equal(t, 200, codebaseSearchCandidateLimit(1))
	require.Equal(t, 200, codebaseSearchCandidateLimit(10))
	require.Equal(t, 400, codebaseSearchCandidateLimit(20))
}

func TestCodebaseSearchToolReportsActiveIndexingStates(t *testing.T) {
	require.Equal(t, "Semantic code index is currently being built in the background. Try again shortly.", formatCodebaseIndexUnavailable(&codebaseindex.StoreUnavailableError{State: codebaseindex.StoreStateIndexing}))
	require.Equal(t, "Semantic code index is currently being refreshed in the background. Try again shortly.", formatCodebaseIndexUnavailable(&codebaseindex.StoreUnavailableError{State: codebaseindex.StoreStateStale}))
}

func TestCodebaseSearchToolRequestsBackgroundReconciliation(t *testing.T) {
	workingDirectory := t.TempDir()
	enabled := true
	requested := make(chan struct{}, 1)
	tool := NewCodebaseSearchTool(workingDirectory, config.ToolCodebaseSearch{StoreDirectory: t.TempDir(), Enabled: &enabled}, nil, func() {
		requested <- struct{}{}
	})

	response := runCodebaseSearchTool(t, tool, CodebaseSearchParams{Query: "query"})
	require.True(t, response.IsError)
	select {
	case <-requested:
	default:
		t.Fatal("semantic search did not request background reconciliation")
	}
}

func TestCodebaseSearchToolReportsUnavailableIndex(t *testing.T) {
	workingDirectory := t.TempDir()
	enabled := true
	tool := NewCodebaseSearchTool(workingDirectory, config.ToolCodebaseSearch{StoreDirectory: t.TempDir(), Enabled: &enabled}, nil, nil)

	response := runCodebaseSearchTool(t, tool, CodebaseSearchParams{Query: "query"})
	require.True(t, response.IsError)
	require.Equal(t, "Semantic code index is not available yet. Background indexing may still be starting.", response.Content)
}

func TestFormatCodebaseSearchResults(t *testing.T) {
	content := strings.Repeat("界", 5000)
	output := formatCodebaseSearchResults([]codebaseindex.SearchResult{{
		Chunk: codebaseindex.Chunk{
			Path:      "internal/session/service.go",
			StartLine: 12,
			EndLine:   34,
			Content:   content,
		},
		Score:       0.875,
		Symbol:      "Service.Load",
		Role:        codebaseindex.SearchRoleDirect,
		Explanation: "Matches loading + session.",
	}})

	require.Contains(t, output, "Found 1 semantic matches:")
	require.Contains(t, output, "Direct implementation")
	require.Contains(t, output, "internal/session/service.go:12-34 (score 0.8750, symbol Service.Load)")
	require.Contains(t, output, "Matches loading + session.")
	require.True(t, utf8.ValidString(output))
	require.True(t, strings.HasSuffix(output, "..."))
	require.Less(t, len(output), len(content)+100)
	require.Equal(t, "No semantic matches found.", formatCodebaseSearchResults(nil))
}

func TestFormatCodebaseSearchResultsGroupsEvidenceRoles(t *testing.T) {
	output := formatCodebaseSearchResults([]codebaseindex.SearchResult{
		{Chunk: codebaseindex.Chunk{Path: "agent.go", StartLine: 1, EndLine: 5, Content: "parallel"}, Score: 0.8, Role: codebaseindex.SearchRoleParallel},
		{Chunk: codebaseindex.Chunk{Path: "shutdown.go", StartLine: 1, EndLine: 5, Content: "comparison"}, Score: 0.8, Role: codebaseindex.SearchRoleComparison},
		{Chunk: codebaseindex.Chunk{Path: "docs/contract.md", StartLine: 1, EndLine: 5, Content: "contract"}, Score: 0.8, Role: codebaseindex.SearchRoleContract},
	})

	contract := strings.Index(output, "Contract")
	comparison := strings.Index(output, "Comparison")
	parallel := strings.Index(output, "Parallel implementation")
	require.NotEqual(t, -1, contract)
	require.Greater(t, comparison, contract)
	require.Greater(t, parallel, comparison)
}

func runCodebaseSearchTool(t *testing.T, tool fantasy.AgentTool, params CodebaseSearchParams) fantasy.ToolResponse {
	t.Helper()
	input, err := json.Marshal(params)
	require.NoError(t, err)
	response, err := tool.Run(context.Background(), fantasy.ToolCall{
		ID:    "test-call",
		Name:  CodebaseSearchToolName,
		Input: string(input),
	})
	require.NoError(t, err)
	return response
}
