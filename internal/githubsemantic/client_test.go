package githubsemantic

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGitHubClientEmbedQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		require.Equal(t, http.MethodPost, request.Method)
		require.Equal(t, "/embeddings", request.URL.Path)
		require.Equal(t, "Bearer secret-token", request.Header.Get("Authorization"))
		require.Equal(t, "application/vnd.github+json", request.Header.Get("Accept"))
		require.Equal(t, "application/json", request.Header.Get("Content-Type"))
		require.Equal(t, "2022-11-28", request.Header.Get("X-GitHub-Api-Version"))
		require.Equal(t, "crux-test", request.Header.Get("User-Agent"))

		var body struct {
			Inputs         []string `json:"inputs"`
			InputType      string   `json:"input_type"`
			EmbeddingModel string   `json:"embedding_model"`
		}
		require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
		require.Equal(t, []string{"find session loading"}, body.Inputs)
		require.Equal(t, "query", body.InputType)
		require.Equal(t, "model-a", body.EmbeddingModel)

		response.Header().Set("Content-Type", "application/json")
		_, err := response.Write([]byte(`{"embedding_model":"model-a","embeddings":[{"embedding":[1,0.5]}]}`))
		require.NoError(t, err)
	}))
	t.Cleanup(server.Close)

	client := NewGitHubClient(server.Client(), func(context.Context) (string, error) { return "secret-token", nil }, "crux-test")
	client.baseURL = server.URL
	embedding, err := client.EmbedQuery(context.Background(), "find session loading", "model-a")
	require.NoError(t, err)
	require.Equal(t, []float32{1, 0.5}, embedding)
}

func TestGitHubClientSerializesSharedCredentialRequests(t *testing.T) {
	var inFlight atomic.Int32
	var maximum atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		current := inFlight.Add(1)
		for observed := maximum.Load(); current > observed && !maximum.CompareAndSwap(observed, current); observed = maximum.Load() {
		}
		time.Sleep(25 * time.Millisecond)
		inFlight.Add(-1)
		_, _ = response.Write([]byte(`{"embedding_model":"model-a","embeddings":[{"embedding":[1,0]}]}`))
	}))
	t.Cleanup(server.Close)

	clients := []*GitHubClient{
		NewGitHubClient(server.Client(), func(context.Context) (string, error) { return "shared-token", nil }, ""),
		NewGitHubClient(server.Client(), func(context.Context) (string, error) { return "shared-token", nil }, ""),
	}
	for _, client := range clients {
		client.baseURL = server.URL
	}
	start := make(chan struct{})
	results := make(chan error, len(clients))
	var workers sync.WaitGroup
	for _, client := range clients {
		workers.Add(1)
		go func(client *GitHubClient) {
			defer workers.Done()
			<-start
			_, err := client.EmbedQuery(t.Context(), "query", "model-a")
			results <- err
		}(client)
	}
	close(start)
	workers.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	require.Equal(t, int32(1), maximum.Load())
}

func TestGitHubClientRequestGateCancellation(t *testing.T) {
	entered := make(chan struct{})
	releaseRequest := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		select {
		case <-entered:
		default:
			close(entered)
			<-releaseRequest
		}
		_, _ = response.Write([]byte(`{"embedding_model":"model-a","embeddings":[{"embedding":[1,0]}]}`))
	}))
	t.Cleanup(server.Close)

	first := NewGitHubClient(server.Client(), func(context.Context) (string, error) { return "shared-token", nil }, "")
	first.baseURL = server.URL
	firstResult := make(chan error, 1)
	go func() {
		_, err := first.EmbedQuery(t.Context(), "first", "model-a")
		firstResult <- err
	}()
	<-entered

	second := NewGitHubClient(server.Client(), func(context.Context) (string, error) { return "shared-token", nil }, "")
	second.baseURL = server.URL
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := second.EmbedQuery(ctx, "second", "model-a")
	require.ErrorIs(t, err, context.Canceled)

	close(releaseRequest)
	require.NoError(t, <-firstResult)
}

