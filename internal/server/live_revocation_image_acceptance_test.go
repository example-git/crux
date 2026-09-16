package server_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/connection"
	"github.com/example-git/crux/internal/imagegen"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/redact"
	"github.com/example-git/crux/internal/server"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/stretchr/testify/require"
)

func TestLiveRevocationDrainsRealImageJobOnSameDaemon(t *testing.T) {
	// A credential from another workspace can match JSON punctuation. The
	// real task-output response must retain a decodable embedded JobResult.
	redact.Register(":true")
	root := t.TempDir()
	for _, name := range []string{"HOME", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "CRUX_GLOBAL_DATA", "CRUX_GLOBAL_CONFIG", "CRUX_CACHE_DIR", "AI_CLI_DIR"} {
		path := filepath.Join(root, name)
		t.Setenv(name, path)
		require.NoError(t, os.MkdirAll(path, 0o700))
	}
	t.Setenv("LIVE_IMAGE_FIXTURE_KEY", "host-value-must-not-be-used")
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	var pixels bytes.Buffer
	require.NoError(t, png.Encode(&pixels, image.NewRGBA(image.Rect(0, 0, 2, 2))))
	encoded := base64.StdEncoding.EncodeToString(pixels.Bytes())
	entered, providerCancelled := make(chan struct{}), make(chan struct{})
	var enterOnce, cancelOnce sync.Once
	var active, revokedImageCalls, retainedImageCalls atomic.Int32
	var revokedToolIssued, retainedToolIssued atomic.Bool
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Prompt string `json:"prompt"`
			Tools  []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode disposable provider request: %v", err)
			http.Error(w, "invalid fixture request", http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/images":
			if body.Prompt != "paper bird" {
				t.Errorf("image tool did not preserve the prompt: %q", body.Prompt)
				http.Error(w, "wrong fixture prompt", http.StatusBadRequest)
				return
			}
			switch r.Header.Get("X-Image-Key") {
			case "synthetic-revoked-image-key":
				revokedImageCalls.Add(1)
				active.Add(1)
				defer active.Add(-1)
				enterOnce.Do(func() { close(entered) })
				<-r.Context().Done()
				cancelOnce.Do(func() { close(providerCancelled) })
			case "synthetic-retained-image-key":
				retainedImageCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"images": []string{encoded}})
			default:
				t.Error("image request did not use its admitted client credential")
				http.Error(w, "wrong fixture credential", http.StatusUnauthorized)
			}
		case "/v1/chat/completions":
			var issued *atomic.Bool
			switch r.Header.Get("Authorization") {
			case "Bearer synthetic-revoked-inference-key":
				issued = &revokedToolIssued
			case "Bearer synthetic-retained-inference-key":
				issued = &retainedToolIssued
			default:
				t.Error("model request did not use its admitted client credential")
				http.Error(w, "wrong fixture credential", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			if r.Header.Get("x-request-purpose") == "conversation" && issued.CompareAndSwap(false, true) {
				offered := false
				for _, tool := range body.Tools {
					offered = offered || tool.Function.Name == "imagegen"
				}
				if !offered {
					t.Error("the production agent did not offer the imagegen tool")
					http.Error(w, "imagegen was not offered", http.StatusBadRequest)
					return
				}
				arguments := `{"mode":"generate","backend":"live-fixture-images","prompt":"paper bird","output":"image.png"}`
				liveImageWriteChunk(w, map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
					"index": 0, "id": "call_live_image", "type": "function",
					"function": map[string]any{"name": "imagegen", "arguments": arguments},
				}}}, nil)
				liveImageWriteChunk(w, map[string]any{}, "tool_calls")
			} else {
				liveImageWriteChunk(w, map[string]any{"role": "assistant", "content": "Image fixture request handled."}, nil)
				liveImageWriteChunk(w, map[string]any{}, "stop")
			}
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		default:
			t.Errorf("unexpected fixture route: %s", r.URL.Path)
			http.Error(w, "unexpected fixture route", http.StatusNotFound)
		}
	}))
	t.Cleanup(provider.Close)
	originalTransport := http.DefaultTransport
	fixtureTransport := provider.Client().Transport.(*http.Transport).Clone()
	http.DefaultTransport = fixtureTransport
	t.Cleanup(func() { fixtureTransport.CloseIdleConnections(); http.DefaultTransport = originalTransport })
	imageOwner, bundles := liveImageRevocationBundle(t, provider.URL)

	serverCode, err := connection.EnsureServerIdentity(ctx)
	require.NoError(t, err)
	identities := map[string]connection.Identity{}
	for _, name := range []string{"revoked", "retained"} {
		identity, err := connection.NewClientIdentity(name)
		require.NoError(t, err)
		require.NoError(t, connection.AuthorizeClient(ctx, name, identity.Certificate))
		identities[name] = identity
	}
	srv := server.NewServer(nil, "tcp", "127.0.0.1:0")
	require.NoError(t, srv.SetWorkspaceRoots([]string{root}))
	require.NoError(t, srv.EnableNetworkAuth(t.Context()))
	tlsConfig, err := connection.ServerTLSConfig(t.Context())
	require.NoError(t, err)
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(tls.NewListener(listener, tlsConfig)) }()
	t.Cleanup(func() {
		_ = srv.Close()
		for _, listed := range srv.Backend().ListWorkspaces() {
			ws, err := srv.Backend().GetWorkspace(listed.ID)
			if err == nil {
				ws.Shutdown()
			}
		}
		srv.Backend().Shutdown()
		select {
		case err := <-served:
			require.ErrorIs(t, err, http.ErrServerClosed)
		case <-time.After(5 * time.Second):
			t.Error("production listener did not stop")
		}
	})

	clients := map[string]*client.Client{}
	workspaces := map[string]*proto.Workspace{}
	sessions := map[string]string{}
	streamEnds := map[string]<-chan struct{}{}
	for _, name := range []string{"revoked", "retained"} {
		owner := providerregistry.RegistrationOwner{ProviderID: "live-image-inference"}
		proposal := config.RemoteRuntimeProposal{
			Version: config.RemoteRuntimeVersion, Revision: 1, Bundles: bundles,
			Images: &config.ImageConfiguration{
				Preferred: []providerplugin.ImageOwner{imageOwner},
				Providers: map[string]config.ImageProviderConfiguration{imageOwner.Backend: {Owner: imageOwner}},
			},
			CredentialEnvironment: map[string]string{"LIVE_IMAGE_FIXTURE_KEY": "synthetic-" + name + "-image-key"},
			Providers: []config.RemoteProviderDefinition{{Config: config.ProviderConfig{
				ID: owner.ProviderID, Name: "Image fixture inference", Type: catalog.TypeOpenAICompat, BaseURL: provider.URL + "/v1",
				Owner:  &config.ProviderOwnerReference{Type: config.ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat},
				Models: []catalog.Model{{ID: "fixture", Name: "Fixture", ContextWindow: 8192, DefaultMaxTokens: 256}},
			}}},
			Models: map[config.SelectedModelType]config.SelectedModel{
				config.SelectedModelTypeLarge: {Provider: owner.ProviderID, Model: "fixture"},
				config.SelectedModelTypeSmall: {Provider: owner.ProviderID, Model: "fixture"},
			},
			Credentials: []config.RemoteCredentialBinding{{Owner: owner, Generation: 1, APIKey: "synthetic-" + name + "-inference-key"}},
		}
		proposal.Digest, err = config.RemoteRuntimeDigest(proposal)
		require.NoError(t, err)
		c, err := client.NewAuthenticatedClient(filepath.Join(root, "client-"+name), connection.Connection{
			Address: "tcp://" + listener.Addr().String(), ServerCertificate: serverCode, Client: identities[name],
		})
		require.NoError(t, err)
		path := filepath.Join(root, "workspace-"+name)
		require.NoError(t, os.MkdirAll(path, 0o700))
		ws, err := c.CreateWorkspace(ctx, proto.Workspace{Path: path, Runtime: &proposal, AuthorityMode: "client"})
		require.NoError(t, err)
		require.NoError(t, c.InitiateAgentProcessing(ctx, ws.ID, false))
		session, err := c.CreateSession(ctx, ws.ID, "Image revocation acceptance")
		require.NoError(t, err)
		events, err := c.SubscribeEvents(ctx, ws.ID, *ws.Authority)
		require.NoError(t, err)
		ended := make(chan struct{})
		go func() {
			for range events {
			}
			close(ended)
		}()
		clients[name], workspaces[name], sessions[name], streamEnds[name] = c, ws, session.ID, ended
	}
	// This test explicitly authorizes only its synthetic tool run. The actual
	// imagegen permission service and queue remain in the production call path.
	require.NoError(t, clients["revoked"].SendMessageWithPermissionMode(ctx, workspaces["revoked"].ID, sessions["revoked"], "revoked-image-run", "Generate the fixture paper bird.", proto.AgentPermissionBypass))
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("the real imagegen tool did not reach the credentialed HTTPS image endpoint")
	}
	var running managedtask.View
	require.Eventually(t, func() bool {
		tasks, err := clients["revoked"].ListTasks(ctx, workspaces["revoked"].ID)
		if err != nil {
			return false
		}
		for _, task := range tasks {
			if task.Type == managedtask.TypeImage && task.State.Status == managedtask.StatusRunning {
				running = task
				return true
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond, "require a running image task before revocation")
	require.Equal(t, sessions["revoked"], running.Ownership.ParentSessionID)
	require.Equal(t, "call_live_image", running.Ownership.OriginToolCallID)
	require.EqualValues(t, 1, active.Load())
	require.EqualValues(t, 1, revokedImageCalls.Load())
	// Retain only a read-only task observer before the workspace is withdrawn.
	// The initiating client must lose all route access after acknowledgement.
	withdrawn, err := srv.Backend().GetWorkspace(workspaces["revoked"].ID)
	require.NoError(t, err)
	outcome, err := connection.RevokeClientWithOutcome(ctx, "revoked", "")
	require.NoError(t, err)
	require.True(t, outcome.Saved)
	require.Equal(t, "acknowledged", outcome.Resolution)
	require.Len(t, outcome.Daemons, 1)
	require.True(t, outcome.Daemons[0].Acknowledged)
	select {
	case <-providerCancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the image HTTPS provider did not observe cancellation")
	}
	require.Eventually(t, func() bool { return active.Load() == 0 }, 5*time.Second, 10*time.Millisecond)
	require.Zero(t, withdrawn.BackgroundImages.ActiveCount(), "acknowledgement must join the image job")
	require.Zero(t, withdrawn.BackgroundImages.RunningCount(), "acknowledgement must drain the image worker")
	finished, err := withdrawn.TaskOutput(ctx, running.ID, true, time.Second)
	require.NoError(t, err)
	require.Contains(t, []managedtask.Status{managedtask.StatusKilled, managedtask.StatusFailed}, finished.Task.State.Status)
	require.False(t, finished.Task.State.EndedAt.IsZero())
	var stopped imagegen.JobResult
	require.NoError(t, json.Unmarshal([]byte(finished.Output), &stopped))
	require.False(t, stopped.Success)
	require.Empty(t, stopped.Outputs)
	require.Equal(t, &imageOwner, stopped.Owner)
	require.NoFileExists(t, filepath.Join(workspaces["revoked"].Path, "image.png"))
	select {
	case <-streamEnds["revoked"]:
	case <-time.After(5 * time.Second):
		t.Fatal("the revoked workspace channel remained open")
	}
	_, err = clients["revoked"].ListTasks(ctx, workspaces["revoked"].ID)
	require.Error(t, err)
	_, err = srv.Backend().GetWorkspace(workspaces["revoked"].ID)
	require.Error(t, err)

	retained, err := clients["retained"].GetWorkspace(ctx, workspaces["retained"].ID)
	require.NoError(t, err)
	require.Equal(t, workspaces["retained"].Authority, retained.Authority)
	require.NoError(t, clients["retained"].SendMessageWithPermissionMode(ctx, retained.ID, sessions["retained"], "retained-image-run", "Generate the fixture paper bird.", proto.AgentPermissionBypass))
	var retainedTask managedtask.View
	require.Eventually(t, func() bool {
		tasks, err := clients["retained"].ListTasks(ctx, retained.ID)
		if err != nil {
			return false
		}
		for _, task := range tasks {
			if task.Type == managedtask.TypeImage && task.State.Status.Terminal() {
				retainedTask = task
				return true
			}
		}
		return false
	}, 10*time.Second, 10*time.Millisecond, "retained principal's real image task did not finish")
	require.Equal(t, managedtask.StatusCompleted, retainedTask.State.Status, retainedTask.FinalOutput)
	require.Equal(t, sessions["retained"], retainedTask.Ownership.ParentSessionID)
	result, err := clients["retained"].TaskOutput(ctx, retained.ID, retainedTask.ID, true, time.Second)
	require.NoError(t, err)
	var generated imagegen.JobResult
	require.NoError(t, json.Unmarshal([]byte(result.Output), &generated))
	require.True(t, generated.Success, generated.Error)
	require.Equal(t, &imageOwner, generated.Owner)
	require.Equal(t, []string{filepath.Join(retained.Path, "image.png")}, generated.Outputs)
	saved, err := os.ReadFile(generated.Outputs[0])
	require.NoError(t, err)
	require.Equal(t, pixels.Bytes(), saved)
	decoded, err := png.Decode(bytes.NewReader(saved))
	require.NoError(t, err)
	require.Equal(t, image.Rect(0, 0, 2, 2), decoded.Bounds())
	require.True(t, retainedToolIssued.Load())
	require.EqualValues(t, 1, retainedImageCalls.Load())
	require.EqualValues(t, 1, revokedImageCalls.Load())
	select {
	case <-streamEnds["retained"]:
		t.Fatal("retained principal's stream closed after another principal's revocation")
	default:
	}
}

func liveImageWriteChunk(w http.ResponseWriter, delta map[string]any, finish any) {
	chunk := map[string]any{
		"id": "live-image-fixture", "object": "chat.completion.chunk", "model": "fixture",
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	}
	if finish != nil {
		chunk["usage"] = map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}
	}
	data, _ := json.Marshal(chunk)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
}

func liveImageRevocationBundle(t *testing.T, endpoint string) (providerplugin.ImageOwner, []providerplugin.TransportBundle) {
	t.Helper()
	literal := func(value any) manifest.ImageValue {
		data, err := json.Marshal(value)
		require.NoError(t, err)
		return manifest.ImageValue{Literal: data}
	}
	value := manifest.ImageManifest{
		PluginType: manifest.PluginTypeImageProvider, ManifestVersion: 1, ID: "fixture.live-revocation.images", Version: "1.0.0", Name: "Revocation fixture images", Description: "Disposable image revocation acceptance",
		Publisher: manifest.Publisher{ID: "fixture", Name: "Fixture"}, Compatibility: manifest.Compatibility{HostAPI: manifest.VersionBounds{Min: 1, Max: 1}},
		Backend: "live-fixture-images", Configuration: manifest.Configuration{Schema: map[string]any{"type": "object", "additionalProperties": false}},
		Credentials: []manifest.ImageCredential{{ID: "key", Source: "environment", Environment: "LIVE_IMAGE_FIXTURE_KEY"}},
		Origins:     []manifest.ImageOrigin{{URL: endpoint, Credentials: []string{"key"}}},
		Models:      []manifest.ImageModel{{ID: "model", Name: "Model"}}, DefaultModel: "model",
		Options:  manifest.ImageOptions{Quality: []string{"auto"}, Background: []string{"auto"}, Sizes: []string{"auto"}, OutputExtension: ".png"},
		Limits:   manifest.ImageLimits{Concurrency: 1, Variants: 1, InputImages: 1, InputBytes: 1024, TotalInputBytes: 1024, OutputBytes: 1024, ResponseBytes: 4096, TimeoutSeconds: 120},
		Generate: "generate", VariantMode: "individual",
		Workflows: map[string]manifest.ImageWorkflow{"generate": {
			Steps: []manifest.ImageStep{{ID: "send", Request: &manifest.ImageRequest{
				Method: "POST", URL: literal(endpoint + "/images"), Headers: map[string]manifest.ImageValue{"X-Image-Key": {Ref: "/credentials/key"}},
				Encoding: "json", Body: &manifest.ImageValue{Object: map[string]manifest.ImageValue{"prompt": {Ref: "/request/prompt"}}},
				Response: "json", Phase: "generation", MaxBytes: 4096, TimeoutSeconds: 120,
			}}},
			Result: manifest.ImageValue{Ref: "/steps/send/body/images"},
		}},
	}
	data, err := json.Marshal(value)
	require.NoError(t, err)
	source, installation := t.TempDir(), t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(source, "manifest.json"), data, 0o600))
	manager, err := providerplugin.NewManager(t.Context(), providerplugin.DefaultPaths(filepath.Join(installation, "data"), filepath.Join(installation, "cache")))
	require.NoError(t, err)
	t.Cleanup(manager.Close)
	_, err = manager.Install(t.Context(), providerplugin.InstallRequest{Source: source, Trust: true})
	require.NoError(t, err)
	owner, err := manager.CaptureImageOwner(value.Backend)
	require.NoError(t, err)
	require.Len(t, owner.Digest, 64)
	version := manager.Snapshot()
	bundles, err := manager.ExportRegisteredBundles(version.Revision, map[string]string{owner.PluginID: owner.Digest})
	require.NoError(t, err)
	require.Len(t, bundles, 1)
	return owner, bundles
}
