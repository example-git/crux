package tools

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/permission"
	"github.com/example-git/crux/internal/trafficcapture"
)

const TrafficCaptureToolName = "traffic_capture"

var environmentVariablePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

//go:embed traffic_capture.md
var trafficCaptureDescription string

type TrafficCaptureParams struct {
	Executable         string   `json:"executable,omitempty" description:"Executable path or PATH name to launch under capture; mutually exclusive with pid"`
	Arguments          []string `json:"arguments,omitempty" description:"Arguments passed directly to executable without shell parsing"`
	PID                int      `json:"pid,omitempty" description:"Existing process ID to relaunch under capture; mutually exclusive with executable"`
	WorkingDir         string   `json:"working_dir,omitempty" description:"Working directory for the relaunched target; defaults to the current project directory"`
	CapturePath        string   `json:"capture_path,omitempty" description:"New .mitm output file; defaults under Crux's private global data directory"`
	UnsetEnv           []string `json:"unset_env,omitempty" description:"Environment variable names to remove from the relaunched target"`
	Wait               bool     `json:"wait,omitempty" description:"Wait for the captured target to finish instead of returning after startup"`
	workingDirExplicit bool
	managedCapture     bool
}

type TrafficCapturePermissionsParams struct {
	Executable  string   `json:"executable,omitempty"`
	Arguments   []string `json:"arguments,omitempty"`
	PID         int      `json:"pid,omitempty"`
	WorkingDir  string   `json:"working_dir"`
	CapturePath string   `json:"capture_path"`
	UnsetEnv    []string `json:"unset_env,omitempty"`
	Wait        bool     `json:"wait,omitempty"`
}

type TrafficCaptureResponseMetadata struct {
	Session     string `json:"session"`
	CapturePath string `json:"capture_path"`
	StatusPath  string `json:"status_path"`
	PaneLogPath string `json:"pane_log_path"`
	Attach      string `json:"attach"`
	ViewerURL   string `json:"viewer_url,omitempty"`
}

func NewTrafficCaptureTool(permissions permission.Service, workingDir string) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		TrafficCaptureToolName,
		trafficCaptureDescription,
		func(ctx context.Context, params TrafficCaptureParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if permission.IsSubagent(ctx) {
				return fantasy.NewTextErrorResponse(permission.ErrSubagentBackgroundTask.Error()), nil
			}
			prepared, err := prepareTrafficCaptureParams(workingDir, params)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			if err := trafficcapture.EmbeddedRuntimeError(); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, errors.New("session ID is required for traffic capture")
			}
			granted, err := permissions.Request(ctx, permission.CreatePermissionRequest{
				SessionID:   sessionID,
				Path:        prepared.CapturePath,
				ToolCallID:  call.ID,
				ToolName:    TrafficCaptureToolName,
				Action:      "capture",
				Description: trafficCapturePermissionDescription(prepared),
				Params: TrafficCapturePermissionsParams{
					Executable:  prepared.Executable,
					Arguments:   prepared.Arguments,
					PID:         prepared.PID,
					WorkingDir:  prepared.WorkingDir,
					CapturePath: prepared.CapturePath,
					UnsetEnv:    prepared.UnsetEnv,
					Wait:        prepared.Wait,
				},
				RequireExplicitApproval: true,
				AllowDetachedPrompt:     true,
			})
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if !granted {
				return NewPermissionDeniedResponse(), nil
			}
			if err := validatePreparedTrafficCaptureParams(prepared); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			metadata, err := trafficcapture.Launch(ctx, trafficcapture.Request{
				Executable:         prepared.Executable,
				Arguments:          prepared.Arguments,
				PID:                prepared.PID,
				WorkingDir:         prepared.WorkingDir,
				WorkingDirExplicit: prepared.workingDirExplicit,
				CapturePath:        prepared.CapturePath,
				ManagedCapture:     prepared.managedCapture,
				UnsetEnv:           prepared.UnsetEnv,
				Wait:               prepared.Wait,
			})
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			result, responseMetadata := trafficCaptureResponse(metadata)
			return fantasy.WithResponseMetadata(
				fantasy.NewTextResponse(result),
				responseMetadata,
			), nil
		},
	)
}

