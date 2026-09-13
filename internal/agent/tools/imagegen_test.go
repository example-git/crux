package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/imagegen"
	"github.com/example-git/crux/internal/permission"
	"github.com/example-git/crux/internal/pubsub"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/stretchr/testify/require"
)

func TestImagegenToolReturnsImmediatelyWhileNativeJobRuns(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	manager, err := imagegen.NewJobManagerWithStore(t.TempDir(), nil, imagegen.JobManagerOptions{
		Executor: func(ctx context.Context, request imagegen.JobRequest) (*imagegen.Response, error) {
			started <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return &imagegen.Response{
				Data:     []imagegen.ImageData{{B64JSON: base64.StdEncoding.EncodeToString([]byte("image"))}},
				AuthMode: imagegen.AuthCodex,
				Model:    "gpt-image-test",
			}, nil
		},
		MaxConcurrent: 1,
		MaxQueued:     2,
	})
	require.NoError(t, err)
	t.Cleanup(func() { manager.StopAll(context.Background()) })
	permissions := &recordingPermissionService{
		Broker: pubsub.NewBroker[permission.PermissionRequest](),
		allow:  true,
	}
	workingDirectory := t.TempDir()
	tool := NewImagegenTool(manager, permissions, workingDirectory)
	output := filepath.Join(workingDirectory, "generated.png")
	resolvedOutput, err := canonicalToolPath(workingDirectory, output)
	require.NoError(t, err)
	input, err := json.Marshal(ImagegenParams{Mode: imagegen.ModeGenerate, Prompt: "A paper fox", Output: output})
	require.NoError(t, err)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "child-session")
	ctx = managedtask.WithOwnership(ctx, managedtask.Ownership{ParentSessionID: "parent-session", OwnerAgentTaskID: "a12345678"})
	type toolResult struct {
		response fantasy.ToolResponse
		err      error
	}
	resultChannel := make(chan toolResult, 1)
	go func() {
		response, runErr := tool.Run(ctx, fantasy.ToolCall{ID: "image-call", Name: ImagegenToolName, Input: string(input)})
		resultChannel <- toolResult{response: response, err: runErr}
	}()

	var result toolResult
	select {
	case result = <-resultChannel:
	case <-time.After(time.Second):
		t.Fatal("imagegen tool waited for background generation to finish")
	}
	require.NoError(t, result.err)
	require.False(t, result.response.IsError)
	require.Contains(t, result.response.Content, "Queued image generation as task i")
	require.NotContains(t, result.response.Content, "{")
	var metadata ImagegenResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(result.response.Metadata), &metadata))
	require.Contains(t, []managedtask.Status{managedtask.StatusPending, managedtask.StatusRunning}, metadata.Status)
	require.Equal(t, []string{resolvedOutput}, metadata.Outputs)
	require.Equal(t, 1, permissions.requestCount)
	require.Equal(t, ImagegenToolName, permissions.lastRequest.ToolName)

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("native image worker did not start")
	}
	close(release)
	jobResult, err := manager.Output(t.Context(), metadata.TaskID, true, time.Second)
	require.NoError(t, err)
	require.Equal(t, managedtask.StatusCompleted, jobResult.Task.State.Status)
}

type replacingImagePermissionService struct {
	*recordingPermissionService
	replace func()
}

func (s *replacingImagePermissionService) Request(ctx context.Context, request permission.CreatePermissionRequest) (bool, error) {
	s.replace()
	return s.recordingPermissionService.Request(ctx, request)
}