func TestGitHubClientSharesRateLimitCooldown(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if requests.Add(1) == 1 {
			response.WriteHeader(http.StatusTooManyRequests)
			_, _ = response.Write([]byte(`{"metadata":{"retry_after":0.05}}`))
			return
		}
		_, _ = response.Write([]byte(`{"embedding_model":"model-a","embeddings":[{"embedding":[1,0]}]}`))
	}))
	t.Cleanup(server.Close)

	first := NewGitHubClient(server.Client(), func(context.Context) (string, error) { return "shared-token", nil }, "")
	first.baseURL = server.URL
	first.maxRetries = 0
	_, err := first.EmbedQuery(t.Context(), "first", "model-a")
	require.ErrorContains(t, err, "status 429")

	second := NewGitHubClient(server.Client(), func(context.Context) (string, error) { return "shared-token", nil }, "")
	second.baseURL = server.URL
	started := time.Now()
	embedding, err := second.EmbedQuery(t.Context(), "second", "model-a")
	require.NoError(t, err)
	require.Equal(t, []float32{1, 0}, embedding)
	require.GreaterOrEqual(t, time.Since(started), 40*time.Millisecond)
	require.Equal(t, int32(2), requests.Load())
}

func TestGitHubClientEmbedQueryRetries(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if requests.Add(1) == 1 {
			response.WriteHeader(http.StatusTooManyRequests)
			_, _ = response.Write([]byte("rate limited"))
			return
		}
		_, _ = response.Write([]byte(`{"embedding_model":"model-a","embeddings":[{"embedding":[1,0]}]}`))
	}))
	t.Cleanup(server.Close)

	client := NewGitHubClient(server.Client(), func(context.Context) (string, error) { return "secret-token", nil }, "")
	client.baseURL = server.URL
	client.wait = func(context.Context, time.Duration) error { return nil }
	embedding, err := client.EmbedQuery(context.Background(), "query", "model-a")
	require.NoError(t, err)
	require.Equal(t, []float32{1, 0}, embedding)
	require.Equal(t, int32(2), requests.Load())
}

func TestGitHubClientEmbedQueryRetriesRateLimit403(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if requests.Add(1) == 1 {
			response.WriteHeader(http.StatusForbidden)
			_, _ = response.Write([]byte(`{"message":"API rate limit exceeded for user ID 123"}`))
			return
		}
		_, _ = response.Write([]byte(`{"embedding_model":"model-a","embeddings":[{"embedding":[1,0]}]}`))
	}))
	t.Cleanup(server.Close)

	client := NewGitHubClient(server.Client(), func(context.Context) (string, error) { return "secret-token", nil }, "")
	client.baseURL = server.URL
	client.wait = func(context.Context, time.Duration) error { return nil }
	embedding, err := client.EmbedQuery(context.Background(), "query", "model-a")
	require.NoError(t, err)
	require.Equal(t, []float32{1, 0}, embedding)
	require.Equal(t, int32(2), requests.Load())
}

func TestGitHubClientEmbedQueryDoesNotRetryForbidden(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		response.WriteHeader(http.StatusForbidden)
		_, _ = response.Write([]byte(`{"message":"Resource not accessible by integration"}`))
	}))
	t.Cleanup(server.Close)

	client := NewGitHubClient(server.Client(), func(context.Context) (string, error) { return "secret-token", nil }, "")
	client.baseURL = server.URL
	client.wait = func(context.Context, time.Duration) error {
		t.Fatal("unexpected retry")
		return nil
	}
	_, err := client.EmbedQuery(context.Background(), "query", "model-a")
	require.ErrorContains(t, err, "status 403")
	require.Equal(t, int32(1), requests.Load())
}

