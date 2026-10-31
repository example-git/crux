package localaddon

import (
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/example-git/crux/internal/compatibility"
	"github.com/example-git/crux/internal/proto"
	"github.com/stretchr/testify/require"
)

type failOnRead struct{}

func (failOnRead) Read([]byte) (int, error) {
	panic("unexpected read")
}

func TestCodexAppServerRejectsNonClosableInputBeforeScanning(t *testing.T) {
	input := failOnRead{}
	err := runCodexAppServerNative(t.Context(), compatibility.Invocation{Stdout: io.Discard}, compatibility.Request{
		Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: input},
	})
	require.EqualError(t, err, "Codex app-server stdin must be closable")
}

func TestCodexChildServerHelper(t *testing.T) {
	if os.Getenv("CRUX_TEST_CODEX_CHILD_SERVER") != "1" {
		return
	}
	select {}
}

func TestCodexNativeBridgeCloseReapsChildServer(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestCodexChildServerHelper$")
	cmd.Env = append(os.Environ(), "CRUX_TEST_CODEX_CHILD_SERVER=1")
	require.NoError(t, cmd.Start())
	serverDone := make(chan error, 1)
	go func() { serverDone <- cmd.Wait() }()
	bridge := &codexNativeBridge{server: cmd, serverDone: serverDone}

	require.NoError(t, bridge.Close())
	require.NotNil(t, cmd.ProcessState)
}

func TestNativeTurnDrainsTerminalEventAfterOutputFailure(t *testing.T) {
	const completionDelay = 250 * time.Millisecond
	workingDir := nativeAPIFixtureWithSessionAndTaskSeedsAndFileSearch(
		t,
		proto.RunComplete{Text: "chunk"},
		completionDelay,
		true,
		[]nativeSessionSeed{{id: "thread-1"}},
		nil,
		nil,
		nil,
		nil,
		nil,
		func(_ http.ResponseWriter, _ *http.Request, request proto.AgentMessage, emit func(proto.Message)) bool {
			emit(proto.Message{
				ID:        "message-1",
				SessionID: request.SessionID,
				Role:      proto.Assistant,
				Parts:     []proto.ContentPart{proto.TextContent{Text: "chunk"}},
			})
			return false
		},
		nil,
	)
	request := compatibility.Request{
		Source:     "claude",
		WorkingDir: workingDir,
		Session:    compatibility.Session{Mode: compatibility.SessionExplicit, ID: "thread-1", Persistent: true},
	}
	bridge, err := newNativeCompatBridge(t.Context(), protocolInvocation("unused", workingDir, nil, io.Discard), request)
	require.NoError(t, err)
	defer func() { require.NoError(t, bridge.Close()) }()
	session, err := bridge.resolveSession(t.Context(), request)
	require.NoError(t, err)
	writeErr := errors.New("stream output failed")
	started := time.Now()
	result := bridge.runTurn(t.Context(), session, "hello", proto.AgentPermissionInteractive, func(string) error {
		return writeErr
	})

	require.ErrorIs(t, result.Err, writeErr)
	require.GreaterOrEqual(t, time.Since(started), completionDelay-50*time.Millisecond)
}