func TestImagegenToolRejectsInputReplacedDuringApproval(t *testing.T) {
	workingDir := t.TempDir()
	input := filepath.Join(workingDir, "input.png")
	require.NoError(t, os.WriteFile(input, []byte("approved fixture"), 0o600))
	executed := make(chan struct{}, 1)
	manager, err := imagegen.NewJobManagerWithStore(t.TempDir(), nil, imagegen.JobManagerOptions{
		Executor: func(context.Context, imagegen.JobRequest) (*imagegen.Response, error) {
			executed <- struct{}{}
			return nil, nil
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { manager.StopAll(context.Background()) })
	permissions := &replacingImagePermissionService{
		recordingPermissionService: &recordingPermissionService{allow: true},
		replace: func() {
			require.NoError(t, os.Rename(input, input+".original"))
			require.NoError(t, os.WriteFile(input, []byte("replacement fixture"), 0o600))
		},
	}
	params, err := json.Marshal(ImagegenParams{Mode: imagegen.ModeEdit, Prompt: "edit fixture", Images: []string{input}, Output: filepath.Join(workingDir, "output.png")})
	require.NoError(t, err)
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "session")
	response, err := NewImagegenTool(manager, permissions, workingDir).Run(ctx, fantasy.ToolCall{ID: "image-call", Name: ImagegenToolName, Input: string(params)})
	require.NoError(t, err)
	require.True(t, response.IsError)
	require.Contains(t, response.Content, "changed after capture")
	require.Equal(t, 1, permissions.requestCount)
	require.Empty(t, executed)
}

func TestImagegenToolRetainsApprovedOutputDirectory(t *testing.T) {
	for _, mode := range []string{"explicit", "numbered", "missing", "denied"} {
		t.Run(mode, func(t *testing.T) {
			workingDir := t.TempDir()
			directory := filepath.Join(workingDir, "outputs")
			require.NoError(t, os.Mkdir(directory, 0o700))
			outside := t.TempDir()
			manager, err := imagegen.NewJobManagerWithStore(t.TempDir(), nil, imagegen.JobManagerOptions{
				Executor: func(context.Context, imagegen.JobRequest) (*imagegen.Response, error) {
					return &imagegen.Response{Data: []imagegen.ImageData{{B64JSON: base64.StdEncoding.EncodeToString([]byte("generated fixture"))}}}, nil
				},
			})
			require.NoError(t, err)
			t.Cleanup(func() { manager.StopAll(context.Background()) })
			params := ImagegenParams{Mode: imagegen.ModeGenerate, Prompt: "fixture", Output: filepath.Join(directory, "image.png"), Force: true}
			name := "image.png"
			if mode == "numbered" {
				params.Output = ""
				params.OutputDirectory = directory
				name = "image_1.png"
			}
			if mode == "missing" || mode == "denied" {
				params.Output = filepath.Join(directory, "nested", "deeper", "image.png")
				name = filepath.Join("nested", "deeper", "image.png")
			}
			permissions := &replacingImagePermissionService{
				recordingPermissionService: &recordingPermissionService{allow: mode != "denied"},
				replace: func() {
					require.NoDirExists(t, filepath.Join(directory, "nested"))
					if mode != "denied" {
						require.NoError(t, os.Rename(directory, directory+"-original"))
						require.NoError(t, os.Symlink(outside, directory))
					}
				},
			}
			encoded, err := json.Marshal(params)
			require.NoError(t, err)
			ctx := context.WithValue(t.Context(), SessionIDContextKey, "session")
			response, err := NewImagegenTool(manager, permissions, workingDir).Run(ctx, fantasy.ToolCall{ID: "image-call", Name: ImagegenToolName, Input: string(encoded)})
			require.NoError(t, err)
			if mode == "denied" {
				require.True(t, response.IsError)
				require.NoDirExists(t, filepath.Join(directory, "nested"))
				return
			}
			require.False(t, response.IsError, response.Content)
			var metadata ImagegenResponseMetadata
			require.NoError(t, json.Unmarshal([]byte(response.Metadata), &metadata))
			result, err := manager.Output(t.Context(), metadata.TaskID, true, 2*time.Second)
			require.NoError(t, err)
			require.Equal(t, managedtask.StatusCompleted, result.Task.State.Status)
			content, err := os.ReadFile(filepath.Join(directory+"-original", name))
			require.NoError(t, err)
			require.Equal(t, "generated fixture", string(content))
			entries, err := os.ReadDir(outside)
			require.NoError(t, err)
			require.Empty(t, entries)
		})
	}
}

func TestImagegenToolQueuesOneJobForMultipleOutputs(t *testing.T) {
	executed := make(chan imagegen.JobRequest, 2)
	manager, err := imagegen.NewJobManagerWithStore(t.TempDir(), nil, imagegen.JobManagerOptions{
		Executor: func(_ context.Context, request imagegen.JobRequest) (*imagegen.Response, error) {
			executed <- request
			response := &imagegen.Response{AuthMode: imagegen.AuthCodex, Model: "gpt-image-test"}
			for range request.Count {
				response.Data = append(response.Data, imagegen.ImageData{B64JSON: base64.StdEncoding.EncodeToString([]byte("image"))})
			}
			return response, nil
		},
		MaxConcurrent: 1,
		MaxQueued:     2,
	})
	require.NoError(t, err)
	t.Cleanup(func() { manager.StopAll(context.Background()) })
	permissions := &recordingPermissionService{
		Broker: pubsub.NewBroker[permission.PermissionRequest](),
		allow:  true,
	}
	workingDirectory := t.TempDir()
	outputDirectory := filepath.Join(workingDirectory, "variants")
	require.NoError(t, os.MkdirAll(outputDirectory, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(outputDirectory, "image_1.jpg"), []byte("existing-1"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(outputDirectory, "image_3.jpg"), []byte("existing-3"), 0o644))
	resolvedOutputDirectory, err := canonicalToolPath(workingDirectory, outputDirectory)
	require.NoError(t, err)
	count := 3
	input, err := json.Marshal(ImagegenParams{
		Mode:            imagegen.ModeGenerate,
		Backend:         imagegen.BackendFlow,
		Prompt:          "Three paper fox variants",
		OutputDirectory: outputDirectory,
		N:               &count,
	})
	require.NoError(t, err)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "parent-session")
	response, err := NewImagegenTool(manager, permissions, workingDirectory).Run(ctx, fantasy.ToolCall{
		ID:    "image-call",
		Name:  ImagegenToolName,
		Input: string(input),
	})
	require.NoError(t, err)
	require.False(t, response.IsError)

	var metadata ImagegenResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(response.Metadata), &metadata))
	require.Equal(t, []string{
		filepath.Join(resolvedOutputDirectory, "image_2.jpg"),
		filepath.Join(resolvedOutputDirectory, "image_4.jpg"),
		filepath.Join(resolvedOutputDirectory, "image_5.jpg"),
	}, metadata.Outputs)
	request := <-executed
	require.Equal(t, imagegen.BackendFlow, request.Backend)
	require.Equal(t, count, request.Count)
	require.Equal(t, metadata.Outputs, request.OutputPaths)
	permissionParams, ok := permissions.lastRequest.Params.(ImagegenPermissionsParams)
	require.True(t, ok)
	require.Equal(t, imagegen.BackendFlow, permissionParams.Backend)
	require.Equal(t, permissionParams.Outputs, request.OutputPaths)
	result, err := manager.Output(t.Context(), metadata.TaskID, true, time.Second)
	require.NoError(t, err)
	require.Equal(t, managedtask.StatusCompleted, result.Task.State.Status)
	for _, output := range metadata.Outputs {
		require.FileExists(t, output)
	}
	first, err := os.ReadFile(filepath.Join(outputDirectory, "image_1.jpg"))
	require.NoError(t, err)
	require.Equal(t, []byte("existing-1"), first)
	third, err := os.ReadFile(filepath.Join(outputDirectory, "image_3.jpg"))
	require.NoError(t, err)
	require.Equal(t, []byte("existing-3"), third)
	select {
	case extra := <-executed:
		t.Fatalf("tool queued an extra image job: %+v", extra)
	default:
	}
}

