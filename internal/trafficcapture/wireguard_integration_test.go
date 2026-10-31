//go:build embedded_mitmproxy && darwin

package trafficcapture

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

const wireGuardProbeCommand = "__capture-wireguard-network-probe"

func init() {
	captureTestCommands[wireGuardProbeCommand] = runWireGuardNetworkProbe
}

func probeDirectConnection(endpoint string) bool {
	transport, err := captureTestTransport()
	if err != nil {
		fmt.Fprintln(os.Stderr, "network probe:", err)
		return false
	}
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	response, err := client.Get(endpoint)
	if err != nil {
		fmt.Fprintln(os.Stderr, "network probe:", err)
		return false
	}
	_, err = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	return err == nil && response.StatusCode == http.StatusOK
}

func runWireGuardNetworkProbe(arguments []string) int {
	if len(arguments) == 2 && arguments[0] == "child" {
		if probeDirectConnection(arguments[1]) {
			return 0
		}
		return 1
	}
	if len(arguments) != 4 {
		return 2
	}
	executable, err := os.Executable()
	if err != nil {
		return 3
	}
	probe := func(phase string) error {
		parent := probeDirectConnection(arguments[0] + "/" + phase + "-parent")
		child := exec.Command(executable, wireGuardProbeCommand, "child", arguments[0]+"/"+phase+"-child").Run() == nil
		return writePrivateJSON(arguments[2], map[string]any{"phase": phase, "parent": parent, "child": child, "proxy_environment": os.Getenv("HTTP_PROXY") + os.Getenv("HTTPS_PROXY")})
	}
	if err := probe("connected"); err != nil {
		return 4
	}
	for {
		if _, err := os.Stat(arguments[1]); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := probe("disconnected"); err != nil {
		return 5
	}
	providerProbed := false
	for {
		if _, err := os.Stat(arguments[3]); err == nil {
			return 0
		}
		if _, err := os.Stat(arguments[1] + ".provider"); err == nil && !providerProbed {
			if err := probe("provider-disconnected"); err != nil {
				return 6
			}
			providerProbed = true
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func nonLoopbackIPv4(t *testing.T) net.IP {
	t.Helper()
	interfaces, err := net.Interfaces()
	require.NoError(t, err)
	for _, networkInterface := range interfaces {
		if networkInterface.Flags&net.FlagUp == 0 || networkInterface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, err := networkInterface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err == nil && ip.To4() != nil && ip.IsGlobalUnicast() && !ip.IsLinkLocalUnicast() {
				return ip.To4()
			}
		}
	}
	t.Skip("a non-loopback IPv4 interface is required to distinguish routing from loopback exemptions")
	return nil
}

func captureViewerClient(t *testing.T, ctx context.Context, viewer string) (*http.Client, *url.URL) {
	t.Helper()
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	client := &http.Client{Transport: &http.Transport{}, Jar: jar, Timeout: 10 * time.Second}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, viewer, nil)
	require.NoError(t, err)
	response, err := client.Do(request)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, response.Body)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusOK, response.StatusCode)
	base, err := url.Parse(viewer)
	require.NoError(t, err)
	base.RawQuery = ""
	updates := *base
	updates.Scheme = "ws"
	updates.Path = "/updates"
	dialer := websocket.Dialer{Jar: jar, HandshakeTimeout: 10 * time.Second}
	connection, _, err := dialer.DialContext(ctx, updates.String(), nil)
	require.NoError(t, err)
	require.NoError(t, connection.Close())
	return client, base
}

