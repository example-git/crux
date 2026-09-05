package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/permission"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/example-git/crux/internal/trafficcapture"
	"github.com/stretchr/testify/require"
)

func TestTrafficCaptureRejectsSubagentBeforeLaunch(t *testing.T) {
	workingDir := t.TempDir()
	permissions := &recordingPermissionService{
		Broker: pubsub.NewBroker[permission.PermissionRequest](),
		allow:  true,
	}
	tool := NewTrafficCaptureTool(permissions, workingDir)
	input, err := json.Marshal(TrafficCaptureParams{Executable: "tool"})
	require.NoError(t, err)
	ctx := context.WithValue(permission.WithSubagent(t.Context()), SessionIDContextKey, "child-session")

	response, err := tool.Run(ctx, fantasy.ToolCall{ID: "capture-call", Name: TrafficCaptureToolName, Input: string(input)})
	require.NoError(t, err)
	require.True(t, response.IsError)
	require.Equal(t, permission.ErrSubagentBackgroundTask.Error(), response.Content)
	require.Zero(t, permissions.requestCount)
}

func TestPrepareTrafficCaptureParamsValidatesTargetAndPaths(t *testing.T) {
	workingDir := t.TempDir()
	globalData := t.TempDir()
	t.Setenv("CRUX_GLOBAL_DATA", globalData)

	_, err := prepareTrafficCaptureParams(workingDir, TrafficCaptureParams{})
	require.ErrorContains(t, err, "exactly one")
	_, err = prepareTrafficCaptureParams(workingDir, TrafficCaptureParams{Executable: "tool", PID: 12})
	require.ErrorContains(t, err, "exactly one")
	_, err = prepareTrafficCaptureParams(workingDir, TrafficCaptureParams{PID: 12, Arguments: []string{"arg"}})
	require.ErrorContains(t, err, "only valid with executable")
	_, err = prepareTrafficCaptureParams(workingDir, TrafficCaptureParams{Executable: "tool", UnsetEnv: []string{"BAD-NAME"}})
	require.ErrorContains(t, err, "invalid environment variable")
	_, err = prepareTrafficCaptureParams(workingDir, TrafficCaptureParams{Executable: "tool", CapturePath: "capture.txt"})
	require.ErrorContains(t, err, "must end in .mitm")

	prepared, err := prepareTrafficCaptureParams(workingDir, TrafficCaptureParams{Executable: "tool"})
	require.NoError(t, err)
	canonicalWorkingDir, err := canonicalToolPath(workingDir, ".")
	require.NoError(t, err)
	require.Equal(t, canonicalWorkingDir, prepared.WorkingDir)
	require.False(t, prepared.workingDirExplicit)
	require.Equal(t, ".mitm", filepath.Ext(prepared.CapturePath))
	require.True(t, prepared.managedCapture)
	canonicalCaptureDirectory, err := canonicalToolPath(workingDir, filepath.Join(globalData, "traffic-capture", "captures"))
	require.NoError(t, err)
	require.Equal(t, canonicalCaptureDirectory, filepath.Dir(prepared.CapturePath))
	require.NotContains(t, prepared.CapturePath, canonicalWorkingDir)
	require.NoFileExists(t, prepared.CapturePath)
}

func TestPrepareTrafficCaptureParamsTracksExplicitWorkingDirectory(t *testing.T) {
	workingDir := t.TempDir()
	explicit := t.TempDir()

	prepared, err := prepareTrafficCaptureParams(workingDir, TrafficCaptureParams{
		Executable: "tool",
		WorkingDir: explicit,
	})
	require.NoError(t, err)
	require.True(t, prepared.workingDirExplicit)
	canonicalExplicit, err := canonicalToolPath(workingDir, explicit)
	require.NoError(t, err)
	require.Equal(t, canonicalExplicit, prepared.WorkingDir)
}

func TestTrafficCaptureResponseExposesUsableViewerURL(t *testing.T) {
	viewerURL := "http://127.0.0.1:8081/?token=plain-user-token"
	result, metadata := trafficCaptureResponse(trafficcapture.Metadata{
		Session:     "capture-session",
		CapturePath: "/private/capture.mitm",
		StatusPath:  "/private/status.json",
		PaneLogPath: "/private/pane.log",
		ViewerURL:   viewerURL,
	})

	require.Contains(t, result, "Viewer: "+viewerURL)
	require.Equal(t, viewerURL, metadata.ViewerURL)
}

func TestTrafficCaptureDescriptionUsesEmbeddedRuntime(t *testing.T) {
	require.Contains(t, trafficCaptureDescription, "embedded mitmproxy runtime")
	require.NotContains(t, trafficCaptureDescription, "no_install")
	require.NotContains(t, trafficCaptureDescription, "mitmdump_path")
}