func trafficCaptureResponse(metadata trafficcapture.Metadata) (string, TrafficCaptureResponseMetadata) {
	responseMetadata := TrafficCaptureResponseMetadata{
		Session:     metadata.Session,
		CapturePath: metadata.CapturePath,
		StatusPath:  metadata.StatusPath,
		PaneLogPath: metadata.PaneLogPath,
		Attach:      metadata.Attach,
		ViewerURL:   metadata.ViewerURL,
	}
	parts := []string{
		fmt.Sprintf("Traffic capture started in tmux session %s. Use the Crux tmux sessions menu to attach.", metadata.Session),
		"Capture: " + metadata.CapturePath,
		"Status: " + metadata.StatusPath,
		"Pane log: " + metadata.PaneLogPath,
	}
	if metadata.ViewerURL != "" {
		parts = append(parts, "Viewer: "+metadata.ViewerURL)
	}
	return strings.Join(parts, "\n"), responseMetadata
}

func prepareTrafficCaptureParams(workingDir string, params TrafficCaptureParams) (TrafficCaptureParams, error) {
	workingDirExplicit := params.WorkingDir != ""
	if (params.Executable == "") == (params.PID == 0) {
		return TrafficCaptureParams{}, errors.New("exactly one of executable or pid is required")
	}
	if params.PID < 0 {
		return TrafficCaptureParams{}, errors.New("pid must be greater than zero")
	}
	if params.PID != 0 && len(params.Arguments) > 0 {
		return TrafficCaptureParams{}, errors.New("arguments are only valid with executable")
	}
	for _, name := range params.UnsetEnv {
		if !environmentVariablePattern.MatchString(name) {
			return TrafficCaptureParams{}, fmt.Errorf("invalid environment variable name %q", name)
		}
	}
	if params.WorkingDir == "" {
		params.WorkingDir = workingDir
	}
	resolvedWorkingDir, err := canonicalToolPath(workingDir, params.WorkingDir)
	if err != nil {
		return TrafficCaptureParams{}, err
	}
	params.WorkingDir = resolvedWorkingDir
	params.workingDirExplicit = workingDirExplicit
	if params.CapturePath == "" {
		name := fmt.Sprintf("capture-%s-%d.mitm", time.Now().UTC().Format("20060102T150405.000000000Z"), os.Getpid())
		params.CapturePath = filepath.Join(trafficcapture.CaptureDirectory(), name)
		params.managedCapture = true
	}
	resolvedCapturePath, err := canonicalToolPath(params.WorkingDir, params.CapturePath)
	if err != nil {
		return TrafficCaptureParams{}, err
	}
	if filepath.Ext(resolvedCapturePath) != ".mitm" {
		return TrafficCaptureParams{}, errors.New("capture_path must end in .mitm")
	}
	params.CapturePath = resolvedCapturePath
	return params, nil
}

func validatePreparedTrafficCaptureParams(params TrafficCaptureParams) error {
	info, err := os.Stat(params.WorkingDir)
	if err != nil {
		return fmt.Errorf("access working directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("working directory is not a directory: %s", params.WorkingDir)
	}
	if _, err := os.Lstat(params.CapturePath); err == nil {
		return fmt.Errorf("capture file already exists: %s", params.CapturePath)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("access capture path: %w", err)
	}
	return nil
}

func trafficCapturePermissionDescription(params TrafficCaptureParams) string {
	if params.Executable != "" {
		return "Capture HTTPS traffic from executable: " + params.Executable
	}
	return fmt.Sprintf("Relaunch PID %d and capture its HTTPS traffic", params.PID)
}