func TestEmbeddedWireGuardExternalCapture(t *testing.T) {
	endpoint := "https://example.com/?crux-wireguard="
	require.True(t, probeDirectConnection(endpoint+"control"), "uncaptured external control must succeed")
	t.Setenv("CRUX_GLOBAL_DATA", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SHELL", "/bin/sh")
	working := t.TempDir()
	executable, err := os.Executable()
	require.NoError(t, err)
	resultPath := filepath.Join(working, "result.json")
	release := filepath.Join(working, "release")
	trigger := filepath.Join(working, "trigger")
	metadata, err := Launch(t.Context(), Request{
		Executable: executable, Arguments: []string{wireGuardProbeCommand, endpoint, trigger, resultPath, release},
		WorkingDir: working, CapturePath: filepath.Join(CaptureDirectory(), "external.mitm"), ManagedCapture: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = os.WriteFile(trigger, nil, 0o600)
		_ = os.WriteFile(release, nil, 0o600)
		_ = os.WriteFile(filepath.Join(filepath.Dir(metadata.StatusPath), "stop"), nil, 0o600)
		require.Eventually(t, func() bool {
			status, ok := readWorkerStatus(metadata.StatusPath)
			return ok && (status.State == "completed" || status.State == "failed")
		}, 25*time.Second, 100*time.Millisecond)
	})
	require.Eventually(t, func() bool { _, err := os.Stat(resultPath); return err == nil }, 15*time.Second, 100*time.Millisecond)
	result, err := os.ReadFile(resultPath)
	require.NoError(t, err)
	t.Logf("external probe: %s", result)
	t.Cleanup(func() {
		if t.Failed() {
			data, _ := os.ReadFile(metadata.TargetLogPath)
			t.Logf("target output: %s", data)
		}
	})
	client, viewer := captureViewerClient(t, t.Context(), metadata.ViewerURL)
	viewer.Path = "/flows"
	var flowResponse string
	success := false
	for attempt := 0; attempt < 20; attempt++ {
		response, err := client.Get(viewer.String())
		require.NoError(t, err)
		data, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		flowResponse = string(data)
		if bytes.Contains(data, []byte("connected-parent")) && bytes.Contains(data, []byte("connected-child")) {
			success = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	require.True(t, success, "external requests not captured: %s", flowResponse)
}

func TestEmbeddedWireGuardRoutesHelpersAndFailsClosed(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is unavailable")
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort(nonLoopbackIPv4(t).String(), "0"))
	require.NoError(t, err)
	var mu sync.Mutex
	seen := make(map[string]int)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		mu.Lock()
		seen[request.URL.Path]++
		mu.Unlock()
		_, _ = io.WriteString(response, "reachable")
	}))
	_ = server.Listener.Close()
	server.Listener = listener
	server.Start()
	defer server.Close()
	require.True(t, probeDirectConnection(server.URL+"/baseline-control"))
	t.Setenv("CRUX_GLOBAL_DATA", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SHELL", "/bin/sh")
	working := t.TempDir()
	trigger := filepath.Join(working, "disconnect")
	resultPath := filepath.Join(working, "result.json")
	release := filepath.Join(working, "release")
	executable, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	metadata, err := Launch(ctx, Request{
		Executable: executable, Arguments: []string{wireGuardProbeCommand, server.URL, trigger, resultPath, release},
		WorkingDir: working, CapturePath: filepath.Join(CaptureDirectory(), "enforcement.mitm"), ManagedCapture: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = os.WriteFile(release, nil, 0o600)
		_ = os.WriteFile(filepath.Join(filepath.Dir(metadata.StatusPath), "stop"), nil, 0o600)
		require.Eventually(t, func() bool {
			status, ok := readWorkerStatus(metadata.StatusPath)
			return ok && (status.State == "completed" || status.State == "failed")
		}, 25*time.Second, 100*time.Millisecond)
	})
	var result struct {
		Phase  string `json:"phase"`
		Parent bool   `json:"parent"`
		Child  bool   `json:"child"`
		Proxy  string `json:"proxy_environment"`
	}
	readPhase := func(phase string) bool {
		data, err := os.ReadFile(resultPath)
		return err == nil && json.Unmarshal(data, &result) == nil && result.Phase == phase
	}
	require.Eventually(t, func() bool { return readPhase("connected") }, 15*time.Second, 100*time.Millisecond)
	require.True(t, result.Parent, "parent could not connect through WireGuard")
	require.True(t, result.Child, "spawned helper could not connect through WireGuard")
	require.Empty(t, result.Proxy)
	client, viewer := captureViewerClient(t, ctx, metadata.ViewerURL)
	flowsURL := *viewer
	flowsURL.Path = "/flows"
	var flowResponse string
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("capture viewer response: %s", flowResponse)
		}
	})
	require.Eventually(t, func() bool {
		response, err := client.Get(flowsURL.String())
		if err != nil {
			return false
		}
		data, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		flowResponse = fmt.Sprintf("HTTP %d %s", response.StatusCode, data)
		return bytes.Contains(data, []byte("connected-parent")) && bytes.Contains(data, []byte("connected-child"))
	}, 5*time.Second, 100*time.Millisecond, "successful requests bypassed the capture viewer")
	optionsURL := *viewer
	optionsURL.Path = "/options"
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, optionsURL.String(), strings.NewReader(`{"mode":["regular"]}`))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	for _, cookie := range client.Jar.Cookies(viewer) {
		if cookie.Name == "_mitmproxy_xsrf" {
			request.Header.Set("X-XSRFToken", cookie.Value)
		}
	}
	response, err := client.Do(request)
	require.NoError(t, err)
	body, _ := io.ReadAll(response.Body)
	require.NoError(t, response.Body.Close())
	require.Less(t, response.StatusCode, 300, string(body))
	require.NoError(t, os.WriteFile(trigger, nil, 0o600))
	require.Eventually(t, func() bool { return readPhase("disconnected") }, 15*time.Second, 100*time.Millisecond)
	require.False(t, result.Parent, "parent bypassed the unavailable WireGuard endpoint")
	require.False(t, result.Child, "spawned helper bypassed the unavailable WireGuard endpoint")
	require.True(t, probeDirectConnection(server.URL+"/unaffected-control"), "uncaptured control lost access to the destination")
	mu.Lock()
	parentLeaks, childLeaks := seen["/disconnected-parent"], seen["/disconnected-child"]
	mu.Unlock()
	require.Zero(t, parentLeaks, "destination received a direct parent connection during capture failure")
	require.Zero(t, childLeaks, "destination received a direct helper connection during capture failure")
	providerPath := filepath.Join(filepath.Dir(metadata.StatusPath), "Capture.app", "Contents", "PlugIns", "CaptureTunnel.appex", "Contents", "MacOS", "CaptureTunnel")
	providerPath, err = filepath.EvalSymlinks(providerPath)
	require.NoError(t, err)
	pids, err := exec.Command("pgrep", "-f", regexp.QuoteMeta(providerPath)).Output()
	require.NoError(t, err)
	providerPIDs := strings.Fields(string(pids))
	require.Len(t, providerPIDs, 1)
	pid, err := strconv.Atoi(providerPIDs[0])
	require.NoError(t, err)
	process, err := os.FindProcess(pid)
	require.NoError(t, err)
	require.NoError(t, process.Kill())
	require.NoError(t, os.WriteFile(trigger+".provider", nil, 0o600))
	require.Eventually(t, func() bool { return readPhase("provider-disconnected") }, 15*time.Second, 100*time.Millisecond)
	require.False(t, result.Parent, "parent bypassed the unavailable packet provider")
	require.False(t, result.Child, "helper bypassed the unavailable packet provider")
	mu.Lock()
	parentLeaks, childLeaks = seen["/provider-disconnected-parent"], seen["/provider-disconnected-child"]
	mu.Unlock()
	require.Zero(t, parentLeaks)
	require.Zero(t, childLeaks)
	require.True(t, probeDirectConnection(server.URL+"/provider-unaffected-control"))
	t.Log("Verified capture of parent and helper traffic; both blocked after WireGuard server loss and packet-provider termination; uncaptured controls remained reachable")
	policy, err := os.ReadFile(filepath.Join(filepath.Dir(metadata.StatusPath), "policy-status.json"))
	require.NoError(t, err)
	require.Contains(t, string(policy), `"state":"connected"`)
}
