package codebaseindex

import (
	"net/http"

	"github.com/example-git/crux/internal/githubsemantic"
	"github.com/example-git/crux/internal/semanticembedding"
)

// GitHubSemanticUserAgent identifies the bundled indexing client independently
// from Copilot inference identity.
const GitHubSemanticUserAgent = "crux-codebase-index"

// Compatibility aliases keep codebase-index callers source-compatible while
// the concrete GitHub transport lives independently from index storage.
type (
	TokenSource           = githubsemantic.TokenSource
	EmbeddedDocumentChunk = semanticembedding.EmbeddedDocumentChunk
	GitHubSemanticError   = githubsemantic.GitHubSemanticError
	GitHubClient          = githubsemantic.GitHubClient
)

func NewGitHubClient(httpClient *http.Client, tokenSource TokenSource, userAgent string) *GitHubClient {
	return githubsemantic.NewGitHubClient(httpClient, tokenSource, userAgent)
}

func finiteEmbedding(vector []float32) bool {
	return semanticembedding.Finite(vector)
}
