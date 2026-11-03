//go:build !windows

package webclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/example-git/crux/internal/server"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func terminalPair(t *testing.T, command *exec.Cmd) *websocket.Conn {
	t.Helper()
	done := make(chan struct{})
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		connection, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		runTerminal(connection, command, &pty.Winsize{Cols: 80, Rows: 24})
	}))
	t.Cleanup(func() {
		host.Close()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("terminal handler did not finish")
		}
	})
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(host.URL, "http"), nil)
	require.NoError(t, err)
	t.Cleanup(func() { client.Close() })
	return client
}

func readTerminal(t *testing.T, client *websocket.Conn, until func(string, *terminalExit) bool) (string, *terminalExit) {
	t.Helper()
	var output strings.Builder
	require.NoError(t, client.SetReadDeadline(time.Now().Add(10*time.Second)))
	for {
		kind, data, err := client.ReadMessage()
		if err != nil {
			return output.String(), nil
		}
		if kind == websocket.BinaryMessage {
			output.Write(data)
			if until(output.String(), nil) {
				return output.String(), nil
			}
			continue
		}
		var exit terminalExit
		require.NoError(t, json.Unmarshal(data, &exit))
		require.Equal(t, "exit", exit.Type)
		return output.String(), &exit
	}
}

func TestTerminalRunsInteractiveInputAndReportsExit(t *testing.T) {
	client := terminalPair(t, exec.Command("/bin/sh", "-c", `stty size; read line; echo "got:$line"; exit 3`))
	output, _ := readTerminal(t, client, func(value string, _ *terminalExit) bool { return strings.Contains(value, "24 80") })
	require.Contains(t, output, "24 80")
	require.NoError(t, client.WriteMessage(websocket.BinaryMessage, []byte("hello\r")))
	output, exit := readTerminal(t, client, func(string, *terminalExit) bool { return false })
	require.Contains(t, output, "got:hello")
	require.NotNil(t, exit)
	require.Equal(t, 3, exit.Code)
}

func TestTerminalResizeReachesTheProcess(t *testing.T) {
	client := terminalPair(t, exec.Command("/bin/sh", "-c", `read line; stty size`))
	require.NoError(t, client.WriteMessage(websocket.TextMessage, []byte(`{"type":"resize","cols":132,"rows":40}`)))
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, client.WriteMessage(websocket.BinaryMessage, []byte("\r")))
	output, exit := readTerminal(t, client, func(string, *terminalExit) bool { return false })
	require.Contains(t, output, "40 132")
	require.NotNil(t, exit)
	require.Zero(t, exit.Code)
}

func TestTerminalDisconnectStopsTheProcessGroup(t *testing.T) {
	client := terminalPair(t, exec.Command("/bin/sh", "-c", `sleep 300 & echo "child:$!"; wait`))
	output, _ := readTerminal(t, client, func(value string, _ *terminalExit) bool { return strings.Contains(value, "\n") && strings.Contains(value, "child:") })
	fields := strings.Fields(strings.SplitN(output, "child:", 2)[1])
	require.NotEmpty(t, fields)
	child, err := strconv.Atoi(fields[0])
	require.NoError(t, err)
	require.NoError(t, client.Close())
	require.Eventually(t, func() bool { return syscall.Kill(child, 0) != nil }, 10*time.Second, 20*time.Millisecond)
}

func TestTerminalRejectsInvalidControlFrames(t *testing.T) {
	command := exec.Command("/bin/sh", "-c", `sleep 300`)
	client := terminalPair(t, command)
	require.NoError(t, client.WriteMessage(websocket.TextMessage, []byte(`{"type":"resize","cols":0,"rows":40}`)))
	_, exit := readTerminal(t, client, func(string, *terminalExit) bool { return false })
	require.NotNil(t, exit)
}

func TestTerminalSizeValidation(t *testing.T) {
	size, err := terminalSize(url.Values{})
	require.NoError(t, err)
	require.Equal(t, uint16(80), size.Cols)
	require.Equal(t, uint16(24), size.Rows)
	size, err = terminalSize(url.Values{"cols": {"120"}, "rows": {"50"}})
	require.NoError(t, err)
	require.Equal(t, uint16(120), size.Cols)
	for _, query := range []url.Values{{"cols": {"1"}}, {"rows": {"1001"}}, {"cols": {"wide"}}} {
		_, err := terminalSize(query)
		require.Error(t, err, query)
	}
}

func TestTerminalEnvironmentAdvertisesColorTerminal(t *testing.T) {
	environment := terminalEnvironment([]string{"TERM=dumb", "COLORTERM=no", "HOME=/home/user"})
	require.Equal(t, []string{"HOME=/home/user", "TERM=xterm-256color", "COLORTERM=truecolor"}, environment)
}

func TestTerminalRefusesCrossOriginUpgrades(t *testing.T) {
	handler := Handler(server.NewServer(nil, "tcp", "127.0.0.1:9090"), "127.0.0.1:9090", Startup{})
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9090/ui/workspaces/any/terminal", nil)
	request.Header.Set("Origin", "https://attacker.invalid")
	request.Header.Set("Upgrade", "websocket")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	require.Equal(t, http.StatusForbidden, response.Code)
}
