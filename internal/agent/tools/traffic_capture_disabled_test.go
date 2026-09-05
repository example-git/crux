//go:build !embedded_mitmproxy

package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/permission"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/stretchr/testify/require"
)

func TestTrafficCaptureWithoutEmbeddedRuntimeFailsBeforePermission(t *testing.T) {
	workingDir := t.TempDir()
	capturePath := filepath.Join(workingDir, "capture.mitm")
	permissions := &recordingPermissionService{
		Broker: pubsub.NewBroker[permission.PermissionRequest](),
		allow:  true,
	}
	tool := NewTrafficCaptureTool(permissions, workingDir)
	input, err := json.Marshal(TrafficCaptureParams{
		Executable:  "tool",
		CapturePath: capturePath,
	})
	require.NoError(t, err)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session")

	response, err := tool.Run(ctx, fantasy.ToolCall{
		ID:    "call",
		Name:  TrafficCaptureToolName,
		Input: string(input),
	})
	require.NoError(t, err)
	require.True(t, response.IsError)
	require.Contains(t, response.Content, "--embedded-mitmproxy")
	require.Zero(t, permissions.requestCount)
	require.NoFileExists(t, capturePath)
}
