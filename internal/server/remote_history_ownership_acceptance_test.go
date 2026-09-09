package server_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/stretchr/testify/require"
)

func TestRemoteHistoryPrincipalIsolationAfterRetirementThroughMTLS(t *testing.T) {
	for _, explicitDataRoot := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit-data-root=%t", explicitDataRoot), func(t *testing.T) {
			root := remoteOwnershipAcceptanceRoot(t)
			ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
			defer cancel()
			workdir := filepath.Join(root, "canonical-workspace")
			require.NoError(t, os.MkdirAll(workdir, 0o700))
			alias := filepath.Join(root, "workspace-alias")
			require.NoError(t, os.Symlink(workdir, alias))
			var requestedDataRoot, dataAlias string
			if explicitDataRoot {
				requestedDataRoot = filepath.Join(root, "explicit-history")
				dataAlias = filepath.Join(root, "history-alias")
				require.NoError(t, os.MkdirAll(requestedDataRoot, 0o700))
				require.NoError(t, os.Symlink(requestedDataRoot, dataAlias))
			}
			releaseFile := filepath.Join(root, "release-synthetic-shell")
			const outputMarker = "principal-A-persisted-task-output"
			const callID = "call_principal_history_shell"
			command := "printf '" + outputMarker + "\\n'; while [ ! -f " + remoteOwnershipAcceptanceShellQuote(releaseFile) + " ]; do sleep 0.02; done"
			var toolCalls, wrongCredentials atomic.Int32
			provider := remoteOwnershipAcceptanceProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, "cannot read synthetic request", http.StatusBadRequest)
					return
				}
				if r.Header.Get("Authorization") != "Bearer synthetic-history-key" || r.URL.Path != "/v1/chat/completions" {
					wrongCredentials.Add(1)
				}
				if r.Header.Get("x-request-purpose") != "conversation" || toolCalls.Load() != 0 {
					writeLiveRevocationText(w, "principal-A-persisted-session-reply")
					return
				}
				var request struct {
					Tools []struct {
						Function struct {
							Name string `json:"name"`
						} `json:"function"`
					} `json:"tools"`
				}
				if json.Unmarshal(body, &request) != nil {
					http.Error(w, "invalid synthetic request", http.StatusBadRequest)
					return
				}
				var advertised bool
				for _, tool := range request.Tools {
					advertised = advertised || tool.Function.Name == "bash"
				}
				if !advertised || !toolCalls.CompareAndSwap(0, 1) {
					http.Error(w, "expected one real advertised bash tool", http.StatusBadRequest)
					return
				}
				arguments, _ := json.Marshal(map[string]any{"command": command, "description": "Disposable history ownership task", "run_in_background": true})
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: {\"id\":\"history\",\"object\":\"chat.completion.chunk\",\"model\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":%q,\"type\":\"function\",\"function\":{\"name\":\"bash\",\"arguments\":%q}}]},\"finish_reason\":null}]}\n\n", callID, string(arguments))
				_, _ = fmt.Fprint(w, "data: {\"id\":\"history\",\"object\":\"chat.completion.chunk\",\"model\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
			}))
			h := newRemoteOwnershipAcceptanceServer(t, root)
			protectedBefore := remoteOwnershipAcceptanceProtectedState(t)
			proposal := remoteHistoryAcceptanceProposal(t, provider.URL)
			a, b := h.client(t, "A"), h.client(t, "B")
			created, err := a.CreateWorkspace(ctx, proto.Workspace{Path: alias, DataDir: dataAlias, Runtime: &proposal, AuthorityMode: "client"})
			require.NoError(t, err)
			require.Equal(t, workdir, created.Path)
			require.Equal(t, requestedDataRoot, created.RequestedDataDir)
			require.NotNil(t, created.Authority)
			wantData := filepath.Join(workdir, ".crux", "remote", created.Authority.Principal)
			if explicitDataRoot {
				sum := sha256.Sum256([]byte(workdir))
				wantData = filepath.Join(requestedDataRoot, "remote", created.Authority.Principal, hex.EncodeToString(sum[:]))
			}
			require.Equal(t, wantData, created.DataDir)
			reused, err := a.CreateWorkspace(ctx, proto.Workspace{Path: workdir, DataDir: requestedDataRoot, Runtime: &proposal, AuthorityMode: "client"})
			require.NoError(t, err)
			require.Equal(t, created.ID, reused.ID, "canonical aliases must reuse the exact live workspace")
			require.Equal(t, created.DataDir, reused.DataDir)
			require.NoError(t, a.InitiateAgentProcessing(ctx, created.ID, false))
			session, err := a.CreateSession(ctx, created.ID, "Principal A durable history")
			require.NoError(t, err)
			events, err := a.SubscribeEvents(ctx, created.ID, *created.Authority)
			require.NoError(t, err)
			require.NoError(t, a.SendMessageWithPermissionMode(ctx, created.ID, session.ID, "history-persisted-run", "Create the disposable background history marker", proto.AgentPermissionBypass))
			complete := remoteOwnershipAcceptanceRun(t, ctx, events, "history-persisted-run")
			require.Empty(t, complete.Error)
			require.Contains(t, complete.Text, "principal-A-persisted-session-reply")
			tasks, err := a.ListTasks(ctx, created.ID)
			require.NoError(t, err)
			var shellID string
			for _, task := range tasks {
				if task.Type == managedtask.TypeShell {
					require.Empty(t, shellID, "the public message creates exactly one managed shell task")
					shellID = task.ID
				}
			}
			require.NotEmpty(t, shellID, "the public message must create a real managed shell task")
			require.NoError(t, os.WriteFile(releaseFile, nil, 0o600))
			output, err := a.TaskOutput(ctx, created.ID, shellID, true, 5*time.Second)
			require.NoError(t, err)
			require.Equal(t, callID, output.Task.Ownership.OriginToolCallID)
			require.Equal(t, session.ID, output.Task.Ownership.ParentSessionID)
			require.Equal(t, managedtask.StatusCompleted, output.Task.State.Status)
			require.Contains(t, output.Output, outputMarker)
			messages, err := a.ListMessages(ctx, created.ID, session.ID)
			require.NoError(t, err)
			messageBytes, err := json.Marshal(messages)
			require.NoError(t, err)
			require.Contains(t, string(messageBytes), "principal-A-persisted-session-reply")

			_, err = b.CreateWorkspace(ctx, proto.Workspace{Path: workdir, DataDir: requestedDataRoot, Runtime: &proposal, AuthorityMode: "client"})
			require.ErrorContains(t, err, "403", "a second principal cannot reuse A's live workspace")
			_, err = b.GetSession(ctx, created.ID, session.ID)
			require.ErrorContains(t, err, "403")
			_, err = b.TaskOutput(ctx, created.ID, shellID, false, 0)
			require.ErrorContains(t, err, "403")
			require.NoError(t, a.RetireClient(ctx))
			_, err = a.CreateWorkspace(ctx, proto.Workspace{Path: alias, DataDir: dataAlias, Runtime: &proposal, AuthorityMode: "client"})
			require.Error(t, err, "a retired client UUID cannot acquire a fresh claim")

			fresh, err := b.CreateWorkspace(ctx, proto.Workspace{Path: alias, DataDir: dataAlias, Runtime: &proposal, AuthorityMode: "client"})
			require.NoError(t, err, "full A retirement releases the path for another authorized principal")
			require.NotEqual(t, created.ID, fresh.ID)
			require.NotEqual(t, created.Authority.Principal, fresh.Authority.Principal)
			require.NotEqual(t, created.DataDir, fresh.DataDir)
			require.NoError(t, b.InitiateAgentProcessing(ctx, fresh.ID, false))
			freshSessions, err := b.ListSessions(ctx, fresh.ID)
			require.NoError(t, err)
			require.Empty(t, freshSessions)
			freshTasks, err := b.ListTasks(ctx, fresh.ID)
			require.NoError(t, err)
			require.Empty(t, freshTasks)
			foreignSession, err := b.GetSession(ctx, fresh.ID, session.ID)
			require.Error(t, err, "guessing A's session ID inside B's workspace must fail")
			require.Nil(t, foreignSession)
			foreignOutput, err := b.TaskOutput(ctx, fresh.ID, shellID, false, 0)
			require.Error(t, err, "guessing A's durable task ID must fail")
			require.Empty(t, foreignOutput.Output)
			require.Empty(t, foreignOutput.Task.ID)
			_, err = b.GetSession(ctx, created.ID, session.ID)
			require.ErrorContains(t, err, "404", "the retired workspace ID must not be rebound")
			require.NoError(t, b.RetireClient(ctx))

			// A new SDK has a fresh claim UUID but the exact same certificate.
			// That principal recovers its own canonical storage, not B's storage.
			aAgain := h.client(t, "A")
			restored, err := aAgain.CreateWorkspace(ctx, proto.Workspace{Path: workdir, DataDir: requestedDataRoot, Runtime: &proposal, AuthorityMode: "client"})
			require.NoError(t, err)
			require.NotEqual(t, created.ID, restored.ID)
			require.Equal(t, created.Authority.Principal, restored.Authority.Principal)
			require.Equal(t, created.DataDir, restored.DataDir)
			require.NoError(t, aAgain.InitiateAgentProcessing(ctx, restored.ID, false))
			restoredSession, err := aAgain.GetSession(ctx, restored.ID, session.ID)
			require.NoError(t, err)
			require.Equal(t, session.ID, restoredSession.ID)
			restoredMessages, err := aAgain.ListMessages(ctx, restored.ID, session.ID)
			require.NoError(t, err)
			restoredBytes, err := json.Marshal(restoredMessages)
			require.NoError(t, err)
			require.Contains(t, string(restoredBytes), "principal-A-persisted-session-reply")
			restoredOutput, err := aAgain.TaskOutput(ctx, restored.ID, shellID, false, 0)
			require.NoError(t, err)
			require.Equal(t, managedtask.StatusCompleted, restoredOutput.Task.State.Status)
			require.Contains(t, restoredOutput.Output, outputMarker)
			require.EqualValues(t, 1, toolCalls.Load(), "restoring durable task history must not execute the shell again")
			require.Zero(t, wrongCredentials.Load())
			require.NoError(t, aAgain.RetireClient(ctx))
			require.Equal(t, protectedBefore, remoteOwnershipAcceptanceProtectedState(t), "history reuse, inference and retirement must not mutate protected server stores")
		})
	}
}