func TestImagegenToolSharesOutputDirectoryAcrossMultiOutputJobs(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{}, 2)
	manager, err := imagegen.NewJobManagerWithStore(t.TempDir(), nil, imagegen.JobManagerOptions{
		Executor: func(ctx context.Context, request imagegen.JobRequest) (*imagegen.Response, error) {
			started <- request.OutputPaths[0]
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			response := &imagegen.Response{AuthMode: imagegen.AuthCodex, Model: "gpt-image-test"}
			for range request.Count {
				response.Data = append(response.Data, imagegen.ImageData{B64JSON: base64.StdEncoding.EncodeToString([]byte("image"))})
			}
			return response, nil
		},
		MaxConcurrent: 1,
		MaxQueued:     2,
	})
	require.NoError(t, err)
	t.Cleanup(func() { manager.StopAll(context.Background()) })
	permissions := &recordingPermissionService{
		Broker: pubsub.NewBroker[permission.PermissionRequest](),
		allow:  true,
	}
	workingDirectory := t.TempDir()
	outputDirectory := filepath.Join(workingDirectory, "variants")
	resolvedOutputDirectory, err := canonicalToolPath(workingDirectory, outputDirectory)
	require.NoError(t, err)
	tool := NewImagegenTool(manager, permissions, workingDirectory)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "parent-session")
	run := func(callID, prompt string) ImagegenResponseMetadata {
		t.Helper()
		count := 2
		input, marshalErr := json.Marshal(ImagegenParams{
			Mode:            imagegen.ModeGenerate,
			Prompt:          prompt,
			OutputDirectory: outputDirectory,
			N:               &count,
		})
		require.NoError(t, marshalErr)
		response, runErr := tool.Run(ctx, fantasy.ToolCall{ID: callID, Name: ImagegenToolName, Input: string(input)})
		require.NoError(t, runErr)
		require.False(t, response.IsError, response.Content)
		var metadata ImagegenResponseMetadata
		require.NoError(t, json.Unmarshal([]byte(response.Metadata), &metadata))
		return metadata
	}

	first := run("first-image-call", "first")
	require.Equal(t, []string{
		filepath.Join(resolvedOutputDirectory, "image_1.png"),
		filepath.Join(resolvedOutputDirectory, "image_2.png"),
	}, first.Outputs)
	select {
	case path := <-started:
		require.Equal(t, first.Outputs[0], path)
	case <-time.After(time.Second):
		t.Fatal("first image job did not start")
	}
	second := run("second-image-call", "second")
	require.Equal(t, []string{
		filepath.Join(resolvedOutputDirectory, "image_3.png"),
		filepath.Join(resolvedOutputDirectory, "image_4.png"),
	}, second.Outputs)

	release <- struct{}{}
	_, err = manager.Output(t.Context(), first.TaskID, true, time.Second)
	require.NoError(t, err)
	select {
	case path := <-started:
		require.Equal(t, second.Outputs[0], path)
	case <-time.After(time.Second):
		t.Fatal("second image job did not start")
	}
	release <- struct{}{}
	_, err = manager.Output(t.Context(), second.TaskID, true, time.Second)
	require.NoError(t, err)
	for _, output := range append(first.Outputs, second.Outputs...) {
		require.FileExists(t, output)
	}
	require.Equal(t, 2, permissions.requestCount)
}

