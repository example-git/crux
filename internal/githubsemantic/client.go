// Package githubsemantic implements the bundled GitHub semantic embedding
// transport independently from inference providers and plugin discovery.
package githubsemantic

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/example-git/crux/internal/semanticembedding"
)

const (
	githubAPIURL          = "https://api.github.com"
	defaultEmbeddingModel = "metis-1024-I16-Binary"
)

var (
	rejectedCredentials sync.Map
	requestGates        sync.Map
)

type githubRequestGate struct {
	permit  chan struct{}
	mu      sync.Mutex
	retryAt time.Time
}

func githubRequestGateFor(baseURL, token string) *githubRequestGate {
	key := sha256.Sum256([]byte(baseURL + "\x00" + token))
	created := &githubRequestGate{permit: make(chan struct{}, 1)}
	created.permit <- struct{}{}
	actual, _ := requestGates.LoadOrStore(key, created)
	return actual.(*githubRequestGate)
}

func (g *githubRequestGate) acquire(ctx context.Context) (func(), error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-g.permit:
	}
	g.mu.Lock()
	retryAt := g.retryAt
	g.mu.Unlock()
	if delay := time.Until(retryAt); delay > 0 {
		if err := waitForRetry(ctx, delay); err != nil {
			g.release()
			return nil, err
		}
	}
	return g.release, nil
}

func (g *githubRequestGate) deferRetry(delay time.Duration) {
	if delay <= 0 {
		return
	}
	retryAt := time.Now().Add(delay)
	g.mu.Lock()
	if retryAt.After(g.retryAt) {
		g.retryAt = retryAt
	}
	g.mu.Unlock()
}

func (g *githubRequestGate) release() {
	g.permit <- struct{}{}
}

func AuthenticationRequired(token string) bool {
	_, rejected := rejectedCredentials.Load(sha256.Sum256([]byte(token)))
	return rejected
}

type TokenSource func(context.Context) (string, error)

type GitHubSemanticError struct {
	Operation  string
	Status     int
	Body       string
	RetryAfter time.Duration
}

func (e *GitHubSemanticError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("GitHub %s request failed with status %d; retry after %s: %s", e.Operation, e.Status, e.RetryAfter.Round(time.Second), e.Body)
	}
	if e.Body == "" {
		return fmt.Sprintf("GitHub %s request failed with status %d", e.Operation, e.Status)
	}
	return fmt.Sprintf("GitHub %s request failed with status %d: %s", e.Operation, e.Status, e.Body)
}

type GitHubClient struct {
	baseURL     string
	httpClient  *http.Client
	tokenSource TokenSource
	userAgent   string
	maxRetries  int
	wait        func(context.Context, time.Duration) error
}

func NewGitHubClient(httpClient *http.Client, tokenSource TokenSource, userAgent string) *GitHubClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &GitHubClient{
		baseURL:     githubAPIURL,
		httpClient:  httpClient,
		tokenSource: tokenSource,
		userAgent:   userAgent,
		maxRetries:  6,
		wait:        waitForRetry,
	}
}

func (c *GitHubClient) PreferredEmbeddingModel(ctx context.Context) string {
	var response struct {
		Models []struct {
			ID     string `json:"id"`
			Active *bool  `json:"active"`
		} `json:"models"`
	}
	if err := c.requestJSON(ctx, http.MethodGet, "/embeddings/models", nil, &response, "embedding model", 0, 0); err != nil {
		return defaultEmbeddingModel
	}
	for _, model := range response.Models {
		if model.ID != "" && (model.Active == nil || *model.Active) {
			return model.ID
		}
	}
	for _, model := range response.Models {
		if model.ID != "" {
			return model.ID
		}
	}
	return defaultEmbeddingModel
}

