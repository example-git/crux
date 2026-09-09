package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/pubsub"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func workspaceMCPServer(t *testing.T, label string) *sdk.Server {
	t.Helper()
	s := sdk.NewServer(&sdk.Implementation{Name: label}, nil)
	sdk.AddTool(s, &sdk.Tool{Name: "identify", Description: "Return this workspace's MCP identity"}, func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: label}}}, nil, nil
	})
	s.AddPrompt(&sdk.Prompt{Name: label + "-prompt"}, func(context.Context, *sdk.GetPromptRequest) (*sdk.GetPromptResult, error) {
		return &sdk.GetPromptResult{}, nil
	})
	s.AddResource(&sdk.Resource{Name: label, URI: "fixture://" + label}, func(context.Context, *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
		return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{URI: "fixture://" + label, Text: label}}}, nil
	})
	return s
}

func workspaceMCPStore(t *testing.T, endpoint string) *config.ConfigStore {
	t.Helper()
	store := config.NewTestStore(&config.Config{MCP: config.MCPs{"same-name": {Type: config.MCPHttp, URL: endpoint}}})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, For(store).Close(ctx))
	})
	return store
}

func TestWorkspaceMCPIndependentTransportsAndShutdown(t *testing.T) {
	for _, label := range []string{"separate-stores"} {
		t.Run(label, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			first := workspaceMCPServer(t, label+"-first")
			second := workspaceMCPServer(t, label+"-second")
			aHTTP := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return first }, nil))
			defer aHTTP.Close()
			bHTTP := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return second }, nil))
			defer bHTTP.Close()
			a, b := workspaceMCPStore(t, aHTTP.URL), workspaceMCPStore(t, bHTTP.URL)
			ra, rb := For(a), For(b)
			require.NotSame(t, ra, rb)
			var wg sync.WaitGroup
			wg.Go(func() { ra.Initialize(ctx, nil, a) })
			wg.Go(func() { rb.Initialize(ctx, nil, b) })
			wg.Wait()
			for store, want := range map[*config.ConfigStore]string{a: label + "-first", b: label + "-second"} {
				result, err := For(store).RunTool(ctx, store, "same-name", "identify", "{}")
				require.NoError(t, err)
				require.Equal(t, want, result.Content)
				For(store).RefreshResources(ctx, "same-name")
				resources, err := For(store).ReadResource(ctx, store, "same-name", "fixture://"+want)
				require.NoError(t, err)
				require.Equal(t, want, resources[0].Text)
			}
			require.NoError(t, ra.Close(ctx))
			require.NoError(t, ra.Close(ctx))
			require.Same(t, ra, For(a))
			require.Empty(t, ra.GetStates())
			require.Empty(t, ra.allTools.Copy())
			require.Empty(t, ra.allPrompts.Copy())
			require.Empty(t, ra.allResources.Copy())
			_, err := ra.RunTool(ctx, a, "same-name", "identify", "{}")
			require.ErrorIs(t, err, ErrClosed)
			require.ErrorIs(t, ra.InitializeSingle(ctx, "same-name", a), ErrClosed)
			ra.Reinitialize(ctx, a)
			result, err := rb.RunTool(ctx, b, "same-name", "identify", "{}")
			require.NoError(t, err)
			require.Equal(t, label+"-second", result.Content)
			rb.Reinitialize(ctx, b)
			result, err = rb.RunTool(ctx, b, "same-name", "identify", "{}")
			require.NoError(t, err)
			require.Equal(t, label+"-second", result.Content)
			require.Len(t, rb.GetStates(), 1)
			require.Len(t, rb.allPrompts.Copy(), 1)
			require.Len(t, rb.allResources.Copy(), 1)
		})
	}
}

func TestWorkspaceMCPShutdownDuringInitializeAndUpdate(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	started := make(chan struct{})
	var once sync.Once
	var requests atomic.Int32
	release := make(chan struct{})
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		once.Do(func() { close(started) })
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer blocked.Close()
	defer close(release)
	a := workspaceMCPStore(t, blocked.URL)
	ra := For(a)
	initDone := make(chan struct{})
	go func() { defer close(initDone); ra.Initialize(ctx, nil, a) }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var updates sync.WaitGroup
	for range 8 {
		updates.Go(func() { ra.Reinitialize(ctx, a) })
	}
	require.NoError(t, ra.Close(ctx))
	updates.Wait()
	<-initDone
	require.Empty(t, ra.GetStates())
	require.Empty(t, ra.sessions.Copy())
	before := requests.Load()
	ra.Initialize(ctx, nil, a)
	ra.Reinitialize(ctx, a)
	require.ErrorIs(t, ra.InitializeSingle(ctx, "same-name", a), ErrClosed)
	require.Equal(t, before, requests.Load())
}