func TestImagegenToolRejectsSubagentBeforeQueueing(t *testing.T) {
	manager, err := imagegen.NewJobManagerWithStore(t.TempDir(), nil, imagegen.JobManagerOptions{MaxConcurrent: 1, MaxQueued: 1})
	require.NoError(t, err)
	t.Cleanup(func() { manager.StopAll(context.Background()) })
	permissions := &recordingPermissionService{
		Broker: pubsub.NewBroker[permission.PermissionRequest](),
		allow:  true,
	}
	workingDirectory := t.TempDir()
	tool := NewImagegenTool(manager, permissions, workingDirectory)
	input, err := json.Marshal(ImagegenParams{
		Mode:   imagegen.ModeGenerate,
		Prompt: "A paper fox",
		Output: filepath.Join(workingDirectory, "generated.png"),
	})
	require.NoError(t, err)
	ctx := context.WithValue(permission.WithSubagent(t.Context()), SessionIDContextKey, "child-session")

	response, err := tool.Run(ctx, fantasy.ToolCall{ID: "image-call", Name: ImagegenToolName, Input: string(input)})
	require.NoError(t, err)
	require.True(t, response.IsError)
	require.Equal(t, permission.ErrSubagentBackgroundTask.Error(), response.Content)
	require.Zero(t, permissions.requestCount)
	require.Zero(t, manager.ActiveCount())
}

func TestImagegenToolDoesNotQueueWhenPermissionIsDenied(t *testing.T) {
	manager, err := imagegen.NewJobManagerWithStore(t.TempDir(), nil, imagegen.JobManagerOptions{MaxConcurrent: 1, MaxQueued: 1})
	require.NoError(t, err)
	t.Cleanup(func() { manager.StopAll(context.Background()) })
	permissions := &recordingPermissionService{
		Broker: pubsub.NewBroker[permission.PermissionRequest](),
		allow:  false,
	}
	workingDirectory := t.TempDir()
	tool := NewImagegenTool(manager, permissions, workingDirectory)
	input, err := json.Marshal(ImagegenParams{
		Mode:   imagegen.ModeGenerate,
		Prompt: "A paper fox",
		Output: filepath.Join(workingDirectory, "generated.png"),
	})
	require.NoError(t, err)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "parent-session")
	response, err := tool.Run(ctx, fantasy.ToolCall{ID: "image-call", Name: ImagegenToolName, Input: string(input)})
	require.NoError(t, err)
	require.True(t, response.IsError)
	require.Contains(t, response.Content, "denied")
	require.Zero(t, manager.ActiveCount())
}

func TestImagegenToolAuthorizesDanglingOutputTarget(t *testing.T) {
	manager, err := imagegen.NewJobManagerWithStore(t.TempDir(), nil, imagegen.JobManagerOptions{MaxConcurrent: 1, MaxQueued: 1})
	require.NoError(t, err)
	t.Cleanup(func() { manager.StopAll(context.Background()) })
	permissions := &recordingPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest](), allow: false}
	workspace := t.TempDir()
	outside, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	target := filepath.Join(outside, "generated.png")
	link := filepath.Join(workspace, "generated.png")
	require.NoError(t, os.Symlink(target, link))
	tool := NewImagegenTool(manager, permissions, workspace)
	input, err := json.Marshal(ImagegenParams{Mode: imagegen.ModeGenerate, Prompt: "A paper fox", Output: link, Force: true})
	require.NoError(t, err)
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "parent-session")
	response, err := tool.Run(ctx, fantasy.ToolCall{ID: "image-call", Name: ImagegenToolName, Input: string(input)})
	require.NoError(t, err)
	require.Contains(t, response.Content, "denied")
	require.Equal(t, 1, permissions.requestCount)
	require.Equal(t, target, permissions.lastRequest.Path)
	params, ok := permissions.lastRequest.Params.(ImagegenPermissionsParams)
	require.True(t, ok)
	require.Equal(t, []string{target}, params.Outputs)
	require.Zero(t, manager.ActiveCount())
	require.NoFileExists(t, target)
}
