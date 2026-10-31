package localaddon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/example-git/crux/internal/agent"
	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/compatibility"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/pubsub"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/sahilm/fuzzy"
	"github.com/stretchr/testify/require"
)

func protocolFixture(t *testing.T) (string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	workingDir := t.TempDir()
	executable := filepath.Join(workingDir, "crux")
	require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\nprintf 'streamed response\\n'\n"), 0o700))
	return executable, workingDir
}

func protocolInvocation(executable, workingDir string, stdin io.Reader, stdout io.Writer) compatibility.Invocation {
	return compatibility.Invocation{
		Executable: executable,
		WorkingDir: workingDir,
		Env:        []string{compatibility.BypassEnvironment + "=1", "CODEX_VERSION=9.9.9", "CODEX_HOME=" + filepath.Join(workingDir, ".codex")},
		Stdin:      stdin,
		Stdout:     stdout,
		Stderr:     io.Discard,
	}
}

func codexNativeAPIFixture(t *testing.T) string {
	return nativeAPIFixture(t, "streamed response")
}

func nativeFileSearchFixture(workspaceRoot string, request proto.WorkspaceFileSearchRequest) (proto.WorkspaceFileSearchResponse, error) {
	workspaceRoot, err := filepath.EvalSymlinks(workspaceRoot)
	if err != nil {
		return proto.WorkspaceFileSearchResponse{}, err
	}
	response := proto.WorkspaceFileSearchResponse{Roots: make([]string, 0, len(request.Roots)), Files: []proto.WorkspaceFileSearchMatch{}}
	paths := make([]string, 0)
	types := make([]string, 0)
	roots := make([]string, 0)
	for _, root := range request.Roots {
		if !filepath.IsAbs(root) {
			root = filepath.Join(workspaceRoot, root)
		}
		root, _ = filepath.Abs(filepath.Clean(root))
		root, err = filepath.EvalSymlinks(root)
		if err != nil {
			return proto.WorkspaceFileSearchResponse{}, err
		}
		relativeRoot, err := filepath.Rel(workspaceRoot, root)
		if err != nil || relativeRoot == ".." || strings.HasPrefix(relativeRoot, ".."+string(filepath.Separator)) {
			return proto.WorkspaceFileSearchResponse{}, fmt.Errorf("fuzzy file search root is outside the workspace: %s", root)
		}
		response.Roots = append(response.Roots, root)
		if request.Query == "" {
			continue
		}
		err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil || path == root {
				return walkErr
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			paths = append(paths, filepath.ToSlash(relative))
			matchType := "file"
			if entry.IsDir() {
				matchType = "directory"
			}
			types = append(types, matchType)
			roots = append(roots, root)
			return nil
		})
		if err != nil {
			return proto.WorkspaceFileSearchResponse{}, err
		}
	}
	matches := fuzzy.Find(request.Query, paths)
	for _, match := range matches {
		indices := make([]uint32, len(match.MatchedIndexes))
		for index, value := range match.MatchedIndexes {
			indices[index] = uint32(value)
		}
		response.Files = append(response.Files, proto.WorkspaceFileSearchMatch{Root: roots[match.Index], Path: paths[match.Index], MatchType: types[match.Index], FileName: filepath.Base(paths[match.Index]), Score: uint32(match.Score + 1<<16), Indices: indices})
	}
	sort.SliceStable(response.Files, func(i, j int) bool {
		if response.Files[i].Score == response.Files[j].Score {
			return response.Files[i].Path < response.Files[j].Path
		}
		return response.Files[i].Score > response.Files[j].Score
	})
	if len(response.Files) > request.Limit {
		response.Files = response.Files[:request.Limit]
	}
	return response, nil
}

func TestCodexNativeServerEnvironmentForcesCompatibilityBypass(t *testing.T) {
	require.Equal(t, []string{"PATH=/bin", compatibility.BypassEnvironment + "=1"}, codexNativeServerEnvironment([]string{"PATH=/bin", "crux_compatibility_bypass=0"}))
}

func nativeAPIFixture(t *testing.T, responseText string, onRun ...func(string, string, string, proto.AgentMessage)) string {
	t.Helper()
	return nativeAPIFixtureWithCompletionDelayAndHistory(t, proto.RunComplete{Text: responseText}, 25*time.Millisecond, true, onRun...)
}

func nativeAPIFixtureWithCompletion(t *testing.T, completion proto.RunComplete, onRun ...func(string, string, string, proto.AgentMessage)) string {
	t.Helper()
	return nativeAPIFixtureWithCompletionDelayAndHistory(t, completion, 25*time.Millisecond, true, onRun...)
}

func nativeAPIFixtureWithCompletionDelay(t *testing.T, completion proto.RunComplete, completionDelay time.Duration, onRun ...func(string, string, string, proto.AgentMessage)) string {
	t.Helper()
	return nativeAPIFixtureWithCompletionDelayAndHistory(t, completion, completionDelay, true, onRun...)
}

type nativeSessionSeed struct {
	id           string
	parentID     string
	forkedFromID string
	archived     bool
	active       bool
	messages     []proto.Message
}

type nativeTaskSeed struct {
	view managedtask.View
}

type nativeFileSearchSessionFixture struct {
	roots    []string
	updateID string
	query    string
	limit    int
}

type nativeFixtureEvent struct {
	message    proto.Message
	completion *proto.RunComplete
	sent       chan struct{}
	ack        *sync.Once
}

type failAtWriter struct {
	writer    io.Writer
	failAt    int
	writes    int
	err       error
	onFailure func()
}

func (w *failAtWriter) Write(data []byte) (int, error) {
	w.writes++
	if w.writes == w.failAt {
		if w.onFailure != nil {
			w.onFailure()
		}
		return 0, w.err
	}
	return w.writer.Write(data)
}

func nativeAPIFixtureWithCompletionDelayAndHistory(t *testing.T, completion proto.RunComplete, completionDelay time.Duration, includeHistory bool, onRun ...func(string, string, string, proto.AgentMessage)) string {
	t.Helper()
	return nativeAPIFixtureWithSessionSeeds(t, completion, completionDelay, includeHistory, nil, onRun...)
}

func nativeAPIFixtureWithSessionSeeds(t *testing.T, completion proto.RunComplete, completionDelay time.Duration, includeHistory bool, seeds []nativeSessionSeed, onRun ...func(string, string, string, proto.AgentMessage)) string {
	t.Helper()
	return nativeAPIFixtureWithSessionAndTaskSeeds(t, completion, completionDelay, includeHistory, seeds, nil, onRun...)
}

func nativeAPIFixtureWithSessionAndTaskSeeds(t *testing.T, completion proto.RunComplete, completionDelay time.Duration, includeHistory bool, seeds []nativeSessionSeed, taskSeeds []nativeTaskSeed, onRun ...func(string, string, string, proto.AgentMessage)) string {
	t.Helper()
	return nativeAPIFixtureWithSessionAndTaskSeedsAndFileSearch(t, completion, completionDelay, includeHistory, seeds, taskSeeds, nil, nil, nil, nil, nil, nil, onRun...)
}