func (c *GitHubClient) ChunkAndEmbedFile(ctx context.Context, path, content, model string) ([]semanticembedding.EmbeddedDocumentChunk, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("document path is empty")
	}
	if strings.TrimSpace(content) == "" {
		return nil, nil
	}
	if strings.TrimSpace(model) == "" {
		return nil, fmt.Errorf("embedding model is empty")
	}
	request := struct {
		Embed          bool     `json:"embed"`
		QOS            string   `json:"qos"`
		Content        string   `json:"content"`
		Path           string   `json:"path"`
		LocalHashes    []string `json:"local_hashes"`
		EmbeddingModel string   `json:"embedding_model"`
	}{
		Embed:          true,
		QOS:            "batch",
		Content:        content,
		Path:           path,
		LocalHashes:    []string{},
		EmbeddingModel: model,
	}
	var response struct {
		EmbeddingModel string `json:"embedding_model"`
		Chunks         []struct {
			Hash      string `json:"hash"`
			Text      string `json:"text"`
			LineRange struct {
				Start int `json:"start"`
				End   int `json:"end"`
			} `json:"line_range"`
			Embedding struct {
				Model     string    `json:"model"`
				Embedding []float32 `json:"embedding"`
			} `json:"embedding"`
		} `json:"chunks"`
	}
	if err := c.requestJSON(ctx, http.MethodPost, "/chunks", request, &response, "chunking", c.maxRetries, 0); err != nil {
		return nil, err
	}
	returnedModel := response.EmbeddingModel
	if returnedModel == "" {
		returnedModel = model
	}
	if returnedModel != model {
		return nil, fmt.Errorf("GitHub returned embedding model %q, expected %q", returnedModel, model)
	}
	chunks := make([]semanticembedding.EmbeddedDocumentChunk, 0, len(response.Chunks))
	for _, raw := range response.Chunks {
		chunkModel := raw.Embedding.Model
		if chunkModel == "" {
			chunkModel = returnedModel
		}
		if raw.Hash == "" || raw.Text == "" || len(raw.Embedding.Embedding) == 0 || !semanticembedding.Finite(raw.Embedding.Embedding) {
			continue
		}
		if chunkModel != model {
			return nil, fmt.Errorf("GitHub returned chunk embedding model %q, expected %q", chunkModel, model)
		}
		endLine := raw.LineRange.End
		if endLine == 0 {
			endLine = raw.LineRange.Start
		}
		chunks = append(chunks, semanticembedding.EmbeddedDocumentChunk{
			Hash:      raw.Hash,
			Text:      stripChunkTextMetadata(raw.Text),
			StartLine: raw.LineRange.Start,
			EndLine:   endLine,
			Model:     chunkModel,
			Embedding: raw.Embedding.Embedding,
		})
	}
	return chunks, nil
}

