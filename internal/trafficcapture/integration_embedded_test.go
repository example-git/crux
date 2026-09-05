//go:build embedded_mitmproxy && (darwin || linux)

package trafficcapture

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const embeddedTestTargetCommand = "__traffic-capture-test-target"

func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "__traffic-capture-worker":
			if err := RunWorker(os.Args[2]); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			os.Exit(0)
		case "__traffic-capture-pane-log":
			if err := WritePaneLog(os.Args[2], os.Stdin); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			os.Exit(0)
		case embeddedTestTargetCommand:
			os.Exit(runEmbeddedTestTarget(os.Args[2:]))
		}
	}
	os.Exit(m.Run())
}

func TestEmbeddedCaptureEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not available")
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write([]byte("captured"))
	}))
	defer server.Close()
	workingDir := t.TempDir()
	globalData := t.TempDir()
	t.Setenv("CRUX_GLOBAL_DATA", globalData)
	capturePath := filepath.Join(CaptureDirectory(), "capture.mitm")
	readyPath := filepath.Join(workingDir, "target-ready")
	releasePath := filepath.Join(workingDir, "target-release")
	executable, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	metadata, err := Launch(ctx, Request{
		Executable:         executable,
		Arguments:          []string{embeddedTestTargetCommand, server.URL, readyPath, releasePath},
		WorkingDir:         workingDir,
		WorkingDirExplicit: true,
		CapturePath:        capturePath,
		ManagedCapture:     true,
	})
	if err != nil {
		runtimePaths, _ := filepath.Glob(filepath.Join(runDirectory(), "crux-capture-*"))
		for _, runtimePath := range runtimePaths {
			data, _ := os.ReadFile(filepath.Join(runtimePath, "pane.log"))
			t.Logf("startup pane log:\n%s", data)
		}
	}
	require.NoError(t, err)
	require.Contains(t, metadata.CapturePath, globalData)
	require.Contains(t, metadata.StatusPath, globalData)
	require.Contains(t, metadata.PaneLogPath, globalData)
	require.NotContains(t, metadata.CapturePath, workingDir)
	require.NotContains(t, metadata.StatusPath, workingDir)
	require.NotContains(t, metadata.PaneLogPath, workingDir)
	defer func() {
		if t.Failed() {
			data, _ := os.ReadFile(metadata.PaneLogPath)
			t.Logf("pane log:\n%s", data)
		}
	}()
	require.Eventually(t, func() bool {
		_, err := os.Stat(readyPath)
		return err == nil
	}, 30*time.Second, 100*time.Millisecond)
	viewerURL, err := url.Parse(metadata.ViewerURL)
	require.NoError(t, err)
	require.NotEmpty(t, viewerURL.Query().Get("token"))
	bareViewerURL := *viewerURL
	bareViewerURL.RawQuery = ""
	statusCode, err := viewerStatus(ctx, bareViewerURL.String())
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, statusCode)
	wrongTokenURL := bareViewerURL
	wrongQuery := wrongTokenURL.Query()
	wrongQuery.Set("token", "wrong-token")
	wrongTokenURL.RawQuery = wrongQuery.Encode()
	statusCode, err = viewerStatus(ctx, wrongTokenURL.String())
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, statusCode)
	statusCode, err = viewerStatus(ctx, metadata.ViewerURL)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, statusCode)
	require.NoError(t, os.WriteFile(releasePath, nil, 0o600))
	require.Eventually(t, func() bool {
		status, ok := readWorkerStatus(metadata.StatusPath)
		return ok && status.State == "completed" && status.ExitCode == 0
	}, 30*time.Second, 100*time.Millisecond)
	status, ok := readWorkerStatus(metadata.StatusPath)
	require.True(t, ok)
	require.Empty(t, status.ViewerURL)
	info, err := os.Stat(capturePath)
	require.NoError(t, err)
	require.Positive(t, info.Size())
}

func viewerStatus(ctx context.Context, rawURL string) (int, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, err
	}
	response, err := (&http.Client{
		Transport: &http.Transport{},
		Jar:       jar,
		Timeout:   10 * time.Second,
	}).Do(request)
	if err != nil {
		return 0, err
	}
	_, copyErr := io.Copy(io.Discard, response.Body)
	closeErr := response.Body.Close()
	if copyErr != nil {
		return 0, copyErr
	}
	if closeErr != nil {
		return 0, closeErr
	}
	return response.StatusCode, nil
}

func runEmbeddedTestTarget(arguments []string) int {
	if len(arguments) != 3 {
		return 2
	}
	proxy, err := url.Parse(os.Getenv("HTTP_PROXY"))
	if err != nil || proxy.Scheme == "" || proxy.Host == "" {
		return 3
	}
	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(proxy)},
		Timeout:   10 * time.Second,
	}
	response, err := client.Get(arguments[0])
	if err != nil {
		return 4
	}
	body, err := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if err != nil || closeErr != nil || response.StatusCode != http.StatusOK || string(body) != "captured" {
		return 5
	}
	tlsResponse, err := client.Get("https://mitm.it/")
	if err != nil {
		return 6
	}
	_, copyErr := io.Copy(io.Discard, tlsResponse.Body)
	closeErr = tlsResponse.Body.Close()
	if copyErr != nil || closeErr != nil || tlsResponse.StatusCode != http.StatusOK {
		return 7
	}
	if err := os.WriteFile(arguments[1], nil, 0o600); err != nil {
		return 8
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(arguments[2]); err == nil {
			return 0
		}
		time.Sleep(100 * time.Millisecond)
	}
	return 9
}