func TestGitHubRateLimited(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		headers http.Header
		body    string
		want    bool
	}{
		{name: "rate limit body", status: http.StatusForbidden, body: "API rate limit exceeded", want: true},
		{name: "secondary rate limit body", status: http.StatusForbidden, body: "secondary rate limit", want: true},
		{name: "retry after", status: http.StatusForbidden, headers: http.Header{"Retry-After": []string{"5"}}, want: true},
		{name: "remaining exhausted", status: http.StatusForbidden, headers: http.Header{"X-Ratelimit-Remaining": []string{"0"}}, want: true},
		{name: "ordinary forbidden", status: http.StatusForbidden, body: "forbidden", want: false},
		{name: "non forbidden", status: http.StatusTooManyRequests, body: "rate limit exceeded", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := &http.Response{StatusCode: test.status, Header: test.headers}
			require.Equal(t, test.want, githubRateLimited(response, []byte(test.body)))
		})
	}
}

func TestGitHubClientEmbedQueryErrors(t *testing.T) {
	t.Run("invalid inputs and credential", func(t *testing.T) {
		client := NewGitHubClient(nil, nil, "")
		_, err := client.EmbedQuery(context.Background(), "", "model-a")
		require.ErrorContains(t, err, "query is empty")
		_, err = client.EmbedQuery(context.Background(), "query", "")
		require.ErrorContains(t, err, "model is empty")
		_, err = client.EmbedQuery(context.Background(), "query", "model-a")
		require.ErrorContains(t, err, "token source is nil")

		client = NewGitHubClient(nil, func(context.Context) (string, error) { return "", nil }, "")
		_, err = client.EmbedQuery(context.Background(), "query", "model-a")
		require.ErrorContains(t, err, "credential is unavailable")
		client = NewGitHubClient(nil, func(context.Context) (string, error) { return "", errors.New("locked") }, "")
		_, err = client.EmbedQuery(context.Background(), "query", "model-a")
		require.ErrorContains(t, err, "load GitHub credential: locked")
	})

	t.Run("bounded HTTP error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			response.WriteHeader(http.StatusUnauthorized)
			_, _ = response.Write([]byte(strings.Repeat("failure ", 200)))
		}))
		t.Cleanup(server.Close)
		client := NewGitHubClient(server.Client(), func(context.Context) (string, error) { return "secret-token", nil }, "")
		client.baseURL = server.URL

		_, err := client.EmbedQuery(context.Background(), "query", "model-a")
		require.ErrorContains(t, err, "status 401")
		require.Less(t, len(err.Error()), 600)
		require.NotContains(t, err.Error(), "secret-token")
	})

	t.Run("model mismatch", func(t *testing.T) {
		server := embeddingTestServer(t, `{"embedding_model":"model-b","embeddings":[{"embedding":[1]}]}`)
		client := NewGitHubClient(server.Client(), func(context.Context) (string, error) { return "token", nil }, "")
		client.baseURL = server.URL
		_, err := client.EmbedQuery(context.Background(), "query", "model-a")
		require.ErrorContains(t, err, `model "model-b", expected "model-a"`)
	})

	t.Run("invalid embedding", func(t *testing.T) {
		server := embeddingTestServer(t, `{"embedding_model":"model-a","embeddings":[]}`)
		client := NewGitHubClient(server.Client(), func(context.Context) (string, error) { return "token", nil }, "")
		client.baseURL = server.URL
		_, err := client.EmbedQuery(context.Background(), "query", "model-a")
		require.ErrorContains(t, err, "invalid query embedding")
	})
}