func (c *GitHubClient) EmbedQuery(ctx context.Context, query, model string) ([]float32, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("embedding query is empty")
	}
	if strings.TrimSpace(model) == "" {
		return nil, fmt.Errorf("embedding model is empty")
	}

	request := struct {
		Inputs         []string `json:"inputs"`
		InputType      string   `json:"input_type"`
		EmbeddingModel string   `json:"embedding_model"`
	}{
		Inputs:         []string{query},
		InputType:      "query",
		EmbeddingModel: model,
	}
	var response struct {
		EmbeddingModel string `json:"embedding_model"`
		Embeddings     []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"embeddings"`
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := c.requestJSON(ctx, http.MethodPost, "/embeddings", request, &response, "embedding", c.maxRetries, 10*time.Second); err != nil {
		return nil, err
	}
	if response.EmbeddingModel != "" && response.EmbeddingModel != model {
		return nil, fmt.Errorf("GitHub returned embedding model %q, expected %q", response.EmbeddingModel, model)
	}
	if len(response.Embeddings) != 1 || len(response.Embeddings[0].Embedding) == 0 || !semanticembedding.Finite(response.Embeddings[0].Embedding) {
		return nil, fmt.Errorf("GitHub returned an invalid query embedding")
	}
	return response.Embeddings[0].Embedding, nil
}

func (c *GitHubClient) requestJSON(ctx context.Context, method, path string, requestBody, responseBody any, operation string, retries int, maxRetryWait time.Duration) error {
	if c.tokenSource == nil {
		return fmt.Errorf("GitHub token source is nil")
	}
	var body []byte
	var err error
	if requestBody != nil {
		body, err = json.Marshal(requestBody)
		if err != nil {
			return fmt.Errorf("encode GitHub %s request: %w", operation, err)
		}
	}
	token, err := c.tokenSource(ctx)
	if err != nil {
		return fmt.Errorf("load GitHub credential: %w", err)
	}
	if token == "" {
		return fmt.Errorf("GitHub codebase-index credential is unavailable")
	}
	gate := githubRequestGateFor(c.baseURL, token)
	release, err := gate.acquire(ctx)
	if err != nil {
		return fmt.Errorf("wait for GitHub %s request admission: %w", operation, err)
	}
	defer release()

	var retryWait time.Duration
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("create GitHub %s request: %w", operation, err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		if requestBody != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if c.userAgent != "" {
			req.Header.Set("User-Agent", c.userAgent)
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("execute GitHub %s request: %w", operation, err)
		}
		if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
			rejectedCredentials.Delete(sha256.Sum256([]byte(token)))
			if responseBody == nil || resp.StatusCode == http.StatusNoContent {
				resp.Body.Close()
				return nil
			}
			decodeErr := json.NewDecoder(resp.Body).Decode(responseBody)
			resp.Body.Close()
			if decodeErr != nil {
				return fmt.Errorf("decode GitHub %s response: %w", operation, decodeErr)
			}
			return nil
		}

		responseBytes, readErr := io.ReadAll(io.LimitReader(resp.Body, 500))
		resp.Body.Close()
		rateLimited := resp.StatusCode == http.StatusTooManyRequests || githubRateLimited(resp, responseBytes)
		retryable := resp.StatusCode == http.StatusRequestTimeout ||
			rateLimited ||
			resp.StatusCode >= 500
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden && !retryable {
			rejectedCredentials.Store(sha256.Sum256([]byte(token)), struct{}{})
		}
		delay := githubRetryDelay(resp, responseBytes, attempt+1)
		if rateLimited {
			gate.deferRetry(delay)
		}
		if retryable && attempt < retries && (maxRetryWait == 0 || delay <= maxRetryWait-retryWait) {
			if err := c.wait(ctx, delay); err != nil {
				return fmt.Errorf("wait to retry GitHub %s request: %w", operation, err)
			}
			retryWait += delay
			continue
		}
		if readErr != nil {
			return &GitHubSemanticError{Operation: operation, Status: resp.StatusCode}
		}
		if !retryable {
			delay = 0
		}
		return &GitHubSemanticError{
			RetryAfter: delay,
			Operation:  operation,
			Status:     resp.StatusCode,
			Body:       strings.Join(strings.Fields(string(responseBytes)), " "),
		}
	}
}

func stripChunkTextMetadata(text string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if len(lines) >= 3 && strings.HasPrefix(lines[0], "File: ") && strings.HasPrefix(lines[1], "```") && strings.HasPrefix(lines[len(lines)-1], "```") {
		return strings.Join(lines[2:len(lines)-1], "\n")
	}
	return text
}

func githubRateLimited(response *http.Response, body []byte) bool {
	if response.StatusCode != http.StatusForbidden {
		return false
	}
	if response.Header.Get("Retry-After") != "" || response.Header.Get("X-RateLimit-Remaining") == "0" {
		return true
	}
	message := strings.ToLower(string(body))
	return strings.Contains(message, "rate limit exceeded") || strings.Contains(message, "secondary rate limit")
}

func githubRetryDelay(response *http.Response, body []byte, attempt int) time.Duration {
	if delay := githubRetrySeconds(response.Header.Get("Retry-After")); delay > 0 {
		return delay
	}
	if retryAt, err := http.ParseTime(response.Header.Get("Retry-After")); err == nil {
		if delay := time.Until(retryAt); delay > 0 {
			return delay
		}
	}
	var value struct {
		Metadata struct {
			RetryAfter json.RawMessage `json:"retry_after"`
		} `json:"metadata"`
	}
	if json.Unmarshal(body, &value) == nil {
		if delay := githubRetrySeconds(strings.Trim(string(value.Metadata.RetryAfter), "\"")); delay > 0 {
			return delay
		}
	}
	if reset, err := strconv.ParseInt(response.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		if delay := time.Until(time.Unix(reset, 0)); delay > 0 {
			return delay
		}
	}
	return min(250*time.Millisecond*time.Duration(1<<attempt), 2*time.Second)
}

func githubRetrySeconds(value string) time.Duration {
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(seconds) || seconds <= 0 {
		return 0
	}
	if seconds >= float64(math.MaxInt64)/float64(time.Second) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(seconds * float64(time.Second))
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