func TestWorkspaceMCPSessionOutlivesInitializationRequest(t *testing.T) {
	server := workspaceMCPServer(t, "retained")
	httpServer := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil))
	defer httpServer.Close()
	store := workspaceMCPStore(t, httpServer.URL)
	initCtx, cancel := context.WithCancel(t.Context())
	require.NoError(t, For(store).InitializeSingle(initCtx, "same-name", store))
	cancel()
	ctx, done := context.WithTimeout(t.Context(), 5*time.Second)
	defer done()
	result, err := For(store).RunTool(ctx, store, "same-name", "identify", "{}")
	require.NoError(t, err)
	require.Equal(t, "retained", result.Content)
}

func TestWorkspaceMCPPrivateRuntimeAndConcurrentFactory(t *testing.T) {
	store := workspaceMCPStore(t, "http://127.0.0.1:1")
	var wg sync.WaitGroup
	owners := make(chan *Manager, 32)
	for range 32 {
		wg.Go(func() { owners <- For(store) })
	}
	wg.Wait()
	close(owners)
	for owner := range owners {
		require.Same(t, For(store), owner)
	}
	_, err := json.Marshal(For(store))
	require.Error(t, err)
	for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
		require.Equal(t, "[private workspace MCP runtime]", fmt.Sprintf(format, For(store)))
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, For(store).Close(ctx))
	require.ErrorIs(t, For(store).WaitForInit(ctx), ErrClosed)
}

func TestWorkspaceMCPRejectsAnotherStore(t *testing.T) {
	first := workspaceMCPStore(t, "http://127.0.0.1:1")
	second := workspaceMCPStore(t, "http://127.0.0.1:1")
	require.ErrorContains(t, For(first).InitializeSingle(t.Context(), "same-name", second), "different workspace")
	_, err := For(first).RunTool(t.Context(), second, "same-name", "identify", "{}")
	require.ErrorContains(t, err, "different workspace")
	require.Empty(t, For(first).GetStates())
	require.Empty(t, For(second).GetStates())
}

func TestWorkspaceMCPSubscriptionsCloseWithWorkspace(t *testing.T) {
	first, second := newManager(), newManager()
	t.Cleanup(func() { _ = first.Close(context.Background()); _ = second.Close(context.Background()) })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	a, b := first.SubscribeEvents(ctx), second.SubscribeEvents(ctx)
	for i := 0; i < 100; i++ {
		first.broker.Publish(pubsub.UpdatedEvent, Event{})
	}
	require.NoError(t, first.Close(ctx))
	for {
		select {
		case _, open := <-a:
			if !open {
				goto closed
			}
		case <-ctx.Done():
			t.Fatal("closed workspace retained a blocked event subscription")
		}
	}
closed:
	second.broker.Publish(pubsub.UpdatedEvent, Event{})
	select {
	case _, open := <-b:
		require.True(t, open, "other workspace event subscription closed")
	case <-ctx.Done():
		t.Fatal("other workspace event subscription stopped delivering")
	}
}

func TestWorkspaceMCPCanceledStartupReleasesArmedGate(t *testing.T) {
	store := config.NewTestStore(&config.Config{})
	runtime := For(store)
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	runtime.ArmInit()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	runtime.Initialize(ctx, nil, store)
	wait, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	require.ErrorIs(t, runtime.WaitForInit(wait), context.Canceled)
	require.NoError(t, wait.Err(), "terminal startup did not release the gate")
	require.Empty(t, runtime.GetStates())
	_, err := runtime.ResourceSnapshot(wait)
	require.NoError(t, err, "canceled startup must not close the whole workspace")
	require.NoError(t, runtime.Close(wait))
	_, err = runtime.ResourceSnapshot(wait)
	require.ErrorIs(t, err, ErrClosed)
}