func TestGitHubClientLongQueryRetryReturnsImmediately(t *testing.T) {
	for _, header := range []bool{false, true} {
		t.Run(strconv.FormatBool(header), func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				requests.Add(1)
				if header {
					response.Header().Set("Retry-After", "202")
				}
				response.WriteHeader(http.StatusTooManyRequests)
				_, _ = response.Write([]byte(`{"message":"try again in 201.434100025s","metadata":{"retry_after":"202"}}`))
			}))
			t.Cleanup(server.Close)
			client := NewGitHubClient(server.Client(), func(context.Context) (string, error) { return "token", nil }, "")
			client.baseURL = server.URL
			client.wait = func(context.Context, time.Duration) error {
				t.Fatal("query must not wait for a long rate limit")
				return nil
			}
			_, err := client.EmbedQuery(t.Context(), "query", "model-a")
			var semanticErr *GitHubSemanticError
			require.ErrorAs(t, err, &semanticErr)
			require.Equal(t, 202*time.Second, semanticErr.RetryAfter)
			require.ErrorContains(t, err, "retry after 3m22s")
			require.Equal(t, int32(1), requests.Load())
		})
	}
}

func TestGitHubClientChunkingHonorsBodyRetryDelay(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if requests.Add(1) == 1 {
			response.WriteHeader(http.StatusTooManyRequests)
			_, _ = response.Write([]byte(`{"metadata":{"retry_after":"202"}}`))
			return
		}
		_, _ = response.Write([]byte(`{"embedding_model":"model-a","chunks":[]}`))
	}))
	t.Cleanup(server.Close)
	client := NewGitHubClient(server.Client(), func(context.Context) (string, error) { return "token", nil }, "")
	client.baseURL = server.URL
	var delays []time.Duration
	client.wait = func(_ context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		return nil
	}
	_, err := client.ChunkAndEmbedFile(t.Context(), "main.go", "package main", "model-a")
	require.NoError(t, err)
	require.Equal(t, []time.Duration{202 * time.Second}, delays)
	require.Equal(t, int32(2), requests.Load())
}

func TestGitHubClientQueryRetryBudget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Retry-After", "6")
		response.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(server.Close)
	client := NewGitHubClient(server.Client(), func(context.Context) (string, error) { return "token", nil }, "")
	client.baseURL = server.URL
	var waits int
	client.wait = func(_ context.Context, delay time.Duration) error {
		waits++
		require.Equal(t, 6*time.Second, delay)
		return nil
	}
	_, err := client.EmbedQuery(t.Context(), "query", "model-a")
	require.ErrorContains(t, err, "status 429")
	require.Equal(t, 1, waits)
}

func TestGitHubRetryDelay(t *testing.T) {
	for _, test := range []struct {
		name   string
		header string
		body   string
		want   time.Duration
	}{
		{name: "header", header: "202", want: 202 * time.Second},
		{name: "string metadata", body: `{"metadata":{"retry_after":"202"}}`, want: 202 * time.Second},
		{name: "numeric metadata", body: `{"metadata":{"retry_after":1.5}}`, want: 1500 * time.Millisecond},
		{name: "header precedence", header: "3", body: `{"metadata":{"retry_after":"202"}}`, want: 3 * time.Second},
		{name: "invalid metadata", body: `{"metadata":{"retry_after":"NaN"}}`, want: 500 * time.Millisecond},
		{name: "negative metadata", body: `{"metadata":{"retry_after":-2}}`, want: 500 * time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := &http.Response{Header: make(http.Header)}
			response.Header.Set("Retry-After", test.header)
			require.Equal(t, test.want, githubRetryDelay(response, []byte(test.body), 1))
		})
	}
}

func TestGitHubClientQueryCancellationDuringRetry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(server.Close)
	client := NewGitHubClient(server.Client(), func(context.Context) (string, error) { return "token", nil }, "")
	client.baseURL = server.URL
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client.wait = func(ctx context.Context, delay time.Duration) error {
		cancel()
		return waitForRetry(ctx, delay)
	}
	_, err := client.EmbedQuery(ctx, "query", "model-a")
	require.ErrorIs(t, err, context.Canceled)
}

func embeddingTestServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_, err := response.Write([]byte(body))
		require.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	return server
}
