package tools

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	fantasy "github.com/example-git/crux/foundation"

	"github.com/example-git/crux/internal/codebaseindex"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/permission"
)

const (
	CodebaseSearchToolName    = "codebase_search"
	codebaseSearchDescription = "Search the current project's standalone partitioned semantic code index using GitHub-generated embeddings. Prefer this over search as the first repository-discovery tool for conceptual, behavioral, or implementation-path searches when a completed index is available and the relevant files are indexed. Background refreshes keep serving the last completed index until the replacement is ready. Use LSP or search in content mode for known exact symbols and literals. A codebase-index GitHub login is required."
)

type CodebaseSearchParams struct {
	Query      string   `json:"query" description:"Natural-language query describing the code or behavior to find"`
	Count      int      `json:"count,omitempty" description:"Maximum results to return (default 10, max 20)"`
	MinScore   *float64 `json:"min_score,omitempty" description:"Minimum cosine similarity score (default 0.2)"`
	PathPrefix string   `json:"path_prefix,omitempty" description:"Optional project-relative path prefix"`
}

func NewCodebaseSearchTool(workingDir string, toolConfig config.ToolCodebaseSearch, httpClient *http.Client, requestReconcile func()) fantasy.AgentTool {
	return fantasy.NewParallelAgentTool(
		CodebaseSearchToolName,
		codebaseSearchDescription,
		func(ctx context.Context, params CodebaseSearchParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if permission.IsSubagent(ctx) {
				return fantasy.NewTextErrorResponse(permission.ErrSubagentCodebaseSearch.Error()), nil
			}
			if strings.TrimSpace(params.Query) == "" {
				return fantasy.NewTextErrorResponse("query is required"), nil
			}
			if params.Count == 0 {
				params.Count = 10
			}
			if params.Count < 1 || params.Count > 20 {
				return fantasy.NewTextErrorResponse("count must be between 1 and 20"), nil
			}
			minScore := 0.2
			if params.MinScore != nil {
				minScore = *params.MinScore
			}
			if minScore < -1 || minScore > 1 {
				return fantasy.NewTextErrorResponse("min_score must be between -1 and 1"), nil
			}
			if !toolConfig.IsEnabled() {
				return fantasy.NewTextErrorResponse("Semantic code indexing and search are disabled for this project."), nil
			}

			projectRoot, err := codebaseindex.CanonicalProjectRoot(ctx, workingDir)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			if requestReconcile != nil {
				requestReconcile()
			}
			reader, err := codebaseindex.OpenReadyProjectWithFilters(projectRoot, toolConfig.GetStoreDirectory(), codebaseindex.ProjectFilters{
				IncludePaths: toolConfig.IncludePaths,
				ExcludePaths: toolConfig.ExcludePaths,
			})
			if err != nil {
				var unavailable *codebaseindex.StoreUnavailableError
				if errors.As(err, &unavailable) {
					return fantasy.NewTextErrorResponse(formatCodebaseIndexUnavailable(unavailable)), nil
				}
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			defer reader.Close()

			embedder := codebaseindex.NewGitHubClient(httpClient, codebaseindex.CodebaseIndexToken, codebaseindex.GitHubSemanticUserAgent)
			results, err := reader.Search(ctx, embedder, projectRoot, params.Query, codebaseindex.SearchOptions{
				Limit:      codebaseSearchCandidateLimit(params.Count),
				MinScore:   minScore,
				PathPrefix: params.PathPrefix,
			})
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			if params.MinScore == nil {
				results, err = reader.SupplementRelatedDefinitions(ctx, projectRoot, params.Query, params.PathPrefix, results, params.Count)
				if err != nil {
					return fantasy.NewTextErrorResponse(err.Error()), nil
				}
				results, err = reader.SupplementDocumentation(ctx, projectRoot, params.Query, params.PathPrefix, results, params.Count)
				if err != nil {
					return fantasy.NewTextErrorResponse(err.Error()), nil
				}
			}
			results = codebaseindex.RerankSearchResults(projectRoot, params.Query, results, params.Count)
			return fantasy.NewTextResponse(formatCodebaseSearchResults(results)), nil
		},
	)
}

func codebaseSearchCandidateLimit(resultLimit int) int {
	return max(resultLimit*20, 200)
}

func formatCodebaseIndexUnavailable(err *codebaseindex.StoreUnavailableError) string {
	switch err.State {
	case codebaseindex.StoreStateDisabled:
		return "Semantic code indexing and search are disabled for this project."
	case codebaseindex.StoreStateIndexing:
		return "Semantic code index is currently being built in the background. Try again shortly."
	case codebaseindex.StoreStateStale:
		return "Semantic code index is currently being refreshed in the background. Try again shortly."
	case codebaseindex.StoreStateFailed:
		return "Semantic code index is unavailable because background indexing failed. Check the Crux logs for details."
	default:
		return "Semantic code index is not available yet. Background indexing may still be starting."
	}
}

func formatCodebaseSearchResults(results []codebaseindex.SearchResult) string {
	if len(results) == 0 {
		return "No semantic matches found."
	}

	var output strings.Builder
	fmt.Fprintf(&output, "Found %d semantic matches:\n", len(results))
	roles := []codebaseindex.SearchRole{
		codebaseindex.SearchRoleDirect,
		codebaseindex.SearchRoleDelivery,
		codebaseindex.SearchRolePersistence,
		codebaseindex.SearchRoleRecovery,
		codebaseindex.SearchRoleConstruction,
		codebaseindex.SearchRoleStartup,
		codebaseindex.SearchRoleValidation,
		codebaseindex.SearchRoleContract,
		codebaseindex.SearchRoleComparison,
		codebaseindex.SearchRoleParallel,
		codebaseindex.SearchRoleRelated,
	}
	resultIndex := 0
	for _, role := range roles {
		wroteRole := false
		for _, result := range results {
			resultRole := result.Role
			if resultRole == "" {
				resultRole = codebaseindex.SearchRoleRelated
			}
			if resultRole != role {
				continue
			}
			if !wroteRole {
				fmt.Fprintf(&output, "\n%s\n", role)
				wroteRole = true
			}
			resultIndex++
			fmt.Fprintf(&output, "%d. %s:%d-%d (score %.4f", resultIndex, result.Chunk.Path, result.Chunk.StartLine, result.Chunk.EndLine, result.Score)
			if result.Symbol != "" {
				fmt.Fprintf(&output, ", symbol %s", result.Symbol)
			}
			output.WriteString(")\n")
			if result.Explanation != "" {
				output.WriteString(result.Explanation)
				output.WriteByte('\n')
			}
			content := result.Chunk.Content
			if len(content) > 12000 {
				content = content[:12000]
				for !utf8.ValidString(content) {
					content = content[:len(content)-1]
				}
				content += "..."
			}
			output.WriteString(content)
			output.WriteString("\n")
		}
	}
	return strings.TrimSpace(output.String())
}
