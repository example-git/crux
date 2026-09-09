package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/agent/prompt"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/message"
	"github.com/stretchr/testify/require"
)

func TestCoordinatorAuxiliaryHTTPSRequestsCancelAndJoin(t *testing.T) {
	for _, purpose := range []string{"title", "memory_extraction", "suggestion", "summary"} {
		t.Run(purpose, func(t *testing.T) {
			for _, key := range []string{"HOME", "AI_CLI_DIR", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME"} {
				t.Setenv(key, t.TempDir())
			}
			environment := testEnv(t)
			entered := make(chan struct{}, 1)
			var calls, canceled atomic.Int32
			host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer synthetic-auxiliary-key" || r.Header.Get("x-request-purpose") != purpose {
					http.Error(w, "unexpected auxiliary request", http.StatusBadRequest)
					return
				}
				calls.Add(1)
				entered <- struct{}{}
				<-r.Context().Done()
				canceled.Add(1)
			}))
			t.Cleanup(func() { host.CloseClientConnections(); host.Close() })
			previousClient := http.DefaultClient
			http.DefaultClient = host.Client()
			t.Cleanup(func() { http.DefaultClient = previousClient })
			configuration := fmt.Sprintf(`{
  "options":{"disable_default_providers":true,"disable_auto_summarize":true},
  "providers":{"fixture":{"id":"fixture","name":"Fixture","type":"openai-compat",
    "base_url":%q,"api_key":"synthetic-auxiliary-key",
    "models":[{"id":"fixture","name":"Fixture","context_window":8192,"default_max_tokens":128}]}},
  "models":{"large":{"provider":"fixture","model":"fixture"},"small":{"provider":"fixture","model":"fixture"}}
}`, host.URL+"/v1")
			require.NoError(t, os.WriteFile(filepath.Join(environment.workingDir, "crux.json"), []byte(configuration), 0o600))
			store := initTestConfig(t, environment.workingDir)
			store.SetupAgents()
			coord := &coordinator{cfg: store, sessions: environment.sessions, messages: environment.messages,
				permissions: environment.permissions, history: environment.history, filetracker: *environment.filetracker}
			t.Cleanup(func() {
				host.CloseClientConnections()
				coord.CloseContext(context.Background())
			})
			template, err := coderPrompt(prompt.WithWorkingDir(environment.workingDir))
			require.NoError(t, err)
			built, err := coord.buildAgent(t.Context(), template, store.Config().Agents[config.AgentCoder], false)
			require.NoError(t, err)
			require.NoError(t, coord.readyWg.Wait())
			coord.currentAgent = built
			sess, err := environment.sessions.Create(t.Context(), "Auxiliary lifetime")
			require.NoError(t, err)
			_, err = environment.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
				Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "A disposable conversation to summarize or continue."}},
			})
			require.NoError(t, err)
			caller, cancelCaller := context.WithCancel(t.Context())
			defer cancelCaller()
			finished := make(chan error, 1)
			go func() {
				switch purpose {
				case "title":
					// The production automatic-title path uses this insulation;
					// workspace ownership must remain effective after it is applied.
					built.GenerateTitle(context.WithoutCancel(caller), sess.ID, "Disposable title")
					finished <- nil
				case "memory_extraction":
					_, err := built.GenerateMemory(caller, purpose, "Disposable memory", 64)
					finished <- err
				case "suggestion":
					_, err := built.SuggestPrompt(caller, sess.ID)
					finished <- err
				case "summary":
					finished <- built.Summarize(caller, sess.ID, nil, nil)
				}
			}()
			select {
			case <-entered:
			case err := <-finished:
				t.Fatalf("auxiliary operation returned before reaching real HTTPS: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("auxiliary operation did not reach real HTTPS")
			}
			if purpose == "title" {
				cancelCaller()
				coord.CancelAll()
				select {
				case <-finished:
					t.Fatal("ordinary prompt cancellation stopped its detached title")
				case <-time.After(50 * time.Millisecond):
				}
				require.Zero(t, canceled.Load())
			}
			if purpose == "title" || purpose == "memory_extraction" {
				store.RevokeRuntime()
			}
			closed := make(chan struct{})
			go func() { coord.CloseContext(context.Background()); close(closed) }()
			select {
			case <-closed:
			case <-time.After(5 * time.Second):
				t.Fatal("coordinator close did not cancel and join auxiliary HTTPS")
			}
			select {
			case err := <-finished:
				if purpose != "title" {
					require.ErrorIs(t, err, context.Canceled)
				}
			case <-time.After(time.Second):
				t.Fatal("close returned before the admitted auxiliary operation exited")
			}
			require.Eventually(t, func() bool { return canceled.Load() == 1 }, time.Second, time.Millisecond)
			require.Equal(t, int32(1), calls.Load(), "cancellation must not fall back to another model")
			if purpose == "title" {
				stored, err := environment.sessions.Get(t.Context(), sess.ID)
				require.NoError(t, err)
				require.Equal(t, DefaultSessionName, stored.Title, "title cleanup must finish before close returns")
			}
			_, err = built.GenerateMemory(t.Context(), "memory_extraction", "late request", 64)
			require.ErrorIs(t, err, context.Canceled)
			require.Equal(t, int32(1), calls.Load(), "closed coordinator must not admit new credential work")
		})
	}
}

func TestAuxiliaryCloseJoinsCleanupAndFencesAdmission(t *testing.T) {
	work := &auxiliaryWork{}
	ctx, finish, err := work.begin(t.Context())
	require.NoError(t, err)
	defer func() { finish() }()
	closed := make(chan struct{})
	go func() { work.close(); close(closed) }()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("close did not cancel the admitted context")
	}
	_, _, err = work.begin(t.Context())
	require.ErrorIs(t, err, context.Canceled)
	select {
	case <-closed:
		t.Fatal("close returned while admitted cleanup still held its ticket")
	default:
	}
	// The cleanup ticket is released once, including on assertion failure.
	finish()
	finish = func() {}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("close did not join after cleanup completed")
	}
}