func nativeAPIFixtureWithSessionAndTaskSeedsAndFileSearch(t *testing.T, completion proto.RunComplete, completionDelay time.Duration, includeHistory bool, seeds []nativeSessionSeed, taskSeeds []nativeTaskSeed, beforeFileSearch func(proto.WorkspaceFileSearchRequest), afterFileSearchSessionAcceptance func(proto.WorkspaceFileSearchSessionCompleteRequest), onFileSearchOwnerClose func(proto.WorkspaceFileSearchOwnerRequest), configRequirementsHandler http.HandlerFunc, agentRequestHandler func(http.ResponseWriter, *http.Request, proto.AgentMessage, func(proto.Message)) bool, onCancel func(string), onRun ...func(string, string, string, proto.AgentMessage)) string {
	t.Helper()
	responseText := completion.Text
	workingDir := t.TempDir()
	var mu sync.Mutex
	selectedProvider, selectedModel := "openai", "gpt-5"
	agentProvider, agentModel := selectedProvider, selectedModel
	sessions := make(map[string]map[string]any)
	histories := make(map[string][]proto.Message)
	tasks := make(map[string]managedtask.View)
	cancelled := make(map[string]bool)
	activeSessions := make(map[string]bool)
	fileSearchSessions := make(map[string]*nativeFileSearchSessionFixture)
	for _, seed := range taskSeeds {
		tasks[seed.view.ID] = seed.view
	}
	for _, seed := range seeds {
		sess := map[string]any{"id": seed.id, "title": seed.id, "created_at": int64(1), "updated_at": int64(1)}
		if seed.parentID != "" {
			sess["parent_session_id"] = seed.parentID
		}
		if seed.forkedFromID != "" {
			sess["forked_from_session_id"] = seed.forkedFromID
		}
		if seed.archived {
			sess["archived"] = true
		}
		sessions[seed.id] = sess
		activeSessions[seed.id] = seed.active
		if seed.messages != nil {
			histories[seed.id] = append([]proto.Message(nil), seed.messages...)
		}
	}
	events := pubsub.NewBroker[nativeFixtureEvent]()
	t.Cleanup(events.Shutdown)
	emitMessage := func(message proto.Message) {
		sent := make(chan struct{})
		events.PublishMustDeliver(t.Context(), pubsub.UpdatedEvent, nativeFixtureEvent{message: message, sent: sent, ack: &sync.Once{}})
		<-sent
	}
	providerSurfaces := func() []any {
		return []any{
			map[string]any{"id": "openai", "name": "OpenAI", "owner": map[string]any{"provider_id": "openai", "construction": "openai-compat"}, "available": true, "availability": "available", "models": []any{
				map[string]any{"id": "gpt-5", "name": "GPT 5", "default_reasoning_effort": "high", "reasoning_levels": []string{"medium", "high"}, "supports_images": true},
				map[string]any{"id": "gpt-4.1", "name": "GPT 4.1"},
			}},
			map[string]any{"id": "anthropic", "name": "Anthropic", "owner": map[string]any{"provider_id": "anthropic", "construction": "openai-compat"}, "available": true, "availability": "available", "models": []any{
				map[string]any{"id": "claude-sonnet", "name": "Claude Sonnet"},
			}},
		}
	}
	// NOTE: this fixture must serve over a Unix socket, not TCP. The real
	// client's dialPeerChannel (internal/client/workspace_channel.go) refuses
	// to open the peer channel over insecure TCP outright ("peer channel
	// refuses insecure TCP; use mutually authenticated TLS or a local
	// socket") regardless of protocol version, so a TCP-backed fixture makes
	// every codex-native-bridge call to SubscribeEvents fail before it ever
	// reaches the "/peer-channel" handler below — which then leaves the
	// app-server bridge's stdin scanner goroutine never started and the
	// test's first write to stdin blocked forever. See
	// internal/client/proto_test.go's peerChannelTestServer for the
	// canonical unix-socket-backed reference this mirrors. Do not switch
	// this back to httptest.NewServer (TCP).
	socketDir, err := os.MkdirTemp("", "crx-native-api-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "native-api.sock")
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", socketPath)
	require.NoError(t, err)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1")
		switch {
		case path == "/health":
			w.WriteHeader(http.StatusOK)
		case path == "/workspaces" && r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "workspace-1", "path": workingDir})
		case path == "/workspaces/workspace-1" && r.Method == http.MethodGet:
			mu.Lock()
			defer mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "workspace-1", "path": workingDir,
				"config":            map[string]any{"models": map[string]any{"large": map[string]any{"provider": selectedProvider, "model": selectedModel}}},
				"provider_surfaces": providerSurfaces(),
			})
		case path == "/workspaces/workspace-1/files/search" && r.Method == http.MethodPost:
			var request proto.WorkspaceFileSearchRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			if beforeFileSearch != nil {
				beforeFileSearch(request)
			}
			response, err := nativeFileSearchFixture(workingDir, request)
			if err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(proto.Error{Message: err.Error()})
				return
			}
			_ = json.NewEncoder(w).Encode(response)
		case path == "/workspaces/workspace-1/files/search/sessions/start" && r.Method == http.MethodPost:
			var request proto.WorkspaceFileSearchSessionStartRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			response, err := nativeFileSearchFixture(workingDir, proto.WorkspaceFileSearchRequest{Roots: request.Roots, Limit: 1})
			if err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(proto.Error{Message: err.Error()})
				return
			}
			key := request.Owner + "\x00" + request.SessionID
			mu.Lock()
			fileSearchSessions[key] = &nativeFileSearchSessionFixture{roots: append([]string(nil), response.Roots...)}
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{})
		case path == "/workspaces/workspace-1/files/search/sessions/begin" && r.Method == http.MethodPost:
			var request proto.WorkspaceFileSearchSessionBeginRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			key := request.Owner + "\x00" + request.SessionID
			mu.Lock()
			session := fileSearchSessions[key]
			if session == nil {
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(proto.Error{Message: "invalid workspace file search: fuzzy file search session not found: " + request.SessionID})
				return
			}
			session.updateID = uuid.NewString()
			session.query = request.Query
			session.limit = request.Limit
			updateID := session.updateID
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(proto.WorkspaceFileSearchSessionBeginResponse{UpdateID: updateID})
		case path == "/workspaces/workspace-1/files/search/sessions/complete" && r.Method == http.MethodPost:
			var request proto.WorkspaceFileSearchSessionCompleteRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			key := request.Owner + "\x00" + request.SessionID
			mu.Lock()
			session := fileSearchSessions[key]
			if session == nil || session.updateID != request.UpdateID {
				mu.Unlock()
				_ = json.NewEncoder(w).Encode(proto.WorkspaceFileSearchSessionCompleteResponse{Files: []proto.WorkspaceFileSearchMatch{}})
				return
			}
			roots := append([]string(nil), session.roots...)
			query, limit := session.query, session.limit
			session.updateID = ""
			mu.Unlock()
			response, err := nativeFileSearchFixture(workingDir, proto.WorkspaceFileSearchRequest{Query: query, Roots: roots, Limit: limit})
			result := proto.WorkspaceFileSearchSessionCompleteResponse{Accepted: true, Query: query, Files: response.Files}
			if result.Files == nil {
				result.Files = []proto.WorkspaceFileSearchMatch{}
			}
			if err != nil {
				result.Files = []proto.WorkspaceFileSearchMatch{}
				result.Error = err.Error()
			}
			mu.Lock()
			session = fileSearchSessions[key]
			accepted := session != nil && session.updateID == ""
			mu.Unlock()
			if !accepted {
				result = proto.WorkspaceFileSearchSessionCompleteResponse{Files: []proto.WorkspaceFileSearchMatch{}}
			}
			if afterFileSearchSessionAcceptance != nil {
				afterFileSearchSessionAcceptance(request)
			}
			_ = json.NewEncoder(w).Encode(result)
		case path == "/workspaces/workspace-1/files/search/sessions/stop" && r.Method == http.MethodPost:
			var request proto.WorkspaceFileSearchSessionRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			mu.Lock()
			delete(fileSearchSessions, request.Owner+"\x00"+request.SessionID)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{})
		case path == "/files/search/owners/close" && r.Method == http.MethodPost:
			var request proto.WorkspaceFileSearchOwnerRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			if onFileSearchOwnerClose != nil {
				onFileSearchOwnerClose(request)
			}
			prefix := request.Owner + "\x00"
			mu.Lock()
			for key := range fileSearchSessions {
				if strings.HasPrefix(key, prefix) {
					delete(fileSearchSessions, key)
				}
			}
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{})
		case strings.HasSuffix(path, "/auth") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(providerauth.Snapshot{
				WorkspaceID: "workspace-1",
				Generation:  providerauth.Generation{Epoch: strings.Repeat("a", 32), Sequence: 1},
				Providers:   []providerauth.Status{},
			})
		case strings.HasSuffix(path, "/providers"):
			_ = json.NewEncoder(w).Encode(providerSurfaces())
		case path == "/workspaces/workspace-1/tasks" && r.Method == http.MethodGet:
			mu.Lock()
			values := make([]managedtask.View, 0, len(tasks))
			for _, task := range tasks {
				values = append(values, task)
			}
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(values)
		case strings.HasPrefix(path, "/workspaces/workspace-1/tasks/") && strings.HasSuffix(path, "/output") && r.Method == http.MethodPost:
			parts := strings.Split(path, "/")
			taskID := parts[len(parts)-2]
			mu.Lock()
			task, exists := tasks[taskID]
			mu.Unlock()
			if !exists {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(managedtask.OutputResult{Task: task, RetrievalStatus: managedtask.RetrievalReady})
		case strings.HasPrefix(path, "/workspaces/workspace-1/tasks/") && strings.HasSuffix(path, "/stop") && r.Method == http.MethodPost:
			parts := strings.Split(path, "/")
			taskID := parts[len(parts)-2]
			mu.Lock()
			task, exists := tasks[taskID]
			if exists {
				task.State.Status = managedtask.StatusKilled
				tasks[taskID] = task
			}
			mu.Unlock()
			if !exists {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(task)
		case strings.HasSuffix(path, "/skills/catalog") && r.Method == http.MethodPost:
			var request proto.SkillCatalogRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			_ = json.NewEncoder(w).Encode(proto.SkillCatalogResponse{Skills: []proto.SkillCatalogEntry{{
				ID: filepath.Join(workingDir, ".agents", "skills", "fixture", "SKILL.md"), Name: "fixture", Description: "Fixture skill.", Source: "project", Enabled: true,
			}}})
		case path == "/config-requirements" && r.Method == http.MethodGet:
			if configRequirementsHandler != nil {
				configRequirementsHandler(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(proto.ConfigRequirements{})
		case strings.HasSuffix(path, "/config-view") && r.Method == http.MethodGet:
			workspaceID := ""
			if strings.HasPrefix(path, "/workspaces/") {
				workspaceID = strings.Split(path, "/")[2]
			}
			_ = json.NewEncoder(w).Encode(proto.ConfigReadResponse{WorkspaceID: workspaceID, View: config.ConfigReadView{Values: map[string]string{"models.large.provider": "openai", "models.large.model": "gpt-5"}, Origins: map[string]config.ConfigReadOrigin{}}})
		case strings.HasSuffix(path, "/config") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"models": map[string]any{"large": map[string]any{"provider": "openai", "model": "gpt-5"}}})
		case strings.HasSuffix(path, "/config/model-overrides") && r.Method == http.MethodPost:
			var request proto.ModelOverridesRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			if request.State.Large == nil {
				http.Error(w, "compatibility model override is missing", http.StatusBadRequest)
				return
			}
			model, owner := request.State.Large.Model, request.State.Large.Owner
			anthropicModel := model.Provider == "anthropic" && model.Model == "claude-sonnet"
			openAIModel := model.Provider == "openai" && (model.Model == "gpt-5" || model.Model == "gpt-4.1")
			if !anthropicModel && !openAIModel {
				http.Error(w, "compatibility model was not translated", http.StatusBadRequest)
				return
			}
			if owner.ProviderID != model.Provider {
				http.Error(w, "compatibility provider owner was not retained", http.StatusBadRequest)
				return
			}
			mu.Lock()
			selectedProvider, selectedModel = model.Provider, model.Model
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(request.State)
		case strings.HasSuffix(path, "/config/model") && r.Method == http.MethodPost:
			http.Error(w, "compatibility must not persist model preferences", http.StatusConflict)
		case path == "/peer-channel":
			// NOTE: this fixture used to serve the old v1 workspace channel
			// directly ("/channel", proto.WorkspaceChannelProtocol,
			// proto.WorkspaceChannelFrame). The real client
			// (internal/client/workspace_channel.go's dialPeerChannel) now
			// dials the v2 peer channel at "/v1/peer-channel" instead, so a
			// fixture still speaking v1 here left the client's dial
			// permanently unanswered: the app-server bridge would block
			// forever before it ever read its first stdin message, hanging
			// the whole test. Do not revert this back to the v1 shape; see
			// internal/client/proto_test.go's peerChannelTestServer for the
			// canonical v2 handshake this mirrors.
			connection, err := (&websocket.Upgrader{Subprotocols: []string{proto.PeerChannelProtocol}}).Upgrade(w, r, nil)
			require.NoError(t, err)
			defer connection.Close()

			_, helloData, err := connection.ReadMessage()
			require.NoError(t, err)
			hello, err := proto.DecodePeerMessage(helloData, proto.PeerDirectionClientToServer)
			require.NoError(t, err)
			require.Equal(t, proto.PeerTypeHello, hello.Envelope.Type)
			epoch := hello.Envelope.Epoch

			ack, err := proto.EncodePeerMessage(epoch, 1, "server-1", hello.Envelope.MessageID, "", proto.PeerTypeAcknowledgement, proto.PeerAcknowledgement{Status: proto.WorkspaceChannelStatusOK})
			require.NoError(t, err)
			require.NoError(t, connection.WriteMessage(websocket.TextMessage, ack))
			ready, err := proto.EncodePeerMessage(epoch, 2, "server-2", "", "", proto.PeerTypeReady, proto.PeerReady{})
			require.NoError(t, err)
			require.NoError(t, connection.WriteMessage(websocket.TextMessage, ready))

			_, attachData, err := connection.ReadMessage()
			require.NoError(t, err)
			attach, err := proto.DecodePeerMessage(attachData, proto.PeerDirectionClientToServer)
			require.NoError(t, err)
			require.Equal(t, proto.PeerTypeWorkspaceAttach, attach.Envelope.Type)
			workspaceID := attach.Envelope.WorkspaceID
			stream := events.Subscribe(r.Context())

			attachAck, err := proto.EncodePeerMessage(epoch, 3, "server-3", attach.Envelope.MessageID, workspaceID, proto.PeerTypeAcknowledgement, proto.PeerAcknowledgement{Status: proto.WorkspaceChannelStatusOK})
			require.NoError(t, err)
			require.NoError(t, connection.WriteMessage(websocket.TextMessage, attachAck))

			closed := make(chan struct{})
			go func() {
				defer close(closed)
				for {
					if _, _, err := connection.ReadMessage(); err != nil {
						return
					}
				}
			}()
			seq := uint64(4)
			for {
				select {
				case <-closed:
					return
				case value, ok := <-stream:
					if !ok {
						return
					}
					event := value.Payload
					sent := func() bool {
						if event.sent != nil {
							defer event.ack.Do(func() { close(event.sent) })
						}
						messageType := proto.PeerTypeEventMessage
						var resource any = proto.PeerResourceEvent[proto.Message]{Type: string(pubsub.UpdatedEvent), Payload: event.message}
						if event.completion != nil {
							completion := *event.completion
							messageType = proto.PeerTypeRunCompleted
							if completion.Cancelled {
								messageType = proto.PeerTypeRunCancelled
							} else if completion.Error != "" {
								messageType = proto.PeerTypeRunFailed
							}
							resource = proto.PeerResourceEvent[proto.RunComplete]{Type: string(pubsub.UpdatedEvent), Payload: completion}
						}
						data, err := proto.EncodePeerMessage(epoch, seq, fmt.Sprintf("server-%d", seq), "", workspaceID, messageType, resource)
						if err != nil {
							return false
						}
						seq++
						return connection.WriteMessage(websocket.TextMessage, data) == nil
					}()
					if !sent {
						return
					}
				}
			}
		case strings.HasSuffix(path, "/shell") && strings.Contains(path, "/agent/sessions/") && r.Method == http.MethodPost:
			parts := strings.Split(path, "/")
			sessionID := parts[len(parts)-2]
			var request proto.ShellCommandRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			mu.Lock()
			_, exists := sessions[sessionID]
			mu.Unlock()
			if !exists {
				http.Error(w, "compatibility ID was not translated", http.StatusBadRequest)
				return
			}
			if request.Command == "wait" {
				select {
				case <-time.After(completionDelay):
				case <-r.Context().Done():
					return
				}
			}
			response := proto.ShellCommandResponse{Output: "shell:" + request.Command + "\n"}
			if request.Command == "exit 7" {
				response.ExitCode = 7
			}
			_ = json.NewEncoder(w).Encode(response)
		case strings.HasSuffix(path, "/summarize") && strings.Contains(path, "/agent/sessions/") && r.Method == http.MethodPost:
			parts := strings.Split(path, "/")
			sessionID := parts[len(parts)-2]
			mu.Lock()
			_, exists := sessions[sessionID]
			if !exists {
				mu.Unlock()
				http.NotFound(w, r)
				return
			}
			if activeSessions[sessionID] {
				mu.Unlock()
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(proto.Error{Message: "thread " + sessionID + " already has an active writer"})
				return
			}
			activeSessions[sessionID] = true
			mu.Unlock()
			select {
			case <-time.After(completionDelay):
				mu.Lock()
				activeSessions[sessionID] = false
				mu.Unlock()
				w.WriteHeader(http.StatusOK)
			case <-r.Context().Done():
				mu.Lock()
				activeSessions[sessionID] = false
				mu.Unlock()
			}
		case strings.HasSuffix(path, "/agent/update"):
			var request proto.AgentUpdateRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			if request.State.Large != nil {
				mu.Lock()
				agentProvider, agentModel = request.State.Large.Model.Provider, request.State.Large.Model.Model
				mu.Unlock()
			}
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(path, "/agent/init"):
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(path, "/agent") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"is_ready": true})
		case strings.HasSuffix(path, "/agent") && r.Method == http.MethodPost:
			var request proto.AgentMessage
			_ = json.NewDecoder(r.Body).Decode(&request)
			sessionID := request.SessionID
			mu.Lock()
			sess, exists := sessions[sessionID]
			provider, model := agentProvider, agentModel
			if request.AnalysisEffort == "" && sess != nil {
				request.AnalysisEffort, _ = sess["analysis_effort"].(string)
			}
			mu.Unlock()
			if !exists {
				http.Error(w, "compatibility ID was not translated", http.StatusBadRequest)
				return
			}
			if agentRequestHandler != nil && agentRequestHandler(w, r, request, emitMessage) {
				return
			}
			runID := request.RunID
			if runID != "" {
				mu.Lock()
				activeSessions[sessionID] = true
				mu.Unlock()
			}
			w.WriteHeader(http.StatusAccepted)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			for _, observe := range onRun {
				observe(sessionID, provider, model, request)
			}
			if runID != "" {
				go func() {
					time.Sleep(completionDelay)
					mu.Lock()
					wasCancelled := cancelled[sessionID]
					activeSessions[sessionID] = false
					mu.Unlock()
					event := completion
					event.SessionID = sessionID
					event.RunID = runID
					event.MessageID = "message-1"
					event.Cancelled = event.Cancelled || wasCancelled
					events.PublishMustDeliver(t.Context(), pubsub.UpdatedEvent, nativeFixtureEvent{completion: &event})
				}()
			}
		case strings.HasSuffix(path, "/cancel"):
			parts := strings.Split(path, "/")
			sessionID := parts[len(parts)-2]
			if sessionID != "thread-1" {
				http.Error(w, "compatibility ID was not translated", http.StatusBadRequest)
				return
			}
			mu.Lock()
			cancelled[sessionID] = true
			mu.Unlock()
			if onCancel != nil {
				onCancel(sessionID)
			}
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(path, "/fork"):
			parts := strings.Split(path, "/")
			sourceID := parts[len(parts)-2]
			if sourceID != "thread-1" {
				http.Error(w, "compatibility ID was not translated", http.StatusBadRequest)
				return
			}
			mu.Lock()
			source := sessions[sourceID]
			forked := map[string]any{"id": "fork-" + sourceID, "title": source["title"], "model": source["model"], "provider": source["provider"], "analysis_effort": source["analysis_effort"], "created_at": int64(1), "updated_at": int64(1)}
			if r.URL.Query().Get("record_origin") == "true" {
				forked["forked_from_session_id"] = sourceID
			}
			sessions[forked["id"].(string)] = forked
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(forked)
		case strings.HasSuffix(path, "/archive") && strings.Contains(path, "/sessions/") && r.Method == http.MethodPost:
			parts := strings.Split(path, "/")
			sessionID := parts[len(parts)-2]
			mu.Lock()
			sess := sessions[sessionID]
			if activeSessions[sessionID] {
				mu.Unlock()
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(proto.Error{Message: "thread " + sessionID + " already has an active writer"})
				return
			}
			if sess != nil {
				sess["archived"] = true
			}
			mu.Unlock()
			if sess == nil {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{sess})
		case strings.HasSuffix(path, "/unarchive") && strings.Contains(path, "/sessions/") && r.Method == http.MethodPost:
			parts := strings.Split(path, "/")
			sessionID := parts[len(parts)-2]
			mu.Lock()
			sess := sessions[sessionID]
			if activeSessions[sessionID] {
				mu.Unlock()
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(proto.Error{Message: "thread " + sessionID + " already has an active writer"})
				return
			}
			if sess == nil {
				mu.Unlock()
				http.NotFound(w, r)
				return
			}
			if archived, _ := sess["archived"].(bool); !archived {
				mu.Unlock()
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(proto.Error{Message: "session is not archived"})
				return
			}
			sess["archived"] = false
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(sess)
		case strings.HasSuffix(path, "/delete") && strings.Contains(path, "/sessions/") && r.Method == http.MethodPost:
			parts := strings.Split(path, "/")
			sessionID := parts[len(parts)-2]
			mu.Lock()
			root := sessions[sessionID]
			if root == nil {
				mu.Unlock()
				http.NotFound(w, r)
				return
			}
			subtree := []map[string]any{root}
			for index := 0; index < len(subtree); index++ {
				parentID := subtree[index]["id"].(string)
				for _, candidate := range sessions {
					if candidate["parent_session_id"] == parentID {
						subtree = append(subtree, candidate)
					}
				}
			}
			for _, sess := range subtree {
				id := sess["id"].(string)
				if activeSessions[id] {
					mu.Unlock()
					w.WriteHeader(http.StatusConflict)
					_ = json.NewEncoder(w).Encode(proto.Error{Message: "thread " + id + " already has an active writer"})
					return
				}
			}
			deleted := make([]map[string]any, 0, len(subtree))
			for index := len(subtree) - 1; index >= 0; index-- {
				sess := subtree[index]
				delete(sessions, sess["id"].(string))
				deleted = append(deleted, sess)
			}
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(deleted)
		case strings.HasSuffix(path, "/title") && strings.Contains(path, "/sessions/") && r.Method == http.MethodPut:
			parts := strings.Split(path, "/")
			sessionID := parts[len(parts)-2]
			var request proto.Session
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			mu.Lock()
			sess := sessions[sessionID]
			if sess != nil {
				sess["title"] = request.Title
			}
			mu.Unlock()
			if sess == nil {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(sess)
		case strings.HasSuffix(path, "/analysis-effort") && strings.Contains(path, "/sessions/") && r.Method == http.MethodPut:
			parts := strings.Split(path, "/")
			sessionID := parts[len(parts)-2]
			var request proto.Session
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			mu.Lock()
			sess := sessions[sessionID]
			if sess != nil {
				sess["analysis_effort"] = request.AnalysisEffort
			}
			mu.Unlock()
			if sess == nil {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(sess)
		case strings.HasSuffix(path, "/rollback") && strings.Contains(path, "/sessions/") && r.Method == http.MethodPost:
			parts := strings.Split(path, "/")
			sessionID := parts[len(parts)-2]
			var request struct {
				MessageID string `json:"message_id"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			mu.Lock()
			sess := sessions[sessionID]
			if sess == nil {
				mu.Unlock()
				http.NotFound(w, r)
				return
			}
			if activeSessions[sessionID] {
				mu.Unlock()
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(proto.Error{Message: agent.ErrSessionBusy.Error()})
				return
			}
			messages, exists := histories[sessionID]
			if !exists && includeHistory {
				messages = []proto.Message{
					{ID: sessionID + "-history-user", Role: proto.User, SessionID: sessionID, CreatedAt: 10, UpdatedAt: 10, Parts: []proto.ContentPart{proto.TextContent{Text: "remember this"}, proto.Finish{Reason: "stop"}}},
					{ID: sessionID + "-history-assistant", Role: proto.Assistant, SessionID: sessionID, CreatedAt: 11, UpdatedAt: 15, Parts: []proto.ContentPart{proto.TextContent{Text: responseText}, proto.Finish{Reason: proto.FinishReasonEndTurn}}},
				}
			}
			cutoff := -1
			for index := range messages {
				if messages[index].ID == request.MessageID {
					cutoff = index
					break
				}
			}
			if cutoff < 0 {
				mu.Unlock()
				http.Error(w, "message not found", http.StatusNotFound)
				return
			}
			messages = append([]proto.Message(nil), messages[:cutoff]...)
			histories[sessionID] = messages
			sess["message_count"] = int64(len(messages))
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"session": sess, "messages": messages})
		case strings.HasSuffix(path, "/messages") && strings.Contains(path, "/sessions/") && r.Method == http.MethodGet:
			parts := strings.Split(path, "/")
			sessionID := parts[len(parts)-2]
			mu.Lock()
			_, exists := sessions[sessionID]
			if !exists {
				mu.Unlock()
				http.NotFound(w, r)
				return
			}
			messages, stored := histories[sessionID]
			if !stored && includeHistory {
				messages = []proto.Message{
					{ID: sessionID + "-history-user", Role: proto.User, SessionID: sessionID, CreatedAt: 10, UpdatedAt: 10, Parts: []proto.ContentPart{proto.TextContent{Text: "remember this"}, proto.Finish{Reason: "stop"}}},
					{ID: sessionID + "-history-assistant", Role: proto.Assistant, SessionID: sessionID, CreatedAt: 11, UpdatedAt: 15, Parts: []proto.ContentPart{proto.TextContent{Text: responseText}, proto.Finish{Reason: proto.FinishReasonEndTurn}}},
				}
				histories[sessionID] = messages
			}
			messages = append([]proto.Message(nil), messages...)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(messages)
		case strings.Contains(path, "/sessions/") && r.Method == http.MethodGet:
			parts := strings.Split(path, "/")
			id := parts[len(parts)-1]
			mu.Lock()
			sess := sessions[id]
			mu.Unlock()
			if sess == nil {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(sess)
		case strings.Contains(path, "/sessions/") && r.Method == http.MethodPut:
			parts := strings.Split(path, "/")
			id := parts[len(parts)-1]
			var updated map[string]any
			if err := json.NewDecoder(r.Body).Decode(&updated); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mu.Lock()
			if sessions[id] == nil {
				mu.Unlock()
				http.NotFound(w, r)
				return
			}
			updated["id"] = id
			sessions[id] = updated
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(updated)
		case strings.Contains(path, "/sessions/") && r.Method == http.MethodDelete:
			parts := strings.Split(path, "/")
			id := parts[len(parts)-1]
			mu.Lock()
			delete(sessions, id)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(path, "/sessions") && r.Method == http.MethodPost:
			mu.Lock()
			id := fmt.Sprintf("thread-%d", len(sessions)+1)
			sess := map[string]any{"id": id, "title": "Codex", "created_at": int64(1), "updated_at": int64(1)}
			sessions[id] = sess
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(sess)
		case strings.HasSuffix(path, "/sessions") && r.Method == http.MethodGet:
			archived := r.URL.Query().Get("archived") == "true"
			mu.Lock()
			values := make([]map[string]any, 0, len(sessions))
			for _, sess := range sessions {
				isArchived, _ := sess["archived"].(bool)
				if isArchived == archived {
					values = append(values, sess)
				}
			}
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(values)
		case strings.HasPrefix(path, "/clients/") && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	})
	server := &http.Server{Handler: handler}
	finished := make(chan error, 1)
	go func() { finished <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		_ = listener.Close()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("native API fixture did not stop")
		}
	})
	previous := codexClientFactory
	codexClientFactory = func(path string) (*client.Client, error) { return client.NewClient(path, "unix", socketPath) }
	t.Cleanup(func() { codexClientFactory = previous })
	return workingDir
}

func decodeJSONLines(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	var values []map[string]any
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		var value map[string]any
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &value))
		values = append(values, value)
	}
	require.NoError(t, scanner.Err())
	return values
}

func TestAgyStreamJSONMaintainsConversationAcrossTurns(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	executable := "crux"
	stdin := bytes.NewBufferString("{\"event\":\"user\",\"message\":{\"content\":\"first\"}}\n{\"event\":\"user\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"second\"}]}}\n")
	var stdout bytes.Buffer
	request := compatibility.Request{
		Source: "agy", Style: compatibility.ExecutionHeadless, WorkingDir: workingDir,
		Prompt:  compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: stdin},
		Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true},
		Output:  compatibility.Output{Mode: compatibility.OutputStreamJSON},
	}
	require.NoError(t, runAgyStream(t.Context(), protocolInvocation(executable, workingDir, stdin, &stdout), request))
	lines := decodeJSONLines(t, stdout.Bytes())
	require.Equal(t, "init", lines[0]["event"])
	require.Equal(t, "thread-1", lines[0]["conversation_id"])
	var results []map[string]any
	for _, line := range lines {
		if line["event"] == "result" {
			results = append(results, line["result"].(map[string]any))
		}
	}
	require.Len(t, results, 2)
	require.Equal(t, results[0]["conversation_id"], results[1]["conversation_id"])
	require.Equal(t, float64(2), results[1]["num_turns"])
}

func TestAgyStreamJSONAppliesPerTurnTimeout(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	executable := "crux"
	stdin := bytes.NewBufferString("{\"event\":\"user\",\"message\":{\"content\":\"hello\"}}\n")
	var stdout bytes.Buffer
	request := compatibility.Request{Source: "agy", Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: stdin}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}, Output: compatibility.Output{Mode: compatibility.OutputStreamJSON}, Limits: compatibility.Limits{Timeout: 10 * time.Millisecond}}
	err := runAgyStream(t.Context(), protocolInvocation(executable, workingDir, stdin, &stdout), request)
	exitError, ok := errors.AsType[*compatibility.ExitError](err)
	require.True(t, ok)
	require.Equal(t, 1, exitError.Code)
	lines := decodeJSONLines(t, stdout.Bytes())
	result := lines[len(lines)-1]["result"].(map[string]any)
	require.Equal(t, "ERROR", result["status"])
	require.Contains(t, result["error"], "timed out")
}

func TestClaudeStreamJSONEmitsControlAndPartialEvents(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	executable := "crux"
	stdin := bytes.NewBufferString("{\"type\":\"control_request\",\"request_id\":\"r1\",\"request\":{\"subtype\":\"initialize\"}}\n{\"type\":\"user\",\"uuid\":\"u1\",\"message\":{\"role\":\"user\",\"content\":\"hello\"},\"parent_tool_use_id\":null}\n")
	var stdout bytes.Buffer
	request := compatibility.Request{
		Source: "claude", Style: compatibility.ExecutionHeadless, WorkingDir: workingDir,
		Prompt:   compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: stdin},
		Session:  compatibility.Session{Mode: compatibility.SessionNew, Persistent: true},
		Output:   compatibility.Output{Mode: compatibility.OutputStreamJSON},
		Metadata: map[string]string{"include-partial-messages": "true", "replay-user-messages": "true"},
	}
	require.NoError(t, runClaudeStream(t.Context(), protocolInvocation(executable, workingDir, stdin, &stdout), request))
	lines := decodeJSONLines(t, stdout.Bytes())
	require.Equal(t, "system", lines[0]["type"])
	require.Equal(t, "thread-1", lines[0]["session_id"])
	types := make(map[string]bool)
	streamEvents := make(map[string]bool)
	for _, line := range lines {
		types[line["type"].(string)] = true
		if line["type"] == "stream_event" {
			streamEvents[line["event"].(map[string]any)["type"].(string)] = true
		}
	}
	require.True(t, types["control_response"])
	require.True(t, types["user"])
	require.True(t, types["stream_event"])
	require.True(t, types["assistant"])
	require.True(t, types["result"])
	for _, eventType := range []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"} {
		require.True(t, streamEvents[eventType], eventType)
	}
}

func TestClaudeStreamJSONReturnsSchemaShapedControlResponses(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	executable := "crux"
	stdin := bytes.NewBufferString("{\"type\":\"control_request\",\"request_id\":\"context\",\"request\":{\"subtype\":\"get_context_usage\"}}\n{\"type\":\"control_request\",\"request_id\":\"cancel\",\"request\":{\"subtype\":\"cancel_async_message\",\"message_uuid\":\"missing\"}}\n{\"type\":\"control_request\",\"request_id\":\"unsupported\",\"request\":{\"subtype\":\"mcp_message\"}}\n")
	var stdout bytes.Buffer
	request := compatibility.Request{Source: "claude", Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: stdin}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}, Output: compatibility.Output{Mode: compatibility.OutputStreamJSON}}
	require.NoError(t, runClaudeStream(t.Context(), protocolInvocation(executable, workingDir, stdin, &stdout), request))
	lines := decodeJSONLines(t, stdout.Bytes())
	require.Len(t, lines, 4)
	contextResponse := lines[1]["response"].(map[string]any)
	contextResult := contextResponse["response"].(map[string]any)
	require.Contains(t, contextResult, "categories")
	require.Contains(t, contextResult, "gridRows")
	cancelResponse := lines[2]["response"].(map[string]any)["response"].(map[string]any)
	require.Equal(t, false, cancelResponse["cancelled"])
	unsupported := lines[3]["response"].(map[string]any)
	require.Equal(t, "error", unsupported["subtype"])
}

func TestClaudeStreamJSONInterruptsActiveTurn(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	executable := "crux"
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "claude", Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}, Output: compatibility.Output{Mode: compatibility.OutputStreamJSON}}
	done := make(chan error, 1)
	go func() {
		done <- runClaudeStream(t.Context(), protocolInvocation(executable, workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder := json.NewEncoder(inputWriter)
	decoder := json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "system", response["type"])
	require.NoError(t, encoder.Encode(map[string]any{"type": "user", "uuid": "u1", "message": map[string]any{"role": "user", "content": "hello"}, "parent_tool_use_id": nil}))
	require.NoError(t, encoder.Encode(map[string]any{"type": "control_request", "request_id": "r1", "request": map[string]any{"subtype": "interrupt", "cancel_queued": true}}))
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "control_response", response["type"])
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "result", response["type"])
	require.Equal(t, "error_during_execution", response["subtype"])
	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestClaudeSDKWebSocketTransport(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	executable := "crux"
	resultReceived := make(chan bool, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(writer, request, nil)
		require.NoError(t, err)
		defer connection.Close()
		_, initial, err := connection.ReadMessage()
		require.NoError(t, err)
		var message map[string]any
		require.NoError(t, json.Unmarshal(initial, &message))
		require.Equal(t, "system", message["type"])
		require.NoError(t, connection.WriteJSON(map[string]any{"type": "user", "uuid": "u1", "message": map[string]any{"role": "user", "content": "hello"}}))
		for {
			_, data, err := connection.ReadMessage()
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(data, &message))
			if message["type"] == "result" {
				resultReceived <- true
				require.NoError(t, connection.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second)))
				return
			}
		}
	}))
	defer server.Close()
	request := compatibility.Request{Source: "claude", Protocol: compatibility.ProtocolClaudeSDK, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}, Output: compatibility.Output{Mode: compatibility.OutputStreamJSON}, Metadata: map[string]string{"sdk-url": "ws" + server.URL[4:]}}
	err := runClaudeSDK(t.Context(), protocolInvocation(executable, workingDir, bytes.NewReader(nil), io.Discard), request)
	require.NoError(t, err, "%T %#v", err, err)
	require.True(t, <-resultReceived)
}

func TestCopilotACPStdioLifecycle(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	executable := "crux"
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{
		Source: "copilot", Protocol: compatibility.ProtocolCopilotACP, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir,
		Prompt:  compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader},
		Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true},
	}
	done := make(chan error, 1)
	go func() {
		done <- runCopilotACP(t.Context(), protocolInvocation(executable, workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder := json.NewEncoder(inputWriter)
	decoder := json.NewDecoder(outputReader)
	require.NoError(t, encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}}))
	var response map[string]any
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(1), response["id"])
	require.NoError(t, encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "session/new", "params": map[string]any{"cwd": workingDir, "mcpServers": []any{}}}))
	require.NoError(t, decoder.Decode(&response))
	sessionID := response["result"].(map[string]any)["sessionId"].(string)
	require.NoError(t, encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "session/prompt", "params": map[string]any{"sessionId": sessionID, "prompt": []any{map[string]any{"type": "text", "text": "hello"}}}}))
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "session/update", response["method"])
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "usage_update", response["params"].(map[string]any)["update"].(map[string]any)["sessionUpdate"])
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(3), response["id"])
	require.Equal(t, "end_turn", response["result"].(map[string]any)["stopReason"])
	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCopilotACPCancelsActivePrompt(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	executable := "crux"
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "copilot", Protocol: compatibility.ProtocolCopilotACP, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCopilotACP(t.Context(), protocolInvocation(executable, workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder := json.NewEncoder(inputWriter)
	decoder := json.NewDecoder(outputReader)
	require.NoError(t, encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}}))
	var response map[string]any
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "session/new", "params": map[string]any{"cwd": workingDir, "mcpServers": []any{}}}))
	require.NoError(t, decoder.Decode(&response))
	sessionID := response["result"].(map[string]any)["sessionId"].(string)
	require.NoError(t, encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "session/prompt", "params": map[string]any{"sessionId": sessionID, "prompt": []any{map[string]any{"type": "text", "text": "hello"}}}}))
	require.NoError(t, encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]any{"sessionId": sessionID}}))
	for response["id"] != float64(3) {
		response = nil
		require.NoError(t, decoder.Decode(&response))
	}
	require.Equal(t, "cancelled", response["result"].(map[string]any)["stopReason"])
	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCopilotSDKStdioLifecycle(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	executable := "crux"
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "copilot", Protocol: compatibility.ProtocolCopilotSDK, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCopilotSDK(t.Context(), protocolInvocation(executable, workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder := &protocolOutput{writer: inputWriter, contentLength: true}
	reader := bufio.NewReader(outputReader)
	decode := func(response *map[string]any) {
		payload, err := readCopilotSDKFrame(reader)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(payload, response))
	}
	var response map[string]any
	require.NoError(t, encoder.write(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "connect", "params": map[string]any{}}))
	decode(&response)
	require.Equal(t, float64(3), response["result"].(map[string]any)["protocolVersion"])
	require.NoError(t, encoder.write(map[string]any{"jsonrpc": "2.0", "id": 10, "method": "status.get", "params": map[string]any{}}))
	decode(&response)
	require.Equal(t, float64(3), response["result"].(map[string]any)["protocolVersion"])
	require.NoError(t, encoder.write(map[string]any{"jsonrpc": "2.0", "id": 11, "method": "auth.getStatus", "params": map[string]any{}}))
	decode(&response)
	require.Equal(t, true, response["result"].(map[string]any)["isAuthenticated"])
	require.NoError(t, encoder.write(map[string]any{"jsonrpc": "2.0", "id": 12, "method": "models.list", "params": map[string]any{}}))
	decode(&response)
	models := response["result"].(map[string]any)["models"].([]any)
	require.NotEmpty(t, models)
	require.Contains(t, models[0].(map[string]any), "capabilities")
	requestedSessionID := "copilot-client-session"
	require.NoError(t, encoder.write(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "session.create", "params": map[string]any{"sessionId": requestedSessionID, "workingDirectory": workingDir, "streaming": true}}))
	decode(&response)
	sessionID := response["result"].(map[string]any)["sessionId"].(string)
	require.Equal(t, requestedSessionID, sessionID)
	decode(&response)
	require.Equal(t, "session.start", response["params"].(map[string]any)["event"].(map[string]any)["type"])
	require.NoError(t, encoder.write(map[string]any{"jsonrpc": "2.0", "id": 13, "method": "session.model.getCurrent", "params": map[string]any{"sessionId": sessionID}}))
	decode(&response)
	require.Equal(t, "openai/gpt-5", response["result"].(map[string]any)["modelId"])
	require.NoError(t, encoder.write(map[string]any{"jsonrpc": "2.0", "id": 14, "method": "session.model.list", "params": map[string]any{"sessionId": sessionID}}))
	decode(&response)
	require.NotEmpty(t, response["result"].(map[string]any)["list"])
	require.NoError(t, encoder.write(map[string]any{"jsonrpc": "2.0", "id": 15, "method": "session.model.switchTo", "params": map[string]any{"sessionId": sessionID, "modelId": "anthropic/claude-sonnet"}}))
	decode(&response)
	require.Equal(t, "anthropic/claude-sonnet", response["result"].(map[string]any)["modelId"])
	require.Equal(t, false, response["result"].(map[string]any)["deferred"])
	require.NoError(t, encoder.write(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "session.send", "params": map[string]any{"sessionId": sessionID, "prompt": "hello"}}))
	decode(&response)
	require.NotEmpty(t, response["result"].(map[string]any)["messageId"])
	idle := false
	for !idle {
		decode(&response)
		if response["method"] == "session.event" {
			idle = response["params"].(map[string]any)["event"].(map[string]any)["type"] == "session.idle"
		}
	}
	require.NoError(t, encoder.write(map[string]any{"jsonrpc": "2.0", "id": 4, "method": "session.eventLog.read", "params": map[string]any{"sessionId": sessionID}}))
	decode(&response)
	require.NotEmpty(t, response["result"].(map[string]any)["events"])
	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexInitializeParamsValidateCapabilities(t *testing.T) {
	state, name, version, err := parseCodexInitializeParams(json.RawMessage(`{"clientInfo":{"name":"desktop","version":"1","title":null},"capabilities":{"experimentalApi":true,"requestAttestation":false,"mcpServerOpenaiFormElicitation":true,"optOutNotificationMethods":["thread/started","thread/started"],"extensions":{"openai/form":{}}}}`))
	require.NoError(t, err)
	require.Equal(t, "desktop", name)
	require.Equal(t, "1", version)
	require.True(t, state.experimentalAPI)
	require.False(t, state.requestAttestation)
	require.True(t, state.mcpServerOpenAIFormElicitation)
	require.Equal(t, map[string]struct{}{"thread/started": {}}, state.optOutNotificationMethods)
	require.Contains(t, state.extensions, "openai/form")

	state, _, _, err = parseCodexInitializeParams(json.RawMessage(`{"clientInfo":{"name":"desktop","version":"1"},"capabilities":null}`))
	require.NoError(t, err)
	require.Empty(t, state.optOutNotificationMethods)

	for _, params := range []string{
		`null`,
		`{}`,
		`{"clientInfo":null}`,
		`{"clientInfo":{"name":"desktop"}}`,
		`{"clientInfo":{"name":"desktop","version":"1"},"capabilities":{"experimentalApi":null}}`,
		`{"clientInfo":{"name":"desktop","version":"1"},"capabilities":{"requestAttestation":null}}`,
		`{"clientInfo":{"name":"desktop","version":"1"},"capabilities":{"mcpServerOpenaiFormElicitation":null}}`,
		`{"clientInfo":{"name":"desktop","version":"1"},"capabilities":{"optOutNotificationMethods":[1]}}`,
	} {
		_, _, _, err := parseCodexInitializeParams(json.RawMessage(params))
		require.Error(t, err, params)
	}
	_, _, _, err = parseCodexInitializeParams(json.RawMessage("{\"clientInfo\":{\"name\":\"bad\\nname\",\"version\":\"1\"}}"))
	require.EqualError(t, err, "Invalid clientInfo.name: 'bad\nname'. Must be a valid HTTP header value.")
}

func TestCodexAppServerInitializationAndNotificationOptOut(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{
		Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir,
		Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true},
	}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any

	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "account/read", "params": map[string]any{}}))
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(-32600), response["error"].(map[string]any)["code"])
	require.Equal(t, "Not initialized", response["error"].(map[string]any)["message"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "initialize", "params": map[string]any{}}))
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(-32600), response["error"].(map[string]any)["code"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "desktop", "version": "1"}, "capabilities": map[string]any{"optOutNotificationMethods": []string{"thread/started", "thread/status/changed"}}}}))
	require.NoError(t, decoder.Decode(&response))
	result := response["result"].(map[string]any)
	home := result["codexHome"].(string)
	require.True(t, filepath.IsAbs(home))
	require.NotEqual(t, filepath.Clean(workingDir), home)

	for index, method := range []string{"plugin/list", "thread/goal/set", "thread/goal/get", "thread/goal/clear", "thread/metadata/update"} {
		require.NoError(t, encoder.Encode(map[string]any{"id": 31 + index, "method": method, "params": map[string]any{}}))
		require.NoError(t, decoder.Decode(&response))
		require.Equal(t, float64(-32601), response["error"].(map[string]any)["code"])
		require.Equal(t, "Method not supported by the Crux compatibility layer", response["error"].(map[string]any)["message"])
	}

	require.NoError(t, encoder.Encode(map[string]any{"id": 4, "method": "thread/start", "params": map[string]any{"cwd": workingDir}}))
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(4), response["id"])
	threadID := response["result"].(map[string]any)["thread"].(map[string]any)["id"].(string)
	require.NoError(t, encoder.Encode(map[string]any{"id": 5, "method": "turn/start", "params": map[string]any{"threadId": threadID, "input": []any{map[string]any{"type": "text", "text": "hello"}}}}))
	completed := false
	for !completed {
		response = nil
		require.NoError(t, decoder.Decode(&response))
		method, _ := response["method"].(string)
		require.NotEqual(t, "thread/started", method)
		require.NotEqual(t, "thread/status/changed", method)
		completed = method == "turn/completed"
	}
	require.NoError(t, inputWriter.Close())
	require.ErrorIs(t, decoder.Decode(&response), io.EOF)
	require.NoError(t, <-done)
}

func TestCodexAppServerPersistsAndClearsTurnEffort(t *testing.T) {
	var effortsMu sync.Mutex
	var efforts []string
	workingDir := nativeAPIFixture(t, "streamed response", func(_ string, _ string, _ string, message proto.AgentMessage) {
		effortsMu.Lock()
		efforts = append(efforts, message.AnalysisEffort)
		effortsMu.Unlock()
	})
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{
		Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir,
		Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true},
	}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{"cwd": workingDir}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	threadID := response["result"].(map[string]any)["thread"].(map[string]any)["id"].(string)
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "thread/started", response["method"])

	runTurn := func(id int, effort any, includeEffort bool) {
		params := map[string]any{"threadId": threadID, "input": []any{map[string]any{"type": "text", "text": "hello"}}}
		if includeEffort {
			params["effort"] = effort
		}
		require.NoError(t, encoder.Encode(map[string]any{"id": id, "method": "turn/start", "params": params}))
		completed := false
		for !completed {
			response = nil
			require.NoError(t, decoder.Decode(&response))
			completed = response["method"] == "turn/completed"
		}
	}
	runTurn(3, "high", true)
	runTurn(4, nil, false)
	runTurn(5, nil, true)

	for index, invalid := range []struct {
		effort  any
		message string
	}{
		{effort: "", message: "effort must not be empty"},
		{effort: "extreme", message: `invalid analysis effort "extreme"`},
		{effort: 1, message: "effort must be a string or null"},
	} {
		require.NoError(t, encoder.Encode(map[string]any{"id": 6 + index, "method": "turn/start", "params": map[string]any{"threadId": threadID, "effort": invalid.effort, "input": []any{map[string]any{"type": "text", "text": "ignored"}}}}))
		response = nil
		require.NoError(t, decoder.Decode(&response))
		require.Equal(t, float64(-32602), response["error"].(map[string]any)["code"])
		require.Equal(t, invalid.message, response["error"].(map[string]any)["message"])
	}

	effortsMu.Lock()
	require.Equal(t, []string{"high", "high", ""}, efforts)
	effortsMu.Unlock()
	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerPersistsTurnPolicy(t *testing.T) {
	var modesMu sync.Mutex
	var modes []proto.AgentPermissionMode
	workingDir := nativeAPIFixture(t, "streamed response", func(_ string, _ string, _ string, message proto.AgentMessage) {
		modesMu.Lock()
		modes = append(modes, message.PermissionMode)
		modesMu.Unlock()
	})
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{
		Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir,
		Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true},
	}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{"cwd": workingDir}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	threadID := response["result"].(map[string]any)["thread"].(map[string]any)["id"].(string)
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "thread/started", response["method"])

	runTurn := func(id int, overrides map[string]any) {
		params := map[string]any{"threadId": threadID, "input": []any{map[string]any{"type": "text", "text": "hello"}}}
		for name, value := range overrides {
			params[name] = value
		}
		require.NoError(t, encoder.Encode(map[string]any{"id": id, "method": "turn/start", "params": params}))
		for {
			response = nil
			require.NoError(t, decoder.Decode(&response))
			if response["method"] == "turn/completed" {
				return
			}
		}
	}
	runTurn(3, map[string]any{"approvalPolicy": "never", "sandboxPolicy": map[string]any{"type": "readOnly"}})
	runTurn(4, nil)

	for index, invalid := range []struct {
		params  map[string]any
		message string
	}{
		{params: map[string]any{"sandboxPolicy": map[string]any{"type": "readOnly", "networkAccess": true}}, message: "sandboxPolicy.networkAccess is not supported"},
	} {
		params := map[string]any{"threadId": threadID, "input": []any{map[string]any{"type": "text", "text": "ignored"}}}
		for name, value := range invalid.params {
			params[name] = value
		}
		require.NoError(t, encoder.Encode(map[string]any{"id": 5 + index, "method": "turn/start", "params": params}))
		response = nil
		require.NoError(t, decoder.Decode(&response))
		require.Equal(t, float64(-32602), response["error"].(map[string]any)["code"])
		require.Equal(t, invalid.message, response["error"].(map[string]any)["message"])
	}

	modesMu.Lock()
	require.Equal(t, []proto.AgentPermissionMode{proto.AgentPermissionDeny, proto.AgentPermissionDeny}, modes)
	modesMu.Unlock()
	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerRestoresTurnOverridesAfterRejectedAdmission(t *testing.T) {
	type observedRun struct {
		provider string
		model    string
		message  proto.AgentMessage
	}
	var rejectMu sync.Mutex
	rejectNext := true
	runs := make(chan observedRun, 1)
	workingDir := nativeAPIFixtureWithSessionAndTaskSeedsAndFileSearch(t, proto.RunComplete{Text: "streamed response"}, 25*time.Millisecond, true, nil, nil, nil, nil, nil, nil, func(w http.ResponseWriter, _ *http.Request, _ proto.AgentMessage, _ func(proto.Message)) bool {
		rejectMu.Lock()
		defer rejectMu.Unlock()
		if !rejectNext {
			return false
		}
		rejectNext = false
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(proto.Error{Message: "rejected turn"})
		return true
	}, nil, func(_ string, provider, model string, message proto.AgentMessage) {
		runs <- observedRun{provider: provider, model: model, message: message}
	})
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{
		Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir,
		Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true},
	}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{"cwd": workingDir}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	threadID := response["result"].(map[string]any)["thread"].(map[string]any)["id"].(string)
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "thread/started", response["method"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "turn/start", "params": map[string]any{
		"threadId":       threadID,
		"model":          "openai/gpt-4.1",
		"effort":         "medium",
		"approvalPolicy": "never",
		"sandboxPolicy":  map[string]any{"type": "readOnly"},
		"input":          []any{map[string]any{"type": "text", "text": "rejected"}},
	}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(-32000), response["error"].(map[string]any)["code"])
	require.Contains(t, response["error"].(map[string]any)["message"], "rejected turn")

	require.NoError(t, encoder.Encode(map[string]any{"id": 4, "method": "turn/start", "params": map[string]any{
		"threadId": threadID,
		"input":    []any{map[string]any{"type": "text", "text": "accepted"}},
	}}))
	for {
		response = nil
		require.NoError(t, decoder.Decode(&response))
		if response["method"] == "turn/completed" {
			break
		}
	}
	accepted := <-runs
	require.Equal(t, "openai", accepted.provider)
	require.Equal(t, "gpt-5", accepted.model)
	require.Empty(t, accepted.message.AnalysisEffort)
	require.Equal(t, proto.AgentPermissionInteractive, accepted.message.PermissionMode)
	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerOrdersFastNativeOutputAfterTurnStartup(t *testing.T) {
	workingDir := nativeAPIFixtureWithSessionAndTaskSeedsAndFileSearch(t, proto.RunComplete{Text: "early response"}, 25*time.Millisecond, true, nil, nil, nil, nil, nil, nil, func(_ http.ResponseWriter, _ *http.Request, request proto.AgentMessage, emit func(proto.Message)) bool {
		emit(proto.Message{ID: "other-run-message", RunID: "other-run", ItemID: request.AssistantItemID, Role: proto.Assistant, SessionID: request.SessionID, Parts: []proto.ContentPart{proto.TextContent{Text: "wrong response"}}})
		emit(proto.Message{ID: "early-message", RunID: request.MessageRunID, ItemID: request.AssistantItemID, Role: proto.Assistant, SessionID: request.SessionID, Parts: []proto.ContentPart{proto.TextContent{Text: "early response"}}})
		return false
	}, nil)
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{
		Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir,
		Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true},
	}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{"cwd": workingDir}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	threadID := response["result"].(map[string]any)["thread"].(map[string]any)["id"].(string)
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "thread/started", response["method"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "turn/start", "params": map[string]any{"threadId": threadID, "input": []any{map[string]any{"type": "text", "text": "hello"}}}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(3), response["id"])
	for _, method := range []string{"turn/started", "item/started", "item/started", "thread/status/changed", "item/agentMessage/delta"} {
		response = nil
		require.NoError(t, decoder.Decode(&response))
		require.Equal(t, method, response["method"])
	}
	require.Equal(t, "early response", response["params"].(map[string]any)["delta"])
	for response["method"] != "turn/completed" {
		response = nil
		require.NoError(t, decoder.Decode(&response))
	}
	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerFuzzyFileSearch(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	searchRoot := filepath.Join(workingDir, "search-root")
	require.NoError(t, os.MkdirAll(filepath.Join(searchRoot, "needle-dir"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(searchRoot, "src"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(searchRoot, "needle-dir", "value.txt"), []byte("value"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(searchRoot, "src", "needle-file.txt"), []byte("needle"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(searchRoot, "src", "other.txt"), []byte("other"), 0o600))
	resolvedSearchRoot, err := filepath.EvalSymlinks(searchRoot)
	require.NoError(t, err)
	outsideRoot := t.TempDir()
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, decoder.Decode(&response))

	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "fuzzyFileSearch", "params": map[string]any{"query": "needle", "roots": []string{"search-root"}, "cancellationToken": ""}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(2), response["id"])
	files := response["result"].(map[string]any)["files"].([]any)
	require.NotEmpty(t, files)
	paths := make([]string, 0, len(files))
	for index, value := range files {
		file := value.(map[string]any)
		require.Equal(t, resolvedSearchRoot, file["root"])
		require.Contains(t, []any{"file", "directory"}, file["match_type"])
		require.NotEmpty(t, file["file_name"])
		require.GreaterOrEqual(t, file["score"].(float64), float64(0))
		paths = append(paths, file["path"].(string))
		if index > 0 {
			previous := files[index-1].(map[string]any)
			previousScore := previous["score"].(float64)
			currentScore := file["score"].(float64)
			require.True(t, previousScore > currentScore || previousScore == currentScore && previous["path"].(string) < file["path"].(string))
		}
	}
	require.Contains(t, paths, "needle-dir")
	require.Contains(t, paths, "src/needle-file.txt")

	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "fuzzyFileSearch", "params": map[string]any{"query": "", "roots": []string{"search-root"}}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Empty(t, response["result"].(map[string]any)["files"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 4, "method": "fuzzyFileSearch", "params": map[string]any{"query": "needle"}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "Invalid request: fuzzyFileSearch requires query and roots", response["error"].(map[string]any)["message"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 5, "method": "fuzzyFileSearch", "params": map[string]any{"query": "needle", "roots": []string{outsideRoot}}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Contains(t, response["error"].(map[string]any)["message"], "fuzzy file search root is outside the workspace")

	require.NoError(t, encoder.Encode(map[string]any{"id": 6, "method": "fuzzyFileSearch/sessionStart", "params": map[string]any{"sessionId": "session", "roots": []string{"search-root"}}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "fuzzyFileSearch/sessionStart requires experimentalApi capability", response["error"].(map[string]any)["message"])
	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerDrainsFuzzyFileSearchAfterEOF(t *testing.T) {
	searchStarted := make(chan struct{})
	releaseSearch := make(chan struct{})
	workingDir := nativeAPIFixtureWithSessionAndTaskSeedsAndFileSearch(t, proto.RunComplete{}, 25*time.Millisecond, true, nil, nil, func(proto.WorkspaceFileSearchRequest) {
		close(searchStarted)
		<-releaseSearch
	}, nil, nil, nil, nil, nil)
	searchRoot := filepath.Join(workingDir, "search-root")
	require.NoError(t, os.MkdirAll(searchRoot, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(searchRoot, "needle.txt"), []byte("needle"), 0o600))
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "fuzzyFileSearch", "params": map[string]any{"query": "needle", "roots": []string{"search-root"}}}))
	<-searchStarted
	require.NoError(t, inputWriter.Close())
	close(releaseSearch)
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(2), response["id"])
	require.Len(t, response["result"].(map[string]any)["files"], 1)
	require.NoError(t, <-done)
}

func TestCodexAppServerClosesFuzzyFileSearchOwnerAfterDisconnect(t *testing.T) {
	closedOwners := make(chan proto.WorkspaceFileSearchOwnerRequest, 2)
	workingDir := nativeAPIFixtureWithSessionAndTaskSeedsAndFileSearch(t, proto.RunComplete{}, 25*time.Millisecond, true, nil, nil, nil, nil, func(request proto.WorkspaceFileSearchOwnerRequest) {
		closedOwners <- request
	}, nil, nil, nil)
	searchRoot := filepath.Join(workingDir, "search-root")
	require.NoError(t, os.MkdirAll(searchRoot, 0o755))
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}, "capabilities": map[string]any{"experimentalApi": true}}}))
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "fuzzyFileSearch/sessionStart", "params": map[string]any{"sessionId": "session", "roots": []string{"search-root"}}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(2), response["id"])
	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
	select {
	case closed := <-closedOwners:
		require.NotEmpty(t, closed.Owner)
	case <-time.After(time.Second):
		t.Fatal("fuzzy file search owner was not closed")
	}
	select {
	case duplicate := <-closedOwners:
		t.Fatalf("duplicate fuzzy file search owner close: %+v", duplicate)
	default:
	}
}

func TestCodexAppServerFuzzyFileSearchSessions(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	searchRoot := filepath.Join(workingDir, "search-root")
	require.NoError(t, os.MkdirAll(searchRoot, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(searchRoot, "needle.txt"), []byte("needle"), 0o600))
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}, "capabilities": map[string]any{"experimentalApi": true}}}))
	require.NoError(t, decoder.Decode(&response))

	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "fuzzyFileSearch/sessionStart", "params": map[string]any{"sessionId": "", "roots": []string{"search-root"}}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "sessionId must not be empty", response["error"].(map[string]any)["message"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "fuzzyFileSearch/sessionUpdate", "params": map[string]any{"sessionId": "missing", "query": "needle"}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "fuzzy file search session not found: missing", response["error"].(map[string]any)["message"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 4, "method": "fuzzyFileSearch/sessionStart", "params": map[string]any{"sessionId": "session", "roots": []string{"search-root"}}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(4), response["id"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 5, "method": "fuzzyFileSearch/sessionUpdate", "params": map[string]any{"sessionId": "session", "query": "needle"}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(5), response["id"])
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "fuzzyFileSearch/sessionUpdated", response["method"])
	updated := response["params"].(map[string]any)
	require.Equal(t, "session", updated["sessionId"])
	require.Equal(t, "needle", updated["query"])
	require.Equal(t, "needle.txt", updated["files"].([]any)[0].(map[string]any)["path"])
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "fuzzyFileSearch/sessionCompleted", response["method"])
	require.Equal(t, "session", response["params"].(map[string]any)["sessionId"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 6, "method": "fuzzyFileSearch/sessionStop", "params": map[string]any{"sessionId": "session"}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(6), response["id"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 7, "method": "fuzzyFileSearch/sessionStop", "params": map[string]any{"sessionId": "session"}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(7), response["id"])
	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerSuppressesReplacedFuzzyFileSearchSessionResults(t *testing.T) {
	completionAccepted := make(chan struct{})
	releaseCompletion := make(chan struct{})
	var blockFirstCompletion sync.Once
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseCompletion) }) })
	workingDir := nativeAPIFixtureWithSessionAndTaskSeedsAndFileSearch(t, proto.RunComplete{}, 25*time.Millisecond, true, nil, nil, nil, func(proto.WorkspaceFileSearchSessionCompleteRequest) {
		blockFirstCompletion.Do(func() {
			close(completionAccepted)
			<-releaseCompletion
		})
	}, nil, nil, nil, nil)
	searchRoot := filepath.Join(workingDir, "search-root")
	require.NoError(t, os.MkdirAll(searchRoot, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(searchRoot, "old.txt"), []byte("old"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(searchRoot, "new.txt"), []byte("new"), 0o600))
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}, "capabilities": map[string]any{"experimentalApi": true}}}))
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "fuzzyFileSearch/sessionStart", "params": map[string]any{"sessionId": "session", "roots": []string{"search-root"}}}))
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "fuzzyFileSearch/sessionUpdate", "params": map[string]any{"sessionId": "session", "query": "old"}}))
	require.NoError(t, decoder.Decode(&response))
	<-completionAccepted
	require.NoError(t, encoder.Encode(map[string]any{"id": 4, "method": "fuzzyFileSearch/sessionStart", "params": map[string]any{"sessionId": "session", "roots": []string{"search-root"}}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(4), response["id"])
	releaseOnce.Do(func() { close(releaseCompletion) })
	require.NoError(t, encoder.Encode(map[string]any{"id": 5, "method": "fuzzyFileSearch/sessionUpdate", "params": map[string]any{"sessionId": "session", "query": "new"}}))

	gotResponse, gotUpdated, gotCompleted := false, false, false
	for !gotResponse || !gotUpdated || !gotCompleted {
		response = nil
		require.NoError(t, decoder.Decode(&response))
		if response["id"] == float64(5) {
			gotResponse = true
			continue
		}
		switch response["method"] {
		case "fuzzyFileSearch/sessionUpdated":
			params := response["params"].(map[string]any)
			require.Equal(t, "new", params["query"])
			gotUpdated = true
		case "fuzzyFileSearch/sessionCompleted":
			gotCompleted = true
		default:
			t.Fatalf("unexpected fuzzy file search output: %+v", response)
		}
	}
	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerFuzzyFileSearchNotificationOptOut(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	searchRoot := filepath.Join(workingDir, "search-root")
	require.NoError(t, os.MkdirAll(searchRoot, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(searchRoot, "needle.txt"), []byte("needle"), 0o600))
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}, "capabilities": map[string]any{"experimentalApi": true, "optOutNotificationMethods": []string{"fuzzyFileSearch/sessionUpdated", "fuzzyFileSearch/sessionCompleted"}}}}))
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "fuzzyFileSearch/sessionStart", "params": map[string]any{"sessionId": "session", "roots": []string{"search-root"}}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "fuzzyFileSearch/sessionUpdate", "params": map[string]any{"sessionId": "session", "query": "needle"}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(3), response["id"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 4, "method": "fuzzyFileSearch/sessionStop", "params": map[string]any{"sessionId": "session"}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(4), response["id"])
	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerBackgroundTerminalsRequireExperimentalCapability(t *testing.T) {
	workingDir := nativeAPIFixtureWithSessionSeeds(t, proto.RunComplete{}, 25*time.Millisecond, true, []nativeSessionSeed{{id: "thread-1"}})
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, decoder.Decode(&response))
	for index, method := range []string{"thread/backgroundTerminals/clean", "thread/backgroundTerminals/list", "thread/backgroundTerminals/terminate"} {
		require.NoError(t, encoder.Encode(map[string]any{"id": index + 2, "method": method, "params": map[string]any{}}))
		response = nil
		require.NoError(t, decoder.Decode(&response))
		errorValue := response["error"].(map[string]any)
		require.Equal(t, float64(-32600), errorValue["code"])
		require.Equal(t, method+" requires experimentalApi capability", errorValue["message"])
	}
	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerManagesNativeBackgroundTerminals(t *testing.T) {
	seeds := []nativeTaskSeed{
		{view: managedtask.View{ID: "b11111111", Type: managedtask.TypeShell, Command: "sleep 10", WorkingDirectory: "/one", Ownership: managedtask.Ownership{WorkspaceID: "workspace-1", ParentSessionID: "thread-1", OriginToolCallID: "item-a"}, State: managedtask.State{Status: managedtask.StatusRunning}}},
		{view: managedtask.View{ID: "b22222222", Type: managedtask.TypeShell, Command: "tail -f log", WorkingDirectory: "/two", Ownership: managedtask.Ownership{WorkspaceID: "workspace-1", ParentSessionID: "thread-1", OriginToolCallID: "item-b"}, State: managedtask.State{Status: managedtask.StatusPending}}},
		{view: managedtask.View{ID: "b33333333", Type: managedtask.TypeShell, Command: "other", WorkingDirectory: "/other", Ownership: managedtask.Ownership{WorkspaceID: "workspace-1", ParentSessionID: "other", OriginToolCallID: "item-other"}, State: managedtask.State{Status: managedtask.StatusRunning}}},
		{view: managedtask.View{ID: "b44444444", Type: managedtask.TypeShell, Command: "done", WorkingDirectory: "/done", Ownership: managedtask.Ownership{WorkspaceID: "workspace-1", ParentSessionID: "thread-1", OriginToolCallID: "item-done"}, State: managedtask.State{Status: managedtask.StatusCompleted}}},
		{view: managedtask.View{ID: "a11111111", Type: managedtask.TypeAgent, Ownership: managedtask.Ownership{WorkspaceID: "workspace-1", ParentSessionID: "thread-1"}, State: managedtask.State{Status: managedtask.StatusRunning}}},
	}
	workingDir := nativeAPIFixtureWithSessionAndTaskSeeds(t, proto.RunComplete{}, 25*time.Millisecond, true, []nativeSessionSeed{{id: "thread-1"}, {id: "other"}}, seeds)
	threadID := codexUUIDv7(workingDir, &proto.Session{ID: "thread-1", CreatedAt: 1})
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}, "capabilities": map[string]any{"experimentalApi": true}}}))
	require.NoError(t, decoder.Decode(&response))

	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/backgroundTerminals/list", "params": map[string]any{"threadId": threadID, "limit": 0}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	result := response["result"].(map[string]any)
	terminals := result["data"].([]any)
	require.Len(t, terminals, 1)
	first := terminals[0].(map[string]any)
	require.Equal(t, "item-a", first["itemId"])
	require.Equal(t, "1", first["processId"])
	require.Equal(t, "sleep 10", first["command"])
	require.Equal(t, "/one", first["cwd"])
	require.Nil(t, first["osPid"])
	require.Nil(t, first["cpuPercent"])
	require.Nil(t, first["rssKb"])
	require.Equal(t, "1", result["nextCursor"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "thread/backgroundTerminals/list", "params": map[string]any{"threadId": threadID, "cursor": "1"}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	result = response["result"].(map[string]any)
	terminals = result["data"].([]any)
	require.Len(t, terminals, 1)
	second := terminals[0].(map[string]any)
	require.Equal(t, "item-b", second["itemId"])
	require.Equal(t, "2", second["processId"])
	require.Equal(t, "tail -f log", second["command"])
	require.Equal(t, "/two", second["cwd"])
	require.Nil(t, result["nextCursor"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 4, "method": "thread/backgroundTerminals/list", "params": map[string]any{"threadId": threadID, "cursor": "invalid"}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(-32600), response["error"].(map[string]any)["code"])
	require.Contains(t, response["error"].(map[string]any)["message"], "invalid cursor:")

	require.NoError(t, encoder.Encode(map[string]any{"id": 5, "method": "thread/backgroundTerminals/terminate", "params": map[string]any{"threadId": threadID, "processId": "invalid"}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(-32600), response["error"].(map[string]any)["code"])
	require.Contains(t, response["error"].(map[string]any)["message"], "invalid background terminal process id:")

	require.NoError(t, encoder.Encode(map[string]any{"id": 6, "method": "thread/backgroundTerminals/terminate", "params": map[string]any{"threadId": "invalid", "processId": "invalid"}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(-32600), response["error"].(map[string]any)["code"])
	require.Contains(t, response["error"].(map[string]any)["message"], "invalid background terminal process id:")

	for _, terminate := range []struct {
		id        int
		processID string
		expected  bool
	}{
		{id: 7, processID: "999"},
		{id: 8, processID: "1", expected: true},
		{id: 9, processID: "1"},
	} {
		require.NoError(t, encoder.Encode(map[string]any{"id": terminate.id, "method": "thread/backgroundTerminals/terminate", "params": map[string]any{"threadId": threadID, "processId": terminate.processID}}))
		response = nil
		require.NoError(t, decoder.Decode(&response))
		require.Equal(t, terminate.expected, response["result"].(map[string]any)["terminated"])
	}

	require.NoError(t, encoder.Encode(map[string]any{"id": 10, "method": "thread/backgroundTerminals/clean", "params": map[string]any{"threadId": threadID}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Empty(t, response["result"].(map[string]any))

	require.NoError(t, encoder.Encode(map[string]any{"id": 11, "method": "thread/backgroundTerminals/list", "params": map[string]any{"threadId": threadID}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Empty(t, response["result"].(map[string]any)["data"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 12, "method": "thread/loaded/list", "params": map[string]any{}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, []any{threadID}, response["result"].(map[string]any)["data"])

	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexUUIDv7IsStableForOpaqueNativeSessionIDs(t *testing.T) {
	sess := &proto.Session{ID: "48291", CreatedAt: 1_700_000_000}
	first := codexUUIDv7("/workspace", sess)
	second := codexUUIDv7("/workspace", sess)
	require.Equal(t, first, second)
	require.NotEqual(t, sess.ID, first)
	parsed, err := uuid.Parse(first)
	require.NoError(t, err)
	require.Equal(t, uuid.Version(7), parsed.Version())
	require.NotEqual(t, first, codexUUIDv7("/other-workspace", sess))
}

func TestCodexAppServerCompactsLoadedThreadWithImmediateLifecycle(t *testing.T) {
	workingDir := nativeAPIFixtureWithCompletionDelay(t, proto.RunComplete{}, 150*time.Millisecond)
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{"cwd": workingDir}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	threadID := response["result"].(map[string]any)["thread"].(map[string]any)["id"].(string)
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "thread/started", response["method"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "thread/compact/start", "params": map[string]any{"threadId": threadID}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(3), response["id"])
	require.Equal(t, map[string]any{}, response["result"])

	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "turn/started", response["method"])
	startedTurn := response["params"].(map[string]any)["turn"].(map[string]any)
	turnID := startedTurn["id"].(string)
	require.Equal(t, "inProgress", startedTurn["status"])
	require.Empty(t, startedTurn["items"])

	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "item/started", response["method"])
	startedItem := response["params"].(map[string]any)["item"].(map[string]any)
	itemID := startedItem["id"].(string)
	require.Equal(t, "contextCompaction", startedItem["type"])
	require.NotContains(t, startedItem, "status")

	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "thread/status/changed", response["method"])
	require.Equal(t, "active", response["params"].(map[string]any)["status"].(map[string]any)["type"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 4, "method": "thread/compact/start", "params": map[string]any{"threadId": threadID}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(4), response["id"])
	require.Equal(t, float64(-32600), response["error"].(map[string]any)["code"])
	require.Equal(t, "A turn is already running", response["error"].(map[string]any)["message"])

	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "thread/status/changed", response["method"])
	require.Equal(t, "idle", response["params"].(map[string]any)["status"].(map[string]any)["type"])
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "item/completed", response["method"])
	completedItem := response["params"].(map[string]any)["item"].(map[string]any)
	require.Equal(t, itemID, completedItem["id"])
	require.Equal(t, "contextCompaction", completedItem["type"])
	require.NotContains(t, completedItem, "status")
	require.NotContains(t, completedItem, "text")
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "turn/completed", response["method"])
	completedTurn := response["params"].(map[string]any)["turn"].(map[string]any)
	require.Equal(t, turnID, completedTurn["id"])
	require.Equal(t, "completed", completedTurn["status"])
	require.Len(t, completedTurn["items"], 1)
	require.Equal(t, "contextCompaction", completedTurn["items"].([]any)[0].(map[string]any)["type"])

	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerRunsStandaloneShellCommandThroughNativeBackend(t *testing.T) {
	workingDir := nativeAPIFixture(t, "unused")
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{"cwd": workingDir}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	threadID := response["result"].(map[string]any)["thread"].(map[string]any)["id"].(string)
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "thread/started", response["method"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "thread/shellCommand", "params": map[string]any{"threadId": threadID, "command": "printf hi", "timeoutMs": nil}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(3), response["id"])
	require.Equal(t, map[string]any{}, response["result"])

	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "turn/started", response["method"])
	turnID := response["params"].(map[string]any)["turn"].(map[string]any)["id"].(string)
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "item/started", response["method"])
	startedParams := response["params"].(map[string]any)
	require.Equal(t, turnID, startedParams["turnId"])
	startedItem := startedParams["item"].(map[string]any)
	itemID := startedItem["id"].(string)
	require.Equal(t, "commandExecution", startedItem["type"])
	require.Equal(t, "printf hi", startedItem["command"])
	require.Equal(t, workingDir, startedItem["cwd"])
	require.Equal(t, "userShell", startedItem["source"])
	require.Equal(t, "inProgress", startedItem["status"])
	require.Nil(t, startedItem["pluginId"])
	require.Nil(t, startedItem["scriptPath"])
	require.Nil(t, startedItem["processId"])
	require.Nil(t, startedItem["aggregatedOutput"])
	require.Nil(t, startedItem["exitCode"])
	require.Nil(t, startedItem["durationMs"])
	require.Empty(t, startedItem["commandActions"])

	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "thread/status/changed", response["method"])
	require.Equal(t, "active", response["params"].(map[string]any)["status"].(map[string]any)["type"])
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "item/commandExecution/outputDelta", response["method"])
	deltaParams := response["params"].(map[string]any)
	require.Equal(t, threadID, deltaParams["threadId"])
	require.Equal(t, turnID, deltaParams["turnId"])
	require.Equal(t, itemID, deltaParams["itemId"])
	require.Equal(t, "shell:printf hi\n", deltaParams["delta"])
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "item/completed", response["method"])
	completedItem := response["params"].(map[string]any)["item"].(map[string]any)
	require.Equal(t, itemID, completedItem["id"])
	require.Equal(t, "completed", completedItem["status"])
	require.Equal(t, "shell:printf hi\n", completedItem["aggregatedOutput"])
	require.Equal(t, float64(0), completedItem["exitCode"])
	require.NotNil(t, completedItem["durationMs"])
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "thread/status/changed", response["method"])
	require.Equal(t, "idle", response["params"].(map[string]any)["status"].(map[string]any)["type"])
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "turn/completed", response["method"])
	completedTurn := response["params"].(map[string]any)["turn"].(map[string]any)
	require.Equal(t, turnID, completedTurn["id"])
	require.Equal(t, "completed", completedTurn["status"])
	require.Len(t, completedTurn["items"], 1)

	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerAttachesShellCommandToActiveTurn(t *testing.T) {
	workingDir := nativeAPIFixtureWithCompletionDelay(t, proto.RunComplete{}, 100*time.Millisecond)
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{"cwd": workingDir}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	threadID := response["result"].(map[string]any)["thread"].(map[string]any)["id"].(string)
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "thread/started", response["method"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 4, "method": "turn/start", "params": map[string]any{"threadId": threadID, "input": []any{map[string]any{"type": "text", "text": "hello"}}}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	turnID := response["result"].(map[string]any)["turn"].(map[string]any)["id"].(string)
	for range 4 {
		response = nil
		require.NoError(t, decoder.Decode(&response))
	}
	require.Equal(t, "thread/status/changed", response["method"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 5, "method": "thread/shellCommand", "params": map[string]any{"threadId": threadID, "command": "wait"}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(5), response["id"])
	require.Equal(t, map[string]any{}, response["result"])
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "item/started", response["method"])
	startedParams := response["params"].(map[string]any)
	require.Equal(t, turnID, startedParams["turnId"])
	require.Equal(t, "commandExecution", startedParams["item"].(map[string]any)["type"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 6, "method": "thread/shellCommand", "params": map[string]any{"threadId": threadID, "command": "echo duplicate"}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(6), response["id"])
	require.Equal(t, "A shell command is already running for thread", response["error"].(map[string]any)["message"])
	shellCompleted := false
	turnCompleted := false
	turnStartedCount := 0
	for !turnCompleted {
		response = nil
		require.NoError(t, decoder.Decode(&response))
		switch response["method"] {
		case "turn/started":
			turnStartedCount++
		case "item/completed":
			item := response["params"].(map[string]any)["item"].(map[string]any)
			if item["type"] == "commandExecution" {
				require.Equal(t, turnID, response["params"].(map[string]any)["turnId"])
				require.Equal(t, "completed", item["status"])
				shellCompleted = true
			}
		case "turn/completed":
			require.True(t, shellCompleted)
			require.Equal(t, turnID, response["params"].(map[string]any)["turn"].(map[string]any)["id"])
			turnCompleted = true
		}
	}
	require.Zero(t, turnStartedCount)

	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerRunsTurnsConcurrentlyPerThread(t *testing.T) {
	workingDir := nativeAPIFixtureWithCompletionDelay(t, proto.RunComplete{Text: "completed"}, time.Second)
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	read := func() map[string]any {
		var response map[string]any
		require.NoError(t, decoder.Decode(&response))
		return response
	}
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NotNil(t, read()["result"])
	startThread := func(id int) string {
		require.NoError(t, encoder.Encode(map[string]any{"id": id, "method": "thread/start", "params": map[string]any{"cwd": workingDir}}))
		response := read()
		require.Nil(t, response["error"])
		threadID := response["result"].(map[string]any)["thread"].(map[string]any)["id"].(string)
		require.Equal(t, "thread/started", read()["method"])
		return threadID
	}
	firstThreadID := startThread(2)
	secondThreadID := startThread(3)
	startTurn := func(id int, threadID string) string {
		require.NoError(t, encoder.Encode(map[string]any{"id": id, "method": "turn/start", "params": map[string]any{"threadId": threadID, "input": []any{map[string]any{"type": "text", "text": "hello"}}}}))
		response := read()
		require.Nil(t, response["error"])
		turnID := response["result"].(map[string]any)["turn"].(map[string]any)["id"].(string)
		for range 4 {
			response = read()
			require.Equal(t, threadID, response["params"].(map[string]any)["threadId"])
		}
		return turnID
	}
	firstTurnID := startTurn(4, firstThreadID)
	require.NoError(t, encoder.Encode(map[string]any{"id": 5, "method": "turn/start", "params": map[string]any{"threadId": firstThreadID, "input": []any{map[string]any{"type": "text", "text": "duplicate"}}}}))
	duplicate := read()
	require.Equal(t, float64(5), duplicate["id"])
	require.Equal(t, "A turn is already running", duplicate["error"].(map[string]any)["message"])
	secondTurnID := startTurn(6, secondThreadID)

	completed := make(map[string]string)
	for len(completed) != 2 {
		response := read()
		if response["method"] != "turn/completed" {
			continue
		}
		params := response["params"].(map[string]any)
		threadID := params["threadId"].(string)
		turn := params["turn"].(map[string]any)
		require.Equal(t, "completed", turn["status"])
		completed[threadID] = turn["id"].(string)
	}
	require.Equal(t, firstTurnID, completed[firstThreadID])
	require.Equal(t, secondTurnID, completed[secondThreadID])
	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerShellCommandValidatesTimeoutAndLoadsColdThread(t *testing.T) {
	workingDir := nativeAPIFixtureWithSessionSeeds(t, proto.RunComplete{}, 100*time.Millisecond, true, []nativeSessionSeed{{id: "cold"}})
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, decoder.Decode(&response))
	unknownThreadID := uuid.Must(uuid.NewV7()).String()
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/shellCommand", "params": map[string]any{"threadId": unknownThreadID, "command": "echo hi", "timeoutMs": -1}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(-32602), response["error"].(map[string]any)["code"])
	require.Equal(t, "thread/shellCommand timeoutMs must be non-negative, got -1", response["error"].(map[string]any)["message"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "thread/shellCommand", "params": map[string]any{"threadId": unknownThreadID, "command": "  "}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "command must not be empty", response["error"].(map[string]any)["message"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 4, "method": "thread/list"}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	threadID := response["result"].(map[string]any)["data"].([]any)[0].(map[string]any)["id"].(string)
	require.NoError(t, encoder.Encode(map[string]any{"id": 5, "method": "thread/shellCommand", "params": map[string]any{"threadId": threadID, "command": "wait", "timeoutMs": 0}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(5), response["id"])

	var timedOutItem map[string]any
	turnCompleted := false
	for !turnCompleted {
		response = nil
		require.NoError(t, decoder.Decode(&response))
		if response["method"] == "item/completed" {
			item := response["params"].(map[string]any)["item"].(map[string]any)
			if item["type"] == "commandExecution" {
				timedOutItem = item
			}
		}
		turnCompleted = response["method"] == "turn/completed"
	}
	require.Equal(t, "failed", timedOutItem["status"])
	require.Equal(t, float64(-1), timedOutItem["exitCode"])
	require.Contains(t, timedOutItem["aggregatedOutput"], "Timeout")

	require.NoError(t, encoder.Encode(map[string]any{"id": 6, "method": "thread/loaded/list"}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, []any{threadID}, response["result"].(map[string]any)["data"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 7, "method": "thread/shellCommand", "params": map[string]any{"threadId": threadID, "command": "exit 7"}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(7), response["id"])
	var failedItem map[string]any
	turnCompleted = false
	for !turnCompleted {
		response = nil
		require.NoError(t, decoder.Decode(&response))
		if response["method"] == "item/completed" {
			item := response["params"].(map[string]any)["item"].(map[string]any)
			if item["type"] == "commandExecution" {
				failedItem = item
			}
		}
		if response["method"] == "turn/completed" {
			require.Equal(t, "completed", response["params"].(map[string]any)["turn"].(map[string]any)["status"])
			turnCompleted = true
		}
	}
	require.Equal(t, "failed", failedItem["status"])
	require.Equal(t, float64(7), failedItem["exitCode"])

	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerCompactionRejectsInvalidAndColdThreadsWithoutLoading(t *testing.T) {
	workingDir := nativeAPIFixtureWithSessionSeeds(t, proto.RunComplete{}, 25*time.Millisecond, true, []nativeSessionSeed{{id: "cold"}})
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, decoder.Decode(&response))

	for id, params := range []any{map[string]any{}, map[string]any{"threadId": 1}, map[string]any{"threadId": "invalid"}} {
		require.NoError(t, encoder.Encode(map[string]any{"id": id + 2, "method": "thread/compact/start", "params": params}))
		response = nil
		require.NoError(t, decoder.Decode(&response))
		require.Equal(t, float64(-32600), response["error"].(map[string]any)["code"])
	}

	require.NoError(t, encoder.Encode(map[string]any{"id": 5, "method": "thread/list"}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	threads := response["result"].(map[string]any)["data"].([]any)
	require.Len(t, threads, 1)
	threadID := threads[0].(map[string]any)["id"].(string)
	require.Equal(t, "notLoaded", threads[0].(map[string]any)["status"].(map[string]any)["type"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 6, "method": "thread/compact/start", "params": map[string]any{"threadId": threadID}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(-32002), response["error"].(map[string]any)["code"])
	require.Equal(t, "Thread not found", response["error"].(map[string]any)["message"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 7, "method": "thread/loaded/list"}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Empty(t, response["result"].(map[string]any)["data"])

	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerCompactionProjectsNativeFailure(t *testing.T) {
	workingDir := nativeAPIFixtureWithSessionSeeds(t, proto.RunComplete{}, 25*time.Millisecond, true, []nativeSessionSeed{{id: "active", active: true}})
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/list"}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	threadID := response["result"].(map[string]any)["data"].([]any)[0].(map[string]any)["id"].(string)
	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "thread/resume", "params": map[string]any{"threadId": threadID}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(3), response["id"])
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "thread/status/changed", response["method"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 4, "method": "thread/compact/start", "params": map[string]any{"threadId": threadID}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(4), response["id"])
	require.Equal(t, map[string]any{}, response["result"])

	itemCompleted, turnStatus, turnError := false, "", ""
	for turnStatus == "" {
		response = nil
		require.NoError(t, decoder.Decode(&response))
		if response["method"] == "item/completed" {
			itemCompleted = true
			item := response["params"].(map[string]any)["item"].(map[string]any)
			require.Equal(t, "contextCompaction", item["type"])
			require.NotContains(t, item, "status")
		}
		if response["method"] == "turn/completed" {
			turn := response["params"].(map[string]any)["turn"].(map[string]any)
			turnStatus = turn["status"].(string)
			turnError = turn["error"].(map[string]any)["message"].(string)
		}
	}
	require.True(t, itemCompleted)
	require.Equal(t, "failed", turnStatus)
	require.Contains(t, turnError, "already has an active writer")

	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerInterruptsActiveTurn(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	executable := "unused"
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation(executable, workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder := json.NewEncoder(inputWriter)
	decoder := json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"method": "initialized", "params": map[string]any{}}))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{"cwd": workingDir}}))
	require.NoError(t, decoder.Decode(&response))
	threadID := response["result"].(map[string]any)["thread"].(map[string]any)["id"].(string)
	parsedThreadID, err := uuid.Parse(threadID)
	require.NoError(t, err)
	require.Equal(t, uuid.Version(7), parsedThreadID.Version())
	require.NotEqual(t, "thread-1", threadID)
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "turn/start", "params": map[string]any{"threadId": threadID, "input": []any{map[string]any{"type": "text", "text": "hello"}}}}))
	require.NoError(t, decoder.Decode(&response))
	turnID := response["result"].(map[string]any)["turn"].(map[string]any)["id"].(string)
	interruptSent := make(chan error, 1)
	go func() {
		interruptSent <- encoder.Encode(map[string]any{"id": 4, "method": "turn/interrupt", "params": map[string]any{"threadId": threadID, "turnId": turnID}})
	}()
	parsedTurnID, err := uuid.Parse(turnID)
	require.NoError(t, err)
	require.Equal(t, uuid.Version(7), parsedTurnID.Version())
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, <-interruptSent)
	status := ""
	itemStatus := ""
	interruptResponded := false
	for status == "" {
		response = nil
		require.NoError(t, decoder.Decode(&response))
		if response["id"] == float64(4) {
			interruptResponded = true
			require.Equal(t, map[string]any{}, response["result"])
		}
		if response["method"] == "item/completed" {
			itemStatus = response["params"].(map[string]any)["item"].(map[string]any)["status"].(string)
		}
		if response["method"] == "turn/completed" {
			require.True(t, interruptResponded)
			status = response["params"].(map[string]any)["turn"].(map[string]any)["status"].(string)
		}
	}
	require.True(t, interruptResponded)
	require.Equal(t, "interrupted", itemStatus)
	require.Equal(t, "interrupted", status)
	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerSteersActiveNativeTurn(t *testing.T) {
	requests := make(chan proto.AgentMessage, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseTurn := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseTurn)
	workingDir := nativeAPIFixture(t, "streamed response", func(_, _, _ string, request proto.AgentMessage) {
		requests <- request
		if request.DeliveryMode != string(agent.DeliverySteer) {
			<-release
		}
	})
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{"cwd": workingDir}}))
	require.NoError(t, decoder.Decode(&response))
	threadID := response["result"].(map[string]any)["thread"].(map[string]any)["id"].(string)
	require.NoError(t, decoder.Decode(&response))
	skillID := filepath.Join(workingDir, ".agents", "skills", "fixture", "SKILL.md")
	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "turn/start", "params": map[string]any{"threadId": threadID, "clientUserMessageId": "desktop-start", "approvalPolicy": "never", "sandboxPolicy": map[string]any{"type": "readOnly"}, "input": []any{map[string]any{"type": "text", "text": "look", "textElements": []any{map[string]any{"byteRange": map[string]any{"start": 0, "end": 4}, "placeholder": "<note>"}}}, map[string]any{"type": "localImage", "path": "initial.png"}, map[string]any{"type": "skill", "name": "fixture", "path": skillID}}}}))
	require.NoError(t, decoder.Decode(&response))
	turnID := response["result"].(map[string]any)["turn"].(map[string]any)["id"].(string)
	steerSent := make(chan error, 1)
	go func() {
		steerSent <- encoder.Encode(map[string]any{"id": 6, "method": "turn/steer", "params": map[string]any{"threadId": threadID, "expectedTurnId": turnID, "clientUserMessageId": "desktop-steer", "input": []any{map[string]any{"type": "text", "text": "new direction"}, map[string]any{"type": "localImage", "path": "steer.png"}, map[string]any{"type": "skill", "name": "fixture", "path": skillID}}}})
	}()
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "turn/started", response["method"])
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "item/started", response["method"])
	startedUser := response["params"].(map[string]any)["item"].(map[string]any)
	require.Equal(t, "userMessage", startedUser["type"])
	require.Equal(t, "desktop-start", startedUser["clientId"])
	require.NotContains(t, startedUser, "status")
	startedContent := startedUser["content"].([]any)
	require.Equal(t, "text", startedContent[0].(map[string]any)["type"])
	require.Len(t, startedContent[0].(map[string]any)["textElements"], 1)
	require.Equal(t, "localImage", startedContent[1].(map[string]any)["type"])
	require.Equal(t, map[string]any{"type": "skill", "name": "fixture", "path": skillID}, startedContent[2])
	require.NoError(t, decoder.Decode(&response))
	startedAssistant := response["params"].(map[string]any)["item"].(map[string]any)
	require.Equal(t, "agentMessage", startedAssistant["type"])
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "thread/status/changed", response["method"])
	require.Equal(t, "active", response["params"].(map[string]any)["status"].(map[string]any)["type"])
	initial := <-requests
	require.Equal(t, string(agent.DeliveryQueue), initial.DeliveryMode)
	require.False(t, initial.RequireActiveRun)
	require.True(t, initial.WaitForAdmission)
	require.Equal(t, turnID, initial.UserMessageID)
	require.Equal(t, turnID, startedUser["id"])
	require.Equal(t, initial.RunID, initial.MessageRunID)
	require.Equal(t, initial.AssistantItemID, startedAssistant["id"])
	require.Equal(t, "desktop-start", initial.SubmissionID)
	require.Equal(t, proto.AgentPermissionDeny, initial.PermissionMode)
	require.Empty(t, initial.Prompt)
	require.Empty(t, initial.Attachments)
	require.Equal(t, []proto.AgentInputPart{{Type: "text", Text: "look", TextElements: []any{map[string]any{"byteRange": map[string]any{"start": float64(0), "end": float64(4)}, "placeholder": "<note>"}}}, {Type: "local_file", Path: "initial.png"}, {Type: "skill", Text: "fixture", Path: skillID}}, initial.InputParts)

	require.NoError(t, <-steerSent)
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, turnID, response["result"].(map[string]any)["turnId"])
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "item/started", response["method"])
	steeredUser := response["params"].(map[string]any)["item"].(map[string]any)
	require.Equal(t, "userMessage", steeredUser["type"])
	require.Equal(t, "desktop-steer", steeredUser["clientId"])
	require.Equal(t, []any{}, steeredUser["content"].([]any)[0].(map[string]any)["textElements"])
	require.Equal(t, map[string]any{"type": "skill", "name": "fixture", "path": skillID}, steeredUser["content"].([]any)[2])
	steered := <-requests
	require.Equal(t, string(agent.DeliverySteer), steered.DeliveryMode)
	require.True(t, steered.RequireActiveRun)
	require.True(t, steered.WaitForAdmission)
	require.Empty(t, steered.RunID)
	require.Equal(t, initial.RunID, steered.MessageRunID)
	require.Equal(t, initial.AssistantItemID, steered.AssistantItemID)
	require.Equal(t, steeredUser["id"], steered.UserMessageID)
	require.Equal(t, "desktop-steer", steered.SubmissionID)
	require.Equal(t, proto.AgentPermissionDeny, steered.PermissionMode)
	require.Empty(t, steered.Prompt)
	require.Empty(t, steered.Attachments)
	require.Equal(t, []proto.AgentInputPart{{Type: "text", Text: "new direction"}, {Type: "local_file", Path: "steer.png"}, {Type: "skill", Text: "fixture", Path: skillID}}, steered.InputParts)

	require.NoError(t, encoder.Encode(map[string]any{"id": 4, "method": "turn/steer", "params": map[string]any{"threadId": threadID, "expectedTurnId": "wrong", "input": []any{map[string]any{"type": "text", "text": "ignored"}}}}))
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(-32600), response["error"].(map[string]any)["code"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 5, "method": "turn/steer", "params": map[string]any{"threadId": threadID, "expectedTurnId": turnID}}))
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(-32600), response["error"].(map[string]any)["code"])
	require.Equal(t, "input must be an array", response["error"].(map[string]any)["message"])
	select {
	case unexpected := <-requests:
		t.Fatalf("malformed steer reached native API: %+v", unexpected)
	default:
	}

	releaseTurn()

	status := ""
	for status == "" {
		require.NoError(t, decoder.Decode(&response))
		if response["method"] == "turn/completed" {
			completedTurn := response["params"].(map[string]any)["turn"].(map[string]any)
			status = completedTurn["status"].(string)
			items := completedTurn["items"].([]any)
			require.Equal(t, []string{"userMessage", "userMessage", "agentMessage"}, []string{
				items[0].(map[string]any)["type"].(string),
				items[1].(map[string]any)["type"].(string),
				items[2].(map[string]any)["type"].(string),
			})
		}
	}
	require.Equal(t, "completed", status)
	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerAcceptsExplicitEmptyTurnInput(t *testing.T) {
	requests := make(chan proto.AgentMessage, 1)
	workingDir := nativeAPIFixture(t, "streamed response", func(_, _, _ string, request proto.AgentMessage) {
		requests <- request
	})
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{"cwd": workingDir}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	threadID := response["result"].(map[string]any)["thread"].(map[string]any)["id"].(string)
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "thread/started", response["method"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "turn/start", "params": map[string]any{"threadId": threadID, "clientUserMessageId": "desktop-empty", "input": []any{}}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(3), response["id"])
	require.NotEmpty(t, response["result"].(map[string]any)["turn"].(map[string]any)["id"])

	var startedUser map[string]any
	var completedTurn map[string]any
	for completedTurn == nil {
		response = nil
		require.NoError(t, decoder.Decode(&response))
		if response["method"] == "item/started" {
			item := response["params"].(map[string]any)["item"].(map[string]any)
			if item["type"] == "userMessage" {
				startedUser = item
			}
		}
		if response["method"] == "turn/completed" {
			completedTurn = response["params"].(map[string]any)["turn"].(map[string]any)
		}
	}
	nativeRequest := <-requests
	require.NotNil(t, nativeRequest.InputParts)
	require.Empty(t, nativeRequest.InputParts)
	require.Equal(t, "desktop-empty", startedUser["clientId"])
	require.Empty(t, startedUser["content"])
	require.Equal(t, "completed", completedTurn["status"])
	items := completedTurn["items"].([]any)
	require.Equal(t, []string{"userMessage", "agentMessage"}, []string{
		items[0].(map[string]any)["type"].(string),
		items[1].(map[string]any)["type"].(string),
	})
	require.Empty(t, items[0].(map[string]any)["content"])
	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerRejectsMalformedTurnInput(t *testing.T) {
	requests := make(chan proto.AgentMessage, 1)
	workingDir := nativeAPIFixture(t, "streamed response", func(_, _, _ string, request proto.AgentMessage) {
		requests <- request
	})
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{"cwd": workingDir}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	threadID := response["result"].(map[string]any)["thread"].(map[string]any)["id"].(string)
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "thread/started", response["method"])

	tests := []struct {
		params  map[string]any
		message string
	}{
		{params: map[string]any{"threadId": threadID}, message: "input must be an array"},
		{params: map[string]any{"threadId": threadID, "input": nil}, message: "input must be an array"},
		{params: map[string]any{"threadId": threadID, "input": []any{map[string]any{"type": "text"}}}, message: "input[0].text is required"},
	}
	for index, test := range tests {
		requestID := 3 + index
		require.NoError(t, encoder.Encode(map[string]any{"id": requestID, "method": "turn/start", "params": test.params}))
		response = nil
		require.NoError(t, decoder.Decode(&response))
		require.Equal(t, float64(requestID), response["id"])
		require.Equal(t, float64(-32602), response["error"].(map[string]any)["code"])
		require.Equal(t, test.message, response["error"].(map[string]any)["message"])
		select {
		case unexpected := <-requests:
			t.Fatalf("malformed turn reached native API: %+v", unexpected)
		default:
		}
	}

	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerSetsNativeThreadNameBeforeNotifying(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{"cwd": workingDir}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	threadID := response["result"].(map[string]any)["thread"].(map[string]any)["id"].(string)
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "thread/started", response["method"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "thread/name/set", "params": map[string]any{"threadId": threadID, "name": "  Renamed thread  "}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(3), response["id"])
	require.Equal(t, map[string]any{}, response["result"])
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "thread/name/updated", response["method"])
	require.Equal(t, threadID, response["params"].(map[string]any)["threadId"])
	require.Equal(t, "Renamed thread", response["params"].(map[string]any)["threadName"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 4, "method": "thread/read", "params": map[string]any{"threadId": threadID}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(4), response["id"])
	require.Equal(t, "Renamed thread", response["result"].(map[string]any)["thread"].(map[string]any)["name"])

	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerSetThreadNameValidatesParamsAndMissingThread(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, decoder.Decode(&response))

	missingID := uuid.NewString()
	tests := []struct {
		params  any
		code    float64
		message string
	}{
		{params: map[string]any{}, code: -32600, message: "Invalid request: threadId is required"},
		{params: map[string]any{"threadId": 1, "name": "name"}, code: -32600, message: "Invalid request:"},
		{params: map[string]any{"threadId": "invalid", "name": "name"}, code: -32600, message: "Invalid request: invalid thread id:"},
		{params: map[string]any{"threadId": missingID}, code: -32600, message: "thread name must not be empty"},
		{params: map[string]any{"threadId": missingID, "name": "   "}, code: -32600, message: "thread name must not be empty"},
		{params: map[string]any{"threadId": missingID, "name": "name"}, code: -32002, message: "Thread not found"},
	}
	for index, test := range tests {
		require.NoError(t, encoder.Encode(map[string]any{"id": index + 2, "method": "thread/name/set", "params": test.params}))
		response = nil
		require.NoError(t, decoder.Decode(&response))
		require.Equal(t, test.code, response["error"].(map[string]any)["code"])
		require.Contains(t, response["error"].(map[string]any)["message"], test.message)
	}

	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerSetThreadNameNotificationOptOutPreservesEffect(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}, "capabilities": map[string]any{"optOutNotificationMethods": []string{"thread/name/updated"}}}}))
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{"cwd": workingDir}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	threadID := response["result"].(map[string]any)["thread"].(map[string]any)["id"].(string)
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "thread/started", response["method"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "thread/name/set", "params": map[string]any{"threadId": threadID, "name": "Renamed thread"}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(3), response["id"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 4, "method": "thread/read", "params": map[string]any{"threadId": threadID}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(4), response["id"])
	require.Equal(t, "Renamed thread", response["result"].(map[string]any)["thread"].(map[string]any)["name"])

	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerDeletesNativeThread(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{"cwd": workingDir}}))
	require.NoError(t, decoder.Decode(&response))
	threadID := response["result"].(map[string]any)["thread"].(map[string]any)["id"].(string)
	require.NoError(t, decoder.Decode(&response))

	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "thread/loaded/list"}))
	require.NoError(t, decoder.Decode(&response))
	loaded := response["result"].(map[string]any)
	require.Equal(t, []any{threadID}, loaded["data"])
	require.Nil(t, loaded["nextCursor"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 4, "method": "thread/loaded/list", "params": map[string]any{"cursor": "not-a-thread-id"}}))
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(-32600), response["error"].(map[string]any)["code"])
	require.Equal(t, "Invalid request: invalid cursor: not-a-thread-id", response["error"].(map[string]any)["message"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 5, "method": "thread/delete", "params": map[string]any{"threadId": threadID}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(5), response["id"])
	require.Equal(t, map[string]any{}, response["result"])
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "thread/deleted", response["method"])
	require.Equal(t, threadID, response["params"].(map[string]any)["threadId"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 6, "method": "thread/loaded/list"}))
	require.NoError(t, decoder.Decode(&response))
	loaded = response["result"].(map[string]any)
	require.Empty(t, loaded["data"])
	require.Nil(t, loaded["nextCursor"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 7, "method": "thread/read", "params": map[string]any{"threadId": threadID}}))
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(-32600), response["error"].(map[string]any)["code"])
	require.Contains(t, response["error"].(map[string]any)["message"], "thread not loaded")

	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerDeleteValidatesParamsAndMissingThread(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, decoder.Decode(&response))

	missingID := uuid.NewString()
	tests := []struct {
		params  any
		message string
	}{
		{params: map[string]any{}, message: "Invalid request: threadId is required"},
		{params: map[string]any{"threadId": 1}, message: "Invalid request:"},
		{params: map[string]any{"threadId": "invalid"}, message: "Invalid request: invalid thread id:"},
		{params: map[string]any{"threadId": missingID}, message: "thread not found: " + missingID},
	}
	for index, test := range tests {
		require.NoError(t, encoder.Encode(map[string]any{"id": index + 2, "method": "thread/delete", "params": test.params}))
		response = nil
		require.NoError(t, decoder.Decode(&response))
		require.Equal(t, float64(-32600), response["error"].(map[string]any)["code"])
		require.Contains(t, response["error"].(map[string]any)["message"], test.message)
	}

	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerDeleteRejectsEphemeralThread(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{"cwd": workingDir, "ephemeral": true}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	threadID := response["result"].(map[string]any)["thread"].(map[string]any)["id"].(string)
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "thread/started", response["method"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "thread/delete", "params": map[string]any{"threadId": threadID}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(-32600), response["error"].(map[string]any)["code"])
	require.Equal(t, "thread is not persisted and cannot be deleted: "+threadID, response["error"].(map[string]any)["message"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 4, "method": "thread/loaded/list"}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, []any{threadID}, response["result"].(map[string]any)["data"])

	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerCancellationDeletesActiveEphemeralThread(t *testing.T) {
	runStarted := make(chan struct{}, 1)
	workingDir := nativeAPIFixtureWithCompletionDelay(t, proto.RunComplete{Text: "unused"}, 250*time.Millisecond, func(_, _, _ string, _ proto.AgentMessage) {
		runStarted <- struct{}{}
	})
	inputReader, inputWriter := io.Pipe()
	var output bytes.Buffer
	ctx, cancel := context.WithCancel(t.Context())
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(ctx, protocolInvocation("unused", workingDir, inputReader, &output), request)
	}()

	threadID := codexUUIDv7(workingDir, &proto.Session{ID: "thread-1", CreatedAt: 1})
	encoder := json.NewEncoder(inputWriter)
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{"cwd": workingDir, "ephemeral": true}}))
	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "turn/start", "params": map[string]any{"threadId": threadID, "input": []any{map[string]any{"type": "text", "text": "hello"}}}}))
	select {
	case <-runStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("native turn did not start")
	}
	cancel()
	require.NoError(t, inputWriter.Close())
	select {
	case err := <-done:
		if err != nil {
			require.ErrorIs(t, err, context.Canceled)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("app server did not finish cancellation cleanup")
	}

	nativeClient, err := codexClientFactory(workingDir)
	require.NoError(t, err)
	_, err = nativeClient.GetSession(t.Context(), "workspace-1", "thread-1")
	require.ErrorIs(t, err, client.ErrNotFound)
}

func TestCodexAppServerJoinsActiveTurnWhenResponseWriteFails(t *testing.T) {
	runStarted := make(chan struct{}, 1)
	workingDir := nativeAPIFixtureWithCompletionDelay(t, proto.RunComplete{Text: "unused"}, 5*time.Second, func(_, _, _ string, _ proto.AgentMessage) {
		runStarted <- struct{}{}
	})
	inputReader, inputWriter := io.Pipe()
	writeErr := errors.New("turn result write failed")
	output := &failAtWriter{writer: io.Discard, failAt: 4, err: writeErr}
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, output), request)
	}()
	defer inputWriter.Close()

	threadID := codexUUIDv7(workingDir, &proto.Session{ID: "thread-1", CreatedAt: 1})
	encoder := json.NewEncoder(inputWriter)
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{"cwd": workingDir, "ephemeral": true}}))
	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "turn/start", "params": map[string]any{"threadId": threadID, "input": []any{map[string]any{"type": "text", "text": "hello"}}}}))
	select {
	case <-runStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("native turn did not start")
	}
	select {
	case err := <-done:
		require.ErrorContains(t, err, writeErr.Error())
	case <-time.After(2 * time.Second):
		t.Fatal("app server did not cancel and join the active turn")
	}

	nativeClient, err := codexClientFactory(workingDir)
	require.NoError(t, err)
	_, err = nativeClient.GetSession(t.Context(), "workspace-1", "thread-1")
	require.ErrorIs(t, err, client.ErrNotFound)
}

func TestCodexAppServerCancelsNativeTurnWhenDeltaWriteFails(t *testing.T) {
	cancelled := make(chan string, 1)
	workingDir := nativeAPIFixtureWithSessionAndTaskSeedsAndFileSearch(t, proto.RunComplete{Text: "unused"}, 5*time.Second, true, nil, nil, nil, nil, nil, nil, func(_ http.ResponseWriter, _ *http.Request, request proto.AgentMessage, emit func(proto.Message)) bool {
		emit(proto.Message{
			ID:        "message-1",
			RunID:     request.MessageRunID,
			ItemID:    request.AssistantItemID,
			SessionID: request.SessionID,
			Role:      proto.Assistant,
			Parts:     []proto.ContentPart{proto.TextContent{Text: "chunk"}},
		})
		return false
	}, func(sessionID string) {
		cancelled <- sessionID
	})
	inputReader, inputWriter := io.Pipe()
	writeErr := errors.New("delta write failed")
	output := &failAtWriter{writer: io.Discard, failAt: 9, err: writeErr}
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, output), request)
	}()
	defer inputWriter.Close()

	threadID := codexUUIDv7(workingDir, &proto.Session{ID: "thread-1", CreatedAt: 1})
	encoder := json.NewEncoder(inputWriter)
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{"cwd": workingDir}}))
	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "turn/start", "params": map[string]any{"threadId": threadID, "input": []any{map[string]any{"type": "text", "text": "hello"}}}}))
	select {
	case sessionID := <-cancelled:
		require.Equal(t, "thread-1", sessionID)
	case <-time.After(2 * time.Second):
		t.Fatal("native turn was not cancelled after delta write failure")
	}
	require.NoError(t, inputWriter.Close())
	select {
	case err := <-done:
		require.ErrorContains(t, err, writeErr.Error())
	case <-time.After(2 * time.Second):
		t.Fatal("app server did not finish delta write failure cleanup")
	}
}

func TestCodexAppServerDeletesPersistentThreadWhenStartResultWriteFails(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	inputReader, inputWriter := io.Pipe()
	writeErr := errors.New("thread start result write failed")
	output := &failAtWriter{writer: io.Discard, failAt: 2, err: writeErr}
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, output), request)
	}()
	defer inputWriter.Close()

	encoder := json.NewEncoder(inputWriter)
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{"cwd": workingDir, "model": "claude-sonnet", "modelProvider": "crux"}}))
	select {
	case err := <-done:
		require.ErrorContains(t, err, writeErr.Error())
	case <-time.After(2 * time.Second):
		t.Fatal("app server did not finish failed thread start cleanup")
	}

	nativeClient, err := codexClientFactory(workingDir)
	require.NoError(t, err)
	_, err = nativeClient.GetSession(t.Context(), "workspace-1", "thread-1")
	require.ErrorIs(t, err, client.ErrNotFound)
	workspace, err := nativeClient.GetWorkspace(t.Context(), "workspace-1")
	require.NoError(t, err)
	require.Equal(t, config.SelectedModel{Provider: "openai", Model: "gpt-5"}, workspace.Config.Models[config.SelectedModelTypeLarge])
}

func TestCodexAppServerDeletesForkWhenResponseWriteFailsAfterCancellation(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	writeErr := errors.New("fork result write failed")
	output := &failAtWriter{writer: outputWriter, failAt: 6, err: writeErr, onFailure: cancel}
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(ctx, protocolInvocation("unused", workingDir, inputReader, output), request)
		_ = outputWriter.Close()
	}()
	defer inputWriter.Close()

	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{"cwd": workingDir, "model": "claude-sonnet", "modelProvider": "crux"}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	threadID := response["result"].(map[string]any)["thread"].(map[string]any)["id"].(string)
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "thread/started", response["method"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "thread/start", "params": map[string]any{"cwd": workingDir, "model": "gpt-5", "modelProvider": "crux"}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(3), response["id"])
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "thread/started", response["method"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 4, "method": "thread/fork", "params": map[string]any{"threadId": threadID}}))

	select {
	case err := <-done:
		require.ErrorContains(t, err, writeErr.Error())
	case <-time.After(5 * time.Second):
		t.Fatal("app server did not finish failed fork cleanup")
	}

	nativeClient, err := codexClientFactory(workingDir)
	require.NoError(t, err)
	_, err = nativeClient.GetSession(t.Context(), "workspace-1", "fork-thread-1")
	require.ErrorIs(t, err, client.ErrNotFound)
	workspace, err := nativeClient.GetWorkspace(t.Context(), "workspace-1")
	require.NoError(t, err)
	require.Equal(t, config.SelectedModel{Provider: "openai", Model: "gpt-5"}, workspace.Config.Models[config.SelectedModelTypeLarge])
}

func TestCodexAppServerRejectsProviderOnlyModelMismatchWithoutCreatingThread(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()

	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{"cwd": workingDir, "modelProvider": "anthropic"}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(-32602), response["error"].(map[string]any)["code"])
	require.Equal(t, `Desktop model provider "anthropic" is unavailable; the public provider is crux`, response["error"].(map[string]any)["message"])
	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)

	nativeClient, err := codexClientFactory(workingDir)
	require.NoError(t, err)
	sessions, err := nativeClient.ListSessions(t.Context(), "workspace-1")
	require.NoError(t, err)
	require.Empty(t, sessions)
}

func TestCodexAppServerCancelsStandaloneShellCommandOnEOF(t *testing.T) {
	workingDir := nativeAPIFixtureWithCompletionDelay(t, proto.RunComplete{}, time.Hour)
	inputReader, inputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, io.Discard), request)
	}()

	threadID := codexUUIDv7(workingDir, &proto.Session{ID: "thread-1", CreatedAt: 1})
	encoder := json.NewEncoder(inputWriter)
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{"cwd": workingDir}}))
	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "thread/shellCommand", "params": map[string]any{"threadId": threadID, "command": "wait"}}))
	require.NoError(t, inputWriter.Close())
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("app server did not cancel the standalone shell command after EOF")
	}
}

func TestCodexAppServerDoesNotRespondToUnsupportedNotifications(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()

	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, decoder.Decode(&response))
	for _, notification := range []map[string]any{
		{"method": "plugin/list", "params": map[string]any{}},
		{"method": "thread/items/list", "params": map[string]any{}},
	} {
		require.NoError(t, encoder.Encode(notification))
	}
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "model/list", "params": map[string]any{}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(2), response["id"])
	require.NotNil(t, response["result"])
	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerPropagatesUnsupportedResponseWriteFailures(t *testing.T) {
	requests := []struct {
		name   string
		method string
		params map[string]any
	}{
		{name: "plugin list", method: "plugin/list", params: map[string]any{}},
		{name: "thread items list", method: "thread/items/list", params: map[string]any{}},
	}
	for _, test := range requests {
		t.Run(test.name, func(t *testing.T) {
			workingDir := codexNativeAPIFixture(t)
			inputReader, inputWriter := io.Pipe()
			writeErr := errors.New("unsupported response write failed")
			output := &failAtWriter{writer: io.Discard, failAt: 2, err: writeErr}
			request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
			done := make(chan error, 1)
			go func() {
				done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, output), request)
			}()
			defer inputWriter.Close()

			encoder := json.NewEncoder(inputWriter)
			require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
			require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": test.method, "params": test.params}))
			select {
			case err := <-done:
				require.ErrorContains(t, err, writeErr.Error())
			case <-time.After(2 * time.Second):
				t.Fatal("app server did not propagate unsupported response write failure")
			}
		})
	}
}

func TestCodexAppServerDeletesColdArchivedSubtreeAndPreservesFork(t *testing.T) {
	seeds := []nativeSessionSeed{
		{id: "root", archived: true},
		{id: "child", parentID: "root", archived: true},
		{id: "grandchild", parentID: "child", archived: true},
		{id: "fork", forkedFromID: "root"},
	}
	workingDir := nativeAPIFixtureWithSessionSeeds(t, proto.RunComplete{Text: "unused"}, 25*time.Millisecond, true, seeds)
	codexID := func(id string) string {
		return codexUUIDv7(workingDir, &proto.Session{ID: id, CreatedAt: 1})
	}
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/delete", "params": map[string]any{"threadId": codexID("root")}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(2), response["id"])
	require.Equal(t, map[string]any{}, response["result"])
	for _, expectedID := range []string{codexID("grandchild"), codexID("child"), codexID("root")} {
		response = nil
		require.NoError(t, decoder.Decode(&response))
		require.Equal(t, "thread/deleted", response["method"])
		require.Equal(t, expectedID, response["params"].(map[string]any)["threadId"])
	}

	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "thread/list", "params": map[string]any{"archived": true}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Empty(t, response["result"].(map[string]any)["data"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 4, "method": "thread/list"}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Contains(t, response, "result", "%#v", response)
	threads := response["result"].(map[string]any)["data"].([]any)
	require.Len(t, threads, 1)
	require.Equal(t, codexID("fork"), threads[0].(map[string]any)["id"])
	require.Nil(t, threads[0].(map[string]any)["forkedFromId"])

	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerDeleteRejectsDescendantActiveWriterWithoutMutation(t *testing.T) {
	seeds := []nativeSessionSeed{{id: "root"}, {id: "child", parentID: "root", active: true}}
	workingDir := nativeAPIFixtureWithSessionSeeds(t, proto.RunComplete{Text: "unused"}, 25*time.Millisecond, true, seeds)
	codexID := func(id string) string {
		return codexUUIDv7(workingDir, &proto.Session{ID: id, CreatedAt: 1})
	}
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/delete", "params": map[string]any{"threadId": codexID("root")}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(-32600), response["error"].(map[string]any)["code"])
	require.Equal(t, "thread "+codexID("child")+" already has an active writer", response["error"].(map[string]any)["message"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "thread/list"}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Len(t, response["result"].(map[string]any)["data"], 2)

	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerDeleteNotificationOptOutPreservesEffect(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}, "capabilities": map[string]any{"optOutNotificationMethods": []string{"thread/deleted"}}}}))
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{"cwd": workingDir}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	threadID := response["result"].(map[string]any)["thread"].(map[string]any)["id"].(string)
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "thread/started", response["method"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "thread/delete", "params": map[string]any{"threadId": threadID}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(3), response["id"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 4, "method": "thread/list"}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(4), response["id"])
	require.Empty(t, response["result"].(map[string]any)["data"])

	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerUnsubscribesAndUnloadsIdleThread(t *testing.T) {
	oldDelay := codexThreadUnloadDelayForBridge
	codexThreadUnloadDelayForBridge = 120 * time.Millisecond
	t.Cleanup(func() { codexThreadUnloadDelayForBridge = oldDelay })

	workingDir := codexNativeAPIFixture(t)
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, decoder.Decode(&response))
	codexThreadUnloadDelayForBridge = oldDelay

	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/unsubscribe", "params": map[string]any{"threadId": "invalid"}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(-32600), response["error"].(map[string]any)["code"])
	require.Contains(t, response["error"].(map[string]any)["message"], "invalid thread id:")
	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "thread/unsubscribe", "params": map[string]any{}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "Invalid request: threadId is required", response["error"].(map[string]any)["message"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 4, "method": "thread/unsubscribe", "params": map[string]any{"threadId": uuid.NewString()}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "notLoaded", response["result"].(map[string]any)["status"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 5, "method": "thread/start", "params": map[string]any{"cwd": workingDir}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	threadID := response["result"].(map[string]any)["thread"].(map[string]any)["id"].(string)
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "thread/started", response["method"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 6, "method": "thread/unsubscribe", "params": map[string]any{"threadId": threadID}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "unsubscribed", response["result"].(map[string]any)["status"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 7, "method": "thread/unsubscribe", "params": map[string]any{"threadId": threadID}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "notSubscribed", response["result"].(map[string]any)["status"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 8, "method": "thread/loaded/list"}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, []any{threadID}, response["result"].(map[string]any)["data"])

	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "thread/status/changed", response["method"])
	require.Equal(t, "notLoaded", response["params"].(map[string]any)["status"].(map[string]any)["type"])
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "thread/closed", response["method"])
	require.Equal(t, threadID, response["params"].(map[string]any)["threadId"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 9, "method": "thread/loaded/list"}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Empty(t, response["result"].(map[string]any)["data"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 10, "method": "thread/resume", "params": map[string]any{"threadId": threadID}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "idle", response["result"].(map[string]any)["thread"].(map[string]any)["status"].(map[string]any)["type"])
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "thread/status/changed", response["method"])
	require.Equal(t, "idle", response["params"].(map[string]any)["status"].(map[string]any)["type"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 11, "method": "thread/unsubscribe", "params": map[string]any{"threadId": threadID}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "unsubscribed", response["result"].(map[string]any)["status"])

	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerUnarchiveValidatesAndRequiresArchivedThread(t *testing.T) {
	seeds := []nativeSessionSeed{{id: "active"}}
	workingDir := nativeAPIFixtureWithSessionSeeds(t, proto.RunComplete{Text: "unused"}, 25*time.Millisecond, true, seeds)
	activeID := codexUUIDv7(workingDir, &proto.Session{ID: "active", CreatedAt: 1})
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, decoder.Decode(&response))

	for id, params := range []any{map[string]any{}, map[string]any{"threadId": 1}, map[string]any{"threadId": "invalid"}} {
		require.NoError(t, encoder.Encode(map[string]any{"id": id + 2, "method": "thread/unarchive", "params": params}))
		response = nil
		require.NoError(t, decoder.Decode(&response))
		require.Equal(t, float64(-32600), response["error"].(map[string]any)["code"])
		require.Contains(t, response["error"].(map[string]any)["message"], "Invalid request:")
	}

	require.NoError(t, encoder.Encode(map[string]any{"id": 5, "method": "thread/unarchive", "params": map[string]any{"threadId": activeID}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(-32600), response["error"].(map[string]any)["code"])
	require.Equal(t, "no archived rollout found for thread id "+activeID, response["error"].(map[string]any)["message"])

	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerUnarchivesExactColdThreadBeforeNotifying(t *testing.T) {
	seeds := []nativeSessionSeed{{id: "root", archived: true}, {id: "child", parentID: "root", archived: true}}
	workingDir := nativeAPIFixtureWithSessionSeeds(t, proto.RunComplete{Text: "unused"}, 25*time.Millisecond, true, seeds)
	rootID := codexUUIDv7(workingDir, &proto.Session{ID: "root", CreatedAt: 1})
	childID := codexUUIDv7(workingDir, &proto.Session{ID: "child", CreatedAt: 1})
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/unarchive", "params": map[string]any{"threadId": rootID}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(2), response["id"])
	thread := response["result"].(map[string]any)["thread"].(map[string]any)
	require.Equal(t, rootID, thread["id"])
	require.Equal(t, "root", thread["name"])
	require.Equal(t, "notLoaded", thread["status"].(map[string]any)["type"])
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "thread/unarchived", response["method"])
	require.Equal(t, rootID, response["params"].(map[string]any)["threadId"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "thread/list"}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	active := response["result"].(map[string]any)["data"].([]any)
	require.Len(t, active, 1)
	require.Equal(t, rootID, active[0].(map[string]any)["id"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 4, "method": "thread/list", "params": map[string]any{"archived": true}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	archived := response["result"].(map[string]any)["data"].([]any)
	require.Len(t, archived, 1)
	require.Equal(t, childID, archived[0].(map[string]any)["id"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 5, "method": "thread/unarchive", "params": map[string]any{"threadId": rootID}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(-32600), response["error"].(map[string]any)["code"])
	require.Equal(t, "no archived rollout found for thread id "+rootID, response["error"].(map[string]any)["message"])

	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerUnarchiveRejectsActiveWriterWithoutMutation(t *testing.T) {
	seeds := []nativeSessionSeed{{id: "root", archived: true, active: true}}
	workingDir := nativeAPIFixtureWithSessionSeeds(t, proto.RunComplete{Text: "unused"}, 25*time.Millisecond, true, seeds)
	rootID := codexUUIDv7(workingDir, &proto.Session{ID: "root", CreatedAt: 1})
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/unarchive", "params": map[string]any{"threadId": rootID}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(-32600), response["error"].(map[string]any)["code"])
	require.Equal(t, "thread "+rootID+" already has an active writer", response["error"].(map[string]any)["message"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "thread/list", "params": map[string]any{"archived": true}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Len(t, response["result"].(map[string]any)["data"], 1)

	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerUnarchiveNotificationOptOutPreservesEffect(t *testing.T) {
	seeds := []nativeSessionSeed{{id: "root", archived: true}}
	workingDir := nativeAPIFixtureWithSessionSeeds(t, proto.RunComplete{Text: "unused"}, 25*time.Millisecond, true, seeds)
	rootID := codexUUIDv7(workingDir, &proto.Session{ID: "root", CreatedAt: 1})
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}, "capabilities": map[string]any{"optOutNotificationMethods": []string{"thread/unarchived"}}}}))
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/unarchive", "params": map[string]any{"threadId": rootID}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(2), response["id"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "thread/list"}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(3), response["id"])
	require.Len(t, response["result"].(map[string]any)["data"], 1)

	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerArchiveValidatesAndRejectsUnmaterializedThread(t *testing.T) {
	workingDir := nativeAPIFixtureWithCompletionDelayAndHistory(t, proto.RunComplete{Text: "unused"}, 25*time.Millisecond, false)
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, decoder.Decode(&response))

	for id, params := range []any{map[string]any{}, map[string]any{"threadId": 1}, map[string]any{"threadId": "invalid"}} {
		require.NoError(t, encoder.Encode(map[string]any{"id": id + 2, "method": "thread/archive", "params": params}))
		response = nil
		require.NoError(t, decoder.Decode(&response))
		require.Equal(t, float64(-32600), response["error"].(map[string]any)["code"])
		require.Contains(t, response["error"].(map[string]any)["message"], "Invalid request:")
	}

	require.NoError(t, encoder.Encode(map[string]any{"id": 5, "method": "thread/start", "params": map[string]any{"cwd": workingDir}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	threadID := response["result"].(map[string]any)["thread"].(map[string]any)["id"].(string)
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "thread/started", response["method"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 6, "method": "thread/archive", "params": map[string]any{"threadId": threadID}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(-32600), response["error"].(map[string]any)["code"])
	require.Equal(t, "Invalid request: thread "+threadID+" is not materialized yet", response["error"].(map[string]any)["message"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 7, "method": "thread/list"}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Len(t, response["result"].(map[string]any)["data"], 1)

	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerArchivesThreadBeforeNotifying(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{"cwd": workingDir}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	threadID := response["result"].(map[string]any)["thread"].(map[string]any)["id"].(string)
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "thread/started", response["method"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "thread/archive", "params": map[string]any{"threadId": threadID}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(3), response["id"])
	require.Equal(t, map[string]any{}, response["result"])
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "thread/archived", response["method"])
	require.Equal(t, threadID, response["params"].(map[string]any)["threadId"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 4, "method": "thread/loaded/list"}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Empty(t, response["result"].(map[string]any)["data"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 5, "method": "thread/list"}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Empty(t, response["result"].(map[string]any)["data"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 6, "method": "thread/list", "params": map[string]any{"archived": false}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Empty(t, response["result"].(map[string]any)["data"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 7, "method": "thread/list", "params": map[string]any{"archived": true}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	archived := response["result"].(map[string]any)["data"].([]any)
	require.Len(t, archived, 1)
	require.Equal(t, threadID, archived[0].(map[string]any)["id"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 8, "method": "thread/read", "params": map[string]any{"threadId": threadID}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.NotContains(t, response, "error")
	stored := response["result"].(map[string]any)["thread"].(map[string]any)
	require.Equal(t, threadID, stored["id"])
	require.Equal(t, "notLoaded", stored["status"].(map[string]any)["type"])

	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerArchiveNotificationOptOutPreservesEffect(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}, "capabilities": map[string]any{"optOutNotificationMethods": []string{"thread/archived"}}}}))
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{"cwd": workingDir}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	threadID := response["result"].(map[string]any)["thread"].(map[string]any)["id"].(string)
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "thread/started", response["method"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "thread/archive", "params": map[string]any{"threadId": threadID}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(3), response["id"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 4, "method": "thread/list", "params": map[string]any{"archived": true}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(4), response["id"])
	require.Len(t, response["result"].(map[string]any)["data"], 1)

	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerArchiveRejectsActiveWriter(t *testing.T) {
	runStarted := make(chan struct{}, 1)
	workingDir := nativeAPIFixtureWithCompletionDelay(t, proto.RunComplete{Text: "completed"}, 500*time.Millisecond, func(string, string, string, proto.AgentMessage) {
		runStarted <- struct{}{}
	})
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{"cwd": workingDir}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	threadID := response["result"].(map[string]any)["thread"].(map[string]any)["id"].(string)
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "thread/started", response["method"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "turn/start", "params": map[string]any{"threadId": threadID, "input": []any{map[string]any{"type": "text", "text": "wait"}}}}))
	for {
		response = nil
		require.NoError(t, decoder.Decode(&response))
		if response["id"] == float64(3) {
			break
		}
	}
	for {
		response = nil
		require.NoError(t, decoder.Decode(&response))
		if response["method"] == "thread/status/changed" {
			break
		}
	}
	<-runStarted
	require.NoError(t, encoder.Encode(map[string]any{"id": 4, "method": "thread/archive", "params": map[string]any{"threadId": threadID}}))
	for {
		response = nil
		require.NoError(t, decoder.Decode(&response))
		if response["id"] == float64(4) {
			break
		}
	}
	require.Equal(t, float64(-32600), response["error"].(map[string]any)["code"])
	require.Equal(t, "thread "+threadID+" already has an active writer", response["error"].(map[string]any)["message"])

	for {
		response = nil
		require.NoError(t, decoder.Decode(&response))
		if response["method"] == "turn/completed" {
			break
		}
	}
	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerUnsubscribeDoesNotUnloadActiveTurn(t *testing.T) {
	oldDelay := codexThreadUnloadDelayForBridge
	codexThreadUnloadDelayForBridge = 80 * time.Millisecond
	t.Cleanup(func() { codexThreadUnloadDelayForBridge = oldDelay })

	workingDir := nativeAPIFixtureWithCompletionDelay(t, proto.RunComplete{Text: "completed after unsubscribe"}, 250*time.Millisecond)
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, decoder.Decode(&response))
	codexThreadUnloadDelayForBridge = oldDelay
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{"cwd": workingDir}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	threadID := response["result"].(map[string]any)["thread"].(map[string]any)["id"].(string)
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "thread/started", response["method"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "turn/start", "params": map[string]any{"threadId": threadID, "input": []any{map[string]any{"type": "text", "text": "continue"}}}}))
	for range 5 {
		response = nil
		require.NoError(t, decoder.Decode(&response))
	}
	require.NoError(t, encoder.Encode(map[string]any{"id": 4, "method": "thread/unsubscribe", "params": map[string]any{"threadId": threadID}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(4), response["id"])
	require.Equal(t, "unsubscribed", response["result"].(map[string]any)["status"])

	time.Sleep(120 * time.Millisecond)
	require.NoError(t, encoder.Encode(map[string]any{"id": 5, "method": "thread/loaded/list"}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(5), response["id"])
	require.Equal(t, []any{threadID}, response["result"].(map[string]any)["data"])
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "thread/status/changed", response["method"])
	require.Equal(t, "notLoaded", response["params"].(map[string]any)["status"].(map[string]any)["type"])
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "thread/closed", response["method"])

	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerReadsStoredThreadWithoutLoading(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}

	firstInputReader, firstInputWriter := io.Pipe()
	firstOutputReader, firstOutputWriter := io.Pipe()
	firstRequest := request
	firstRequest.Prompt = compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: firstInputReader}
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, firstInputReader, firstOutputWriter), firstRequest)
		_ = firstOutputWriter.Close()
	}()
	firstEncoder, firstDecoder := json.NewEncoder(firstInputWriter), json.NewDecoder(firstOutputReader)
	require.NoError(t, firstEncoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	var response map[string]any
	require.NoError(t, firstDecoder.Decode(&response))
	require.NoError(t, firstEncoder.Encode(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{"cwd": workingDir}}))
	response = nil
	require.NoError(t, firstDecoder.Decode(&response))
	threadID := response["result"].(map[string]any)["thread"].(map[string]any)["id"].(string)
	response = nil
	require.NoError(t, firstDecoder.Decode(&response))
	require.Equal(t, "thread/started", response["method"])
	require.NoError(t, firstInputWriter.Close())
	require.NoError(t, <-firstDone)

	secondInputReader, secondInputWriter := io.Pipe()
	secondOutputReader, secondOutputWriter := io.Pipe()
	secondRequest := request
	secondRequest.Prompt = compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: secondInputReader}
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, secondInputReader, secondOutputWriter), secondRequest)
		_ = secondOutputWriter.Close()
	}()
	secondEncoder, secondDecoder := json.NewEncoder(secondInputWriter), json.NewDecoder(secondOutputReader)
	require.NoError(t, secondEncoder.Encode(map[string]any{"id": 3, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	response = nil
	require.NoError(t, secondDecoder.Decode(&response))
	require.NoError(t, secondEncoder.Encode(map[string]any{"id": 4, "method": "thread/read", "params": map[string]any{"threadId": threadID}}))
	response = nil
	require.NoError(t, secondDecoder.Decode(&response))
	storedThread := response["result"].(map[string]any)["thread"].(map[string]any)
	require.Equal(t, "notLoaded", storedThread["status"].(map[string]any)["type"])
	require.Empty(t, storedThread["turns"])
	require.NoError(t, secondEncoder.Encode(map[string]any{"id": 5, "method": "thread/loaded/list", "params": map[string]any{"cursor": "not-a-thread-id"}}))
	response = nil
	require.NoError(t, secondDecoder.Decode(&response))
	loaded := response["result"].(map[string]any)
	require.Empty(t, loaded["data"])
	require.Nil(t, loaded["nextCursor"])
	require.NoError(t, secondEncoder.Encode(map[string]any{"id": 6, "method": "thread/read", "params": map[string]any{"threadId": threadID, "includeTurns": true}}))
	response = nil
	require.NoError(t, secondDecoder.Decode(&response))
	storedThread = response["result"].(map[string]any)["thread"].(map[string]any)
	require.Equal(t, "notLoaded", storedThread["status"].(map[string]any)["type"])
	require.Len(t, storedThread["turns"], 1)
	require.NoError(t, secondEncoder.Encode(map[string]any{"id": 7, "method": "thread/turns/list", "params": map[string]any{"threadId": threadID}}))
	response = nil
	require.NoError(t, secondDecoder.Decode(&response))
	require.Equal(t, float64(7), response["id"])
	require.Len(t, response["result"].(map[string]any)["data"], 1)
	require.NoError(t, secondEncoder.Encode(map[string]any{"id": 8, "method": "thread/resume", "params": map[string]any{"threadId": threadID}}))
	response = nil
	require.NoError(t, secondDecoder.Decode(&response))
	require.Equal(t, float64(8), response["id"])
	require.Equal(t, "idle", response["result"].(map[string]any)["thread"].(map[string]any)["status"].(map[string]any)["type"])
	response = nil
	require.NoError(t, secondDecoder.Decode(&response))
	require.Equal(t, "thread/status/changed", response["method"])
	statusParams := response["params"].(map[string]any)
	require.Equal(t, threadID, statusParams["threadId"])
	require.Equal(t, "idle", statusParams["status"].(map[string]any)["type"])
	require.NoError(t, secondEncoder.Encode(map[string]any{"id": 9, "method": "thread/loaded/list", "params": map[string]any{"limit": 0}}))
	response = nil
	require.NoError(t, secondDecoder.Decode(&response))
	require.Equal(t, float64(9), response["id"])
	loaded = response["result"].(map[string]any)
	require.Equal(t, []any{threadID}, loaded["data"])
	require.Nil(t, loaded["nextCursor"])
	require.NoError(t, secondEncoder.Encode(map[string]any{"id": 10, "method": "thread/resume", "params": map[string]any{"threadId": threadID}}))
	response = nil
	require.NoError(t, secondDecoder.Decode(&response))
	require.Equal(t, float64(10), response["id"])
	require.NoError(t, secondInputWriter.Close())
	require.NoError(t, <-secondDone)
	response = nil
	require.ErrorIs(t, secondDecoder.Decode(&response), io.EOF)
}

func TestCodexAppServerListsNativeThreadTurns(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{"cwd": workingDir}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	threadID := response["result"].(map[string]any)["thread"].(map[string]any)["id"].(string)
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "thread/started", response["method"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "thread/turns/list", "params": map[string]any{"threadId": threadID}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	result := response["result"].(map[string]any)
	require.Nil(t, result["nextCursor"])
	require.NotNil(t, result["backwardsCursor"])
	turn := result["data"].([]any)[0].(map[string]any)
	require.Equal(t, "summary", turn["itemsView"])
	require.Equal(t, []string{"userMessage", "agentMessage"}, []string{
		turn["items"].([]any)[0].(map[string]any)["type"].(string),
		turn["items"].([]any)[1].(map[string]any)["type"].(string),
	})

	require.NoError(t, encoder.Encode(map[string]any{"id": 4, "method": "thread/turns/list", "params": map[string]any{"threadId": threadID, "itemsView": "notLoaded"}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	turn = response["result"].(map[string]any)["data"].([]any)[0].(map[string]any)
	require.Equal(t, "notLoaded", turn["itemsView"])
	require.Empty(t, turn["items"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 5, "method": "thread/turns/list", "params": map[string]any{"threadId": threadID, "itemsView": "full", "sortDirection": "asc", "limit": 0}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	turn = response["result"].(map[string]any)["data"].([]any)[0].(map[string]any)
	require.Equal(t, "full", turn["itemsView"])
	require.Len(t, turn["items"], 2)

	invalidRequests := []map[string]any{
		{"id": 6, "method": "thread/turns/list", "params": map[string]any{"threadId": threadID, "sortDirection": "sideways"}},
		{"id": 7, "method": "thread/turns/list", "params": map[string]any{"threadId": threadID, "itemsView": "unknown"}},
		{"id": 8, "method": "thread/turns/list", "params": map[string]any{"threadId": threadID, "cursor": "not-json"}},
	}
	for _, invalid := range invalidRequests {
		require.NoError(t, encoder.Encode(invalid))
		response = nil
		require.NoError(t, decoder.Decode(&response))
		require.Equal(t, float64(-32600), response["error"].(map[string]any)["code"])
	}

	require.NoError(t, encoder.Encode(map[string]any{"id": 9, "method": "thread/items/list", "params": map[string]any{"threadId": threadID}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(-32601), response["error"].(map[string]any)["code"])
	require.Equal(t, "thread/items/list is not supported yet", response["error"].(map[string]any)["message"])

	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerRejectsUnavailableLoadedThreadHistory(t *testing.T) {
	tests := []struct {
		name             string
		ephemeral        bool
		includeHistory   bool
		message          string
		turnsListMessage string
	}{
		{
			name:             "ephemeral thread",
			ephemeral:        true,
			includeHistory:   true,
			message:          "ephemeral threads do not support includeTurns",
			turnsListMessage: "ephemeral threads do not support thread/turns/list",
		},
		{
			name:             "before first user message",
			includeHistory:   false,
			message:          "includeTurns is unavailable before first user message",
			turnsListMessage: "thread/turns/list is unavailable before first user message",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workingDir := nativeAPIFixtureWithCompletionDelayAndHistory(t, proto.RunComplete{Text: "streamed response"}, 25*time.Millisecond, test.includeHistory)
			inputReader, inputWriter := io.Pipe()
			outputReader, outputWriter := io.Pipe()
			request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
			done := make(chan error, 1)
			go func() {
				done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
				_ = outputWriter.Close()
			}()
			encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
			var response map[string]any
			require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
			require.NoError(t, decoder.Decode(&response))
			require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{"cwd": workingDir, "ephemeral": test.ephemeral}}))
			response = nil
			require.NoError(t, decoder.Decode(&response))
			threadID := response["result"].(map[string]any)["thread"].(map[string]any)["id"].(string)
			response = nil
			require.NoError(t, decoder.Decode(&response))
			require.Equal(t, "thread/started", response["method"])
			require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "thread/read", "params": map[string]any{"threadId": threadID}}))
			response = nil
			require.NoError(t, decoder.Decode(&response))
			require.Equal(t, "idle", response["result"].(map[string]any)["thread"].(map[string]any)["status"].(map[string]any)["type"])
			require.NoError(t, encoder.Encode(map[string]any{"id": 4, "method": "thread/read", "params": map[string]any{"threadId": threadID, "includeTurns": true}}))
			response = nil
			require.NoError(t, decoder.Decode(&response))
			require.Equal(t, float64(-32600), response["error"].(map[string]any)["code"])
			require.Contains(t, response["error"].(map[string]any)["message"], test.message)
			require.NoError(t, encoder.Encode(map[string]any{"id": 5, "method": "thread/turns/list", "params": map[string]any{"threadId": threadID}}))
			response = nil
			require.NoError(t, decoder.Decode(&response))
			require.Equal(t, float64(-32600), response["error"].(map[string]any)["code"])
			require.Contains(t, response["error"].(map[string]any)["message"], test.turnsListMessage)
			require.NoError(t, inputWriter.Close())
			require.NoError(t, <-done)
		})
	}
}

func TestCodexAppServerReadTracksActiveAndSystemErrorStatus(t *testing.T) {
	workingDir := nativeAPIFixtureWithCompletionDelay(t, proto.RunComplete{Error: "simulated failure"}, 250*time.Millisecond)
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	var response map[string]any
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{"cwd": workingDir}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	threadID := response["result"].(map[string]any)["thread"].(map[string]any)["id"].(string)
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "turn/start", "params": map[string]any{"threadId": threadID, "input": []any{map[string]any{"type": "text", "text": "fail"}}}}))
	readRequested, readObserved, completed := false, false, false
	statusTypes := make([]string, 0, 2)
	terminalStatusBeforeCompletion := false
	for !completed {
		response = nil
		require.NoError(t, decoder.Decode(&response))
		if response["method"] == "item/started" && !readRequested {
			readRequested = true
			require.NoError(t, encoder.Encode(map[string]any{"id": 4, "method": "thread/read", "params": map[string]any{"threadId": threadID}}))
		}
		if response["id"] == float64(4) {
			readObserved = true
			status := response["result"].(map[string]any)["thread"].(map[string]any)["status"].(map[string]any)
			require.Equal(t, "active", status["type"])
			require.Empty(t, status["activeFlags"])
		}
		if response["method"] == "thread/status/changed" {
			params := response["params"].(map[string]any)
			require.Equal(t, threadID, params["threadId"])
			statusType := params["status"].(map[string]any)["type"].(string)
			statusTypes = append(statusTypes, statusType)
			terminalStatusBeforeCompletion = statusType == "systemError"
		}
		if response["method"] == "turn/completed" {
			completed = true
			require.True(t, terminalStatusBeforeCompletion)
			require.Equal(t, "failed", response["params"].(map[string]any)["turn"].(map[string]any)["status"])
		}
	}
	require.Equal(t, []string{"active", "systemError"}, statusTypes)
	require.True(t, readObserved)
	require.NoError(t, encoder.Encode(map[string]any{"id": 5, "method": "thread/read", "params": map[string]any{"threadId": threadID}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	status := response["result"].(map[string]any)["thread"].(map[string]any)["status"].(map[string]any)
	require.Equal(t, "systemError", status["type"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 6, "method": "thread/unsubscribe", "params": map[string]any{"threadId": threadID}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "unsubscribed", response["result"].(map[string]any)["status"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 7, "method": "thread/resume", "params": map[string]any{"threadId": threadID}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	status = response["result"].(map[string]any)["thread"].(map[string]any)["status"].(map[string]any)
	require.Equal(t, "systemError", status["type"])
	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAccountReadAlwaysUsesLocalAccount(t *testing.T) {
	require.Equal(t, map[string]any{"account": map[string]any{"type": "apiKey"}, "requiresOpenaiAuth": false}, codexAccountReadResult())
}

func TestCodexSkillsListEntryProjectsNativeCatalog(t *testing.T) {
	result, err := codexSkillsListEntry("/workspace", proto.SkillCatalogResponse{
		Skills: []proto.SkillCatalogEntry{
			{ID: "/workspace/.agents/skills/repo/SKILL.md", Name: "repo", Description: "Repo skill.", Source: "project", Enabled: true},
			{ID: "crux://skills/builtin/SKILL.md", Name: "builtin", Description: "Builtin skill.", Source: "system", Enabled: false},
		},
		States: []proto.SkillState{
			{Name: "repo", Path: "/workspace/.agents/skills/repo/SKILL.md", State: proto.SkillStateNormal},
			{Path: "/workspace/.agents/skills/broken/SKILL.md", State: proto.SkillStateError, Error: "invalid frontmatter"},
		},
	})
	require.NoError(t, err)
	require.Equal(t, "/workspace", result["cwd"])
	skills := result["skills"].([]any)
	require.Equal(t, "repo", skills[0].(map[string]any)["scope"])
	require.Equal(t, true, skills[0].(map[string]any)["enabled"])
	require.Nil(t, skills[0].(map[string]any)["pluginId"])
	require.Equal(t, "system", skills[1].(map[string]any)["scope"])
	require.Equal(t, false, skills[1].(map[string]any)["enabled"])
	require.Equal(t, []any{map[string]any{"path": "/workspace/.agents/skills/broken/SKILL.md", "message": "invalid frontmatter"}}, result["errors"])

	_, err = codexSkillsListEntry("/workspace", proto.SkillCatalogResponse{Skills: []proto.SkillCatalogEntry{{Source: "unknown"}}})
	require.EqualError(t, err, `unsupported native skill source "unknown"`)
}

func TestCodexAppServerConfigRequirementsPropagatesNativeFailure(t *testing.T) {
	requests := make(chan struct{}, 2)
	workingDir := nativeAPIFixtureWithSessionAndTaskSeedsAndFileSearch(t, proto.RunComplete{}, 25*time.Millisecond, true, nil, nil, nil, nil, nil, func(w http.ResponseWriter, _ *http.Request) {
		requests <- struct{}{}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		require.NoError(t, json.NewEncoder(w).Encode(proto.Error{Message: "managed requirements unavailable"}))
	}, nil, nil)
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{
		Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir,
		Prompt:  compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader},
		Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true},
	}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "configRequirements/read", "params": map[string]any{}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(-32000), response["error"].(map[string]any)["code"])
	require.Contains(t, response["error"].(map[string]any)["message"], "managed requirements unavailable")
	select {
	case <-requests:
	default:
		t.Fatal("empty-object configRequirements/read did not reach the native endpoint")
	}

	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "configRequirements/read"}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(-32000), response["error"].(map[string]any)["code"])
	require.Contains(t, response["error"].(map[string]any)["message"], "managed requirements unavailable")
	select {
	case <-requests:
	default:
		t.Fatal("valid configRequirements/read did not reach the native endpoint")
	}
	select {
	case <-requests:
		t.Fatal("configRequirements/read reached the native endpoint more than once")
	default:
	}
	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerStdioLifecycle(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	executable := "unused"
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{
		Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir,
		Prompt:  compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader},
		Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true},
	}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation(executable, workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder := json.NewEncoder(inputWriter)
	decoder := json.NewDecoder(outputReader)
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	var response map[string]any
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(1), response["id"])
	require.NoError(t, encoder.Encode(map[string]any{"method": "initialized", "params": map[string]any{}}))
	require.NoError(t, encoder.Encode(map[string]any{"id": 11, "method": "account/read", "params": map[string]any{}}))
	require.NoError(t, decoder.Decode(&response))
	accountResult := response["result"].(map[string]any)
	require.Equal(t, map[string]any{"type": "apiKey"}, accountResult["account"])
	require.Equal(t, false, accountResult["requiresOpenaiAuth"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 12, "method": "account/read", "params": map[string]any{"refreshToken": true}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.NotContains(t, response, "error")
	require.Equal(t, accountResult, response["result"])
	response = nil
	require.NoError(t, encoder.Encode(map[string]any{"id": 13, "method": "configRequirements/read"}))
	require.NoError(t, decoder.Decode(&response))
	require.Nil(t, response["result"].(map[string]any)["requirements"])
	response = nil
	require.NoError(t, encoder.Encode(map[string]any{"id": 14, "method": "configRequirements/read", "params": nil}))
	require.NoError(t, decoder.Decode(&response))
	require.Nil(t, response["result"].(map[string]any)["requirements"])
	response = nil
	require.NoError(t, encoder.Encode(map[string]any{"id": 15, "method": "configRequirements/read", "params": map[string]any{}}))
	require.NoError(t, decoder.Decode(&response))
	require.NotContains(t, response, "error")
	require.Nil(t, response["result"].(map[string]any)["requirements"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 20, "method": "config/read", "params": []any{workingDir}}))
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(-32600), response["error"].(map[string]any)["code"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "config/read", "params": map[string]any{"cwd": workingDir, "includeLayers": true}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.NotContains(t, response, "error")
	require.Equal(t, "OPENAI-GPT-5", response["result"].(map[string]any)["config"].(map[string]any)["model"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "config/read", "params": map[string]any{"cwd": workingDir}}))
	require.NoError(t, decoder.Decode(&response))
	configResult := response["result"].(map[string]any)
	require.Equal(t, "OPENAI-GPT-5", configResult["config"].(map[string]any)["model"])
	require.Equal(t, "crux", configResult["config"].(map[string]any)["model_provider"])
	require.NotNil(t, configResult["origins"])
	require.NotContains(t, configResult, "layers")

	response = nil
	require.NoError(t, encoder.Encode(map[string]any{"id": 16, "method": "skills/list"}))
	require.NoError(t, decoder.Decode(&response))
	skillEntries := response["result"].(map[string]any)["data"].([]any)
	require.Len(t, skillEntries, 1)
	require.Equal(t, workingDir, skillEntries[0].(map[string]any)["cwd"])
	skillValues := skillEntries[0].(map[string]any)["skills"].([]any)
	require.Len(t, skillValues, 1)
	require.Equal(t, "fixture", skillValues[0].(map[string]any)["name"])
	require.Equal(t, "repo", skillValues[0].(map[string]any)["scope"])
	require.Equal(t, true, skillValues[0].(map[string]any)["enabled"])
	require.Nil(t, skillValues[0].(map[string]any)["pluginId"])

	response = nil
	require.NoError(t, encoder.Encode(map[string]any{"id": 17, "method": "skills/list", "params": map[string]any{"cwds": []string{workingDir, workingDir}, "forceReload": true}}))
	require.NoError(t, decoder.Decode(&response))
	skillEntries = response["result"].(map[string]any)["data"].([]any)
	require.Len(t, skillEntries, 2)
	require.Equal(t, workingDir, skillEntries[0].(map[string]any)["cwd"])
	require.Equal(t, workingDir, skillEntries[1].(map[string]any)["cwd"])

	response = nil
	require.NoError(t, encoder.Encode(map[string]any{"id": 18, "method": "skills/list", "params": []any{workingDir}}))
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(-32600), response["error"].(map[string]any)["code"])
	response = nil
	require.NoError(t, encoder.Encode(map[string]any{"id": 19, "method": "skills/list", "params": map[string]any{"cwds": []string{"relative"}}}))
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(-32600), response["error"].(map[string]any)["code"])

	for _, invalid := range []struct {
		id     int
		method string
		params any
		code   float64
	}{
		{id: 21, method: "config/read", params: []any{}, code: -32600},
		{id: 22, method: "thread/read", params: map[string]any{}, code: -32600},
		{id: 23, method: "thread/resume", params: map[string]any{}, code: -32602},
		{id: 24, method: "thread/fork", params: map[string]any{}, code: -32602},
		{id: 25, method: "turn/interrupt", params: map[string]any{}, code: -32602},
	} {
		require.NoError(t, encoder.Encode(map[string]any{"id": invalid.id, "method": invalid.method, "params": invalid.params}))
		require.NoError(t, decoder.Decode(&response))
		require.Equal(t, invalid.code, response["error"].(map[string]any)["code"])
	}
	for _, invalid := range []struct {
		id     int
		method string
	}{
		{id: 26, method: "thread/read"},
		{id: 27, method: "thread/resume"},
		{id: 28, method: "thread/fork"},
	} {
		require.NoError(t, encoder.Encode(map[string]any{"id": invalid.id, "method": invalid.method, "params": []any{}}))
		require.NoError(t, decoder.Decode(&response))
		expectedCode := float64(-32602)
		if invalid.method == "thread/read" {
			expectedCode = -32600
		}
		require.Equal(t, expectedCode, response["error"].(map[string]any)["code"])
	}
	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "plugin/list", "params": map[string]any{"cwds": []string{workingDir}}}))
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(-32601), response["error"].(map[string]any)["code"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 4, "method": "model/list", "params": map[string]any{}}))
	require.NoError(t, decoder.Decode(&response))
	models := response["result"].(map[string]any)["data"].([]any)
	require.Len(t, models, 3)
	require.Equal(t, "ANTHROPIC-CLAUDE-SONNET", models[0].(map[string]any)["id"])
	require.Equal(t, "ANTHROPIC-CLAUDE-SONNET", models[0].(map[string]any)["model"])
	require.Equal(t, "OPENAI-GPT-4-1", models[1].(map[string]any)["id"])
	require.Equal(t, "OPENAI-GPT-5", models[2].(map[string]any)["id"])
	for _, value := range models {
		model := value.(map[string]any)
		require.Contains(t, model, "supportsPersonality")
		require.Contains(t, model, "serviceTiers")
		require.Contains(t, model, "defaultServiceTier")
	}
	require.NoError(t, encoder.Encode(map[string]any{"id": 41, "method": "model/list", "params": map[string]any{"limit": 1, "includeHidden": false}}))
	require.NoError(t, decoder.Decode(&response))
	page := response["result"].(map[string]any)
	require.Len(t, page["data"], 1)
	require.Equal(t, "1", page["nextCursor"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 42, "method": "model/list", "params": map[string]any{"limit": -1}}))
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(-32600), response["error"].(map[string]any)["code"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 5, "method": "thread/start", "params": map[string]any{"cwd": workingDir, "model": "claude-sonnet", "modelProvider": "crux", "approvalPolicy": "never", "approvalsReviewer": "user", "sandbox": "read-only"}}))
	require.NoError(t, decoder.Decode(&response))
	startResult := response["result"].(map[string]any)
	require.Equal(t, "ANTHROPIC-CLAUDE-SONNET", startResult["model"])
	require.Equal(t, "crux", startResult["modelProvider"])
	startThread := startResult["thread"].(map[string]any)
	require.Equal(t, "crux", startThread["modelProvider"])
	require.Equal(t, "idle", startThread["status"].(map[string]any)["type"])
	threadID := startThread["id"].(string)
	parsedThreadID, err := uuid.Parse(threadID)
	require.NoError(t, err)
	require.Equal(t, uuid.Version(7), parsedThreadID.Version())
	require.NotEqual(t, "thread-1", threadID)
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "thread/started", response["method"])

	for id, method := range map[int]string{51: "thread/read", 52: "thread/resume", 53: "thread/fork", 54: "turn/interrupt"} {
		require.NoError(t, encoder.Encode(map[string]any{"id": id, "method": method, "params": map[string]any{}}))
		require.NoError(t, decoder.Decode(&response))
		expectedCode := float64(-32602)
		if method == "thread/read" {
			expectedCode = -32600
		}
		require.Equal(t, expectedCode, response["error"].(map[string]any)["code"])
	}
	require.NoError(t, encoder.Encode(map[string]any{"id": 55, "method": "turn/interrupt", "params": map[string]any{"threadId": threadID, "turnId": "stale-turn"}}))
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(-32600), response["error"].(map[string]any)["code"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 6, "method": "thread/read", "params": map[string]any{"threadId": threadID, "includeTurns": true}}))
	require.NoError(t, decoder.Decode(&response))
	readThread := response["result"].(map[string]any)["thread"].(map[string]any)
	require.Equal(t, threadID, readThread["id"])
	require.Equal(t, "idle", readThread["status"].(map[string]any)["type"])
	turns := readThread["turns"].([]any)
	require.Len(t, turns, 1)
	turn := turns[0].(map[string]any)
	require.Equal(t, "completed", turn["status"])
	items := turn["items"].([]any)
	require.Equal(t, "remember this", items[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"])
	require.Equal(t, "streamed response", items[1].(map[string]any)["text"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 61, "method": "thread/read", "params": map[string]any{"threadId": threadID}}))
	require.NoError(t, decoder.Decode(&response))
	metadataThread := response["result"].(map[string]any)["thread"].(map[string]any)
	require.Empty(t, metadataThread["turns"])
	require.Equal(t, "idle", metadataThread["status"].(map[string]any)["type"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 62, "method": "thread/read", "params": map[string]any{"threadId": threadID, "includeTurns": nil}}))
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(-32600), response["error"].(map[string]any)["code"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 63, "method": "thread/read", "params": map[string]any{"threadId": threadID, "includeTurns": "yes"}}))
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(-32600), response["error"].(map[string]any)["code"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 7, "method": "thread/resume", "params": map[string]any{"threadId": threadID, "model": "claude-sonnet", "modelProvider": "crux"}}))
	require.NoError(t, decoder.Decode(&response))
	resumeResult := response["result"].(map[string]any)
	for _, field := range []string{"thread", "model", "modelProvider", "approvalPolicy", "approvalsReviewer", "cwd", "sandbox"} {
		require.Contains(t, resumeResult, field)
	}
	require.Len(t, resumeResult["thread"].(map[string]any)["turns"], 1)

	require.NoError(t, encoder.Encode(map[string]any{"id": 71, "method": "thread/resume", "params": map[string]any{"threadId": threadID, "excludeTurns": true}}))
	require.NoError(t, decoder.Decode(&response))
	require.Empty(t, response["result"].(map[string]any)["thread"].(map[string]any)["turns"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 8, "method": "thread/fork", "params": map[string]any{"threadId": threadID}}))
	require.NoError(t, decoder.Decode(&response))
	forkThread := response["result"].(map[string]any)["thread"].(map[string]any)
	forkID := forkThread["id"].(string)
	parsedForkID, err := uuid.Parse(forkID)
	require.NoError(t, err)
	require.Equal(t, uuid.Version(7), parsedForkID.Version())
	require.NotEqual(t, threadID, forkID)
	require.NotContains(t, forkID, "fork-thread-1")
	require.Equal(t, threadID, forkThread["forkedFromId"])
	require.Nil(t, forkThread["parentThreadId"])
	require.Len(t, forkThread["turns"], 1)
	require.NoError(t, decoder.Decode(&response))
	startedFork := response["params"].(map[string]any)["thread"].(map[string]any)
	require.Equal(t, forkID, startedFork["id"])
	require.Empty(t, startedFork["turns"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 9, "method": "thread/list", "params": map[string]any{}}))
	require.NoError(t, decoder.Decode(&response))
	listed := response["result"].(map[string]any)["data"].([]any)
	require.Len(t, listed, 2)
	for _, value := range listed {
		listedID := value.(map[string]any)["id"].(string)
		parsedListedID, parseErr := uuid.Parse(listedID)
		require.NoError(t, parseErr)
		require.Equal(t, uuid.Version(7), parsedListedID.Version())
		require.NotContains(t, listedID, "thread-1")
	}
	require.NotNil(t, response["result"].(map[string]any)["backwardsCursor"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 91, "method": "thread/list", "params": map[string]any{"searchTerm": "Cod"}}))
	require.NoError(t, decoder.Decode(&response))
	require.Len(t, response["result"].(map[string]any)["data"], 2)
	require.NoError(t, encoder.Encode(map[string]any{"id": 912, "method": "thread/list", "params": map[string]any{"searchTerm": "cod"}}))
	require.NoError(t, decoder.Decode(&response))
	require.Empty(t, response["result"].(map[string]any)["data"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 911, "method": "thread/list", "params": map[string]any{"isPinned": true}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.NotContains(t, response, "error")
	require.Len(t, response["result"].(map[string]any)["data"], 2)
	require.NoError(t, encoder.Encode(map[string]any{"id": 92, "method": "thread/list", "params": map[string]any{"limit": 1, "sortKey": "updated_at", "sortDirection": "desc"}}))
	require.NoError(t, decoder.Decode(&response))
	pageResult := response["result"].(map[string]any)
	require.Len(t, pageResult["data"], 1)
	require.NotNil(t, pageResult["nextCursor"])
	require.NotNil(t, pageResult["backwardsCursor"])
	forwardCursor := pageResult["nextCursor"].(string)

	require.NoError(t, encoder.Encode(map[string]any{"id": 93, "method": "thread/list", "params": map[string]any{"cursor": forwardCursor, "limit": 1, "sortKey": "updated_at", "sortDirection": "desc"}}))
	require.NoError(t, decoder.Decode(&response))
	pageResult = response["result"].(map[string]any)
	require.Len(t, pageResult["data"], 1)
	require.Nil(t, pageResult["nextCursor"])
	require.NotNil(t, pageResult["backwardsCursor"])
	secondPageID := pageResult["data"].([]any)[0].(map[string]any)["id"]
	reverseCursor := pageResult["backwardsCursor"].(string)

	require.NoError(t, encoder.Encode(map[string]any{"id": 94, "method": "thread/list", "params": map[string]any{"cursor": reverseCursor, "limit": 1, "sortKey": "updated_at", "sortDirection": "asc"}}))
	require.NoError(t, decoder.Decode(&response))
	pageResult = response["result"].(map[string]any)
	require.Len(t, pageResult["data"], 1)
	require.Equal(t, secondPageID, pageResult["data"].([]any)[0].(map[string]any)["id"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 95, "method": "turn/start", "params": map[string]any{"threadId": threadID, "model": "OPENAI-GPT-5", "input": []any{map[string]any{"type": "image", "url": "file:///tmp/image.png"}}}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(-32602), response["error"].(map[string]any)["code"])
	require.Equal(t, `turn input type "image" is not supported by the Crux compatibility layer`, response["error"].(map[string]any)["message"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 96, "method": "thread/read", "params": map[string]any{"threadId": threadID}}))
	response = nil
	require.NoError(t, decoder.Decode(&response))
	threadAfterRejectedTurn := response["result"].(map[string]any)["thread"].(map[string]any)
	require.Equal(t, "crux", threadAfterRejectedTurn["modelProvider"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 10, "method": "turn/start", "params": map[string]any{"threadId": threadID, "model": "ANTHROPIC-CLAUDE-SONNET", "input": []any{map[string]any{"type": "text", "text": "hello"}}}}))
	methods := make(map[string]bool)
	itemStatuses := make(map[string]string)
	statusTypes := make([]string, 0, 2)
	terminalStatusBeforeCompletion := false
	for {
		require.NoError(t, decoder.Decode(&response))
		if method, ok := response["method"].(string); ok {
			methods[method] = true
			if method == "item/started" || method == "item/completed" {
				item := response["params"].(map[string]any)["item"].(map[string]any)
				if item["type"] == "agentMessage" {
					itemStatuses[method] = item["status"].(string)
				}
			}
			if method == "thread/status/changed" {
				params := response["params"].(map[string]any)
				require.Equal(t, threadID, params["threadId"])
				statusType := params["status"].(map[string]any)["type"].(string)
				statusTypes = append(statusTypes, statusType)
				terminalStatusBeforeCompletion = statusType == "idle"
			}
		}
		if response["method"] == "turn/completed" {
			require.True(t, terminalStatusBeforeCompletion)
			break
		}
	}
	require.NoError(t, encoder.Encode(map[string]any{"id": 101, "method": "turn/interrupt", "params": map[string]any{"threadId": threadID, "turnId": "completed-turn"}}))
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(-32600), response["error"].(map[string]any)["code"])
	require.Equal(t, "inProgress", itemStatuses["item/started"])
	require.Equal(t, "completed", itemStatuses["item/completed"])
	require.Equal(t, []string{"active", "idle"}, statusTypes)
	require.True(t, methods["turn/started"])
	require.True(t, methods["item/started"])
	require.True(t, methods["item/agentMessage/delta"])
	require.True(t, methods["item/completed"])
	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestNativeAPIFixtureBroadcastsRunCompletion(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	first, err := codexClientFactory(workingDir)
	require.NoError(t, err)
	second, err := codexClientFactory(workingDir)
	require.NoError(t, err)
	workspace, err := first.CreateWorkspace(ctx, proto.Workspace{Path: workingDir})
	require.NoError(t, err)
	_, err = second.CreateWorkspace(ctx, proto.Workspace{Path: workingDir})
	require.NoError(t, err)
	firstEvents, err := first.SubscribeEvents(ctx, workspace.ID)
	require.NoError(t, err)
	secondEvents, err := second.SubscribeEvents(ctx, workspace.ID)
	require.NoError(t, err)
	current, err := first.CreateSession(ctx, workspace.ID, "broadcast")
	require.NoError(t, err)
	require.NoError(t, first.SendAgentMessage(ctx, workspace.ID, proto.AgentMessage{SessionID: current.ID, RunID: "broadcast-run", Prompt: "hello"}))
	for index, stream := range []<-chan any{firstEvents, secondEvents} {
		found := false
		for !found {
			select {
			case event, ok := <-stream:
				require.True(t, ok, "subscriber %d closed before completion", index)
				if completed, ok := event.(pubsub.Event[proto.RunComplete]); ok {
					require.Equal(t, "broadcast-run", completed.Payload.RunID)
					found = true
				}
			case <-ctx.Done():
				t.Fatalf("subscriber %d did not receive completion", index)
			}
		}
	}
}