func remoteHistoryAcceptanceProposal(t *testing.T, endpoint string) config.RemoteRuntimeProposal {
	t.Helper()
	owner := providerregistry.RegistrationOwner{ProviderID: "history-fixture"}
	proposal := config.RemoteRuntimeProposal{
		Version: config.RemoteRuntimeVersion, Revision: 1,
		Providers: []config.RemoteProviderDefinition{{Config: config.ProviderConfig{
			ID: owner.ProviderID, Name: "History fixture", Type: catalog.TypeOpenAICompat, BaseURL: endpoint + "/v1",
			Owner:  &config.ProviderOwnerReference{Type: config.ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat},
			Models: []catalog.Model{{ID: "fixture", Name: "Fixture", ContextWindow: 8192, DefaultMaxTokens: 256}},
		}}},
		Models: map[config.SelectedModelType]config.SelectedModel{
			config.SelectedModelTypeLarge: {Provider: owner.ProviderID, Model: "fixture"},
			config.SelectedModelTypeSmall: {Provider: owner.ProviderID, Model: "fixture"},
		},
		Credentials: []config.RemoteCredentialBinding{{Owner: owner, Generation: 1, APIKey: "synthetic-history-key"}},
	}
	var err error
	proposal.Digest, err = config.RemoteRuntimeDigest(proposal)
	require.NoError(t, err)
	return proposal
}
