package imagegen

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/stretchr/testify/require"
)

func TestJobManagerRunsFourJobsAndQueuesRemainingFIFO(t *testing.T) {
	started := make(chan string, 6)
	release := make(chan struct{}, 6)
	var running atomic.Int32
	var maximum atomic.Int32
	executor := func(ctx context.Context, request JobRequest) (*Response, error) {
		current := running.Add(1)
		defer running.Add(-1)
		for {
			observed := maximum.Load()
			if current <= observed || maximum.CompareAndSwap(observed, current) {
				break
			}
		}
		started <- request.Prompt
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return testImageResponse(request.Model), nil
	}
	manager, err := NewJobManagerWithStore(t.TempDir(), nil, JobManagerOptions{
		Executor:      executor,
		MaxConcurrent: MaxConcurrentJobs,
		MaxQueued:     10,
	})
	require.NoError(t, err)
	t.Cleanup(func() { manager.StopAll(context.Background()) })

	outputDirectory := t.TempDir()
	views := make([]managedtask.View, 0, 6)
	for index := 1; index <= 6; index++ {
		request := testJobRequest(filepath.Join(outputDirectory, fmt.Sprintf("image_%d.png", index)), fmt.Sprintf("job-%d", index))
		view, enqueueErr := manager.Enqueue(request, request.Prompt, managedtask.Ownership{ParentSessionID: "parent"})
		require.NoError(t, enqueueErr)
		views = append(views, view)
	}

	first := make([]string, 0, 4)
	for range 4 {
		select {
		case prompt := <-started:
			first = append(first, prompt)
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for four running image jobs")
		}
	}
	require.ElementsMatch(t, []string{"job-1", "job-2", "job-3", "job-4"}, first)
	require.Equal(t, MaxConcurrentJobs, manager.RunningCount())
	select {
	case prompt := <-started:
		t.Fatalf("queued image job started before a slot was free: %s", prompt)
	case <-time.After(50 * time.Millisecond):
	}

	release <- struct{}{}
	select {
	case prompt := <-started:
		require.Equal(t, "job-5", prompt)
	case <-time.After(time.Second):
		t.Fatal("fifth image job did not start after a slot became free")
	}
	release <- struct{}{}
	select {
	case prompt := <-started:
		require.Equal(t, "job-6", prompt)
	case <-time.After(time.Second):
		t.Fatal("sixth image job did not start after the next slot became free")
	}
	for range 4 {
		release <- struct{}{}
	}

	for _, view := range views {
		result, outputErr := manager.Output(t.Context(), view.ID, true, 2*time.Second)
		require.NoError(t, outputErr)
		require.Equal(t, managedtask.StatusCompleted, result.Task.State.Status)
	}
	require.Equal(t, int32(MaxConcurrentJobs), maximum.Load())
}

func TestJobManagerExplicitOutputStillRejectsReservedFilePath(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	manager, err := NewJobManagerWithStore(t.TempDir(), nil, JobManagerOptions{
		Executor: func(ctx context.Context, request JobRequest) (*Response, error) {
			started <- struct{}{}
			select {
			case <-release:
				return testImageResponse(request.Model), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
		MaxConcurrent: 1,
		MaxQueued:     2,
	})
	require.NoError(t, err)
	t.Cleanup(func() { manager.StopAll(context.Background()) })
	output := filepath.Join(t.TempDir(), "explicit.png")
	first, err := manager.Enqueue(testJobRequest(output, "first"), "first", managedtask.Ownership{ParentSessionID: "parent"})
	require.NoError(t, err)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first image job did not start")
	}
	_, err = manager.Enqueue(testJobRequest(output, "second"), "second", managedtask.Ownership{ParentSessionID: "parent"})
	require.ErrorContains(t, err, "output path is already reserved by image job")
	close(release)
	_, err = manager.Output(t.Context(), first.ID, true, time.Second)
	require.NoError(t, err)
}

func TestJobManagerCancelsPendingJobWithoutExecutingIt(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	executor := func(ctx context.Context, request JobRequest) (*Response, error) {
		started <- request.Prompt
		select {
		case <-release:
			return testImageResponse(request.Model), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	manager, err := NewJobManagerWithStore(t.TempDir(), nil, JobManagerOptions{
		Executor:      executor,
		MaxConcurrent: 1,
		MaxQueued:     2,
	})
	require.NoError(t, err)
	t.Cleanup(func() { manager.StopAll(context.Background()) })

	outputDirectory := t.TempDir()
	first, err := manager.Enqueue(testJobRequest(filepath.Join(outputDirectory, "first.png"), "first"), "first", managedtask.Ownership{ParentSessionID: "parent"})
	require.NoError(t, err)
	second, err := manager.Enqueue(testJobRequest(filepath.Join(outputDirectory, "second.png"), "second"), "second", managedtask.Ownership{ParentSessionID: "parent"})
	require.NoError(t, err)
	select {
	case prompt := <-started:
		require.Equal(t, "first", prompt)
	case <-time.After(time.Second):
		t.Fatal("first image job did not start")
	}

	stopped, err := manager.Stop(t.Context(), second.ID)
	require.NoError(t, err)
	require.Equal(t, managedtask.StatusKilled, stopped.State.Status)
	select {
	case prompt := <-started:
		t.Fatalf("canceled pending image job executed: %s", prompt)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	_, err = manager.Output(t.Context(), first.ID, true, time.Second)
	require.NoError(t, err)
}

func TestJobManagerNotifiesOriginatingSessionWithStructuredResult(t *testing.T) {
	manager, err := NewJobManagerWithStore(t.TempDir(), nil, JobManagerOptions{
		Executor: func(context.Context, JobRequest) (*Response, error) {
			return testImageResponse("gpt-image-test"), nil
		},
		MaxConcurrent: 1,
		MaxQueued:     1,
	})
	require.NoError(t, err)
	t.Cleanup(func() { manager.StopAll(context.Background()) })
	notifications := manager.SubscribeNotifications(t.Context())
	output := filepath.Join(t.TempDir(), "image.png")
	view, err := manager.Enqueue(testJobRequest(output, "notify"), "notify", managedtask.Ownership{
		ParentSessionID:  "parent-session",
		OwnerAgentTaskID: "a12345678",
		OriginToolCallID: "image-call",
	})
	require.NoError(t, err)

	select {
	case event := <-notifications:
		notification := event.Payload
		require.Equal(t, view.ID, notification.TaskID)
		require.Equal(t, managedtask.TypeImage, notification.TaskType)
		require.Equal(t, "parent-session", notification.ParentSessionID)
		require.Equal(t, "image-call", notification.ToolUseID)
		require.Equal(t, managedtask.StatusCompleted, notification.Status)
		var result JobResult
		require.NoError(t, json.Unmarshal([]byte(notification.FinalOutput), &result))
		require.True(t, result.Success)
		require.Equal(t, []string{output}, result.Outputs)
		require.Equal(t, "gpt-image-test", result.Model)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for image job notification")
	}
}

func TestJobManagerCompletesWithSuccessfulVariantsWhenOneFails(t *testing.T) {
	manager, err := NewJobManagerWithStore(t.TempDir(), nil, JobManagerOptions{
		Executor: func(context.Context, JobRequest) (*Response, error) {
			return &Response{
				Data: []ImageData{
					{B64JSON: base64.StdEncoding.EncodeToString([]byte("second")), Variant: 2},
					{B64JSON: base64.StdEncoding.EncodeToString([]byte("third")), Variant: 3},
				},
				Failures: []ImageVariantFailure{{Variant: 1, Error: "generation rejected"}},
				AuthMode: AuthFlow,
				Model:    "Nano Banana Pro",
			}, nil
		},
		MaxConcurrent: 1,
		MaxQueued:     1,
	})
	require.NoError(t, err)
	t.Cleanup(func() { manager.StopAll(context.Background()) })

	directory := t.TempDir()
	outputs := []string{
		filepath.Join(directory, "image_1.jpg"),
		filepath.Join(directory, "image_2.jpg"),
		filepath.Join(directory, "image_3.jpg"),
	}
	view, err := manager.Enqueue(JobRequest{
		Mode:        ModeGenerate,
		Backend:     BackendFlow,
		Prompt:      "three variants",
		Count:       3,
		OutputPaths: outputs,
	}, "three variants", managedtask.Ownership{ParentSessionID: "parent"})
	require.NoError(t, err)

	output, err := manager.Output(t.Context(), view.ID, true, time.Second)
	require.NoError(t, err)
	require.Equal(t, managedtask.StatusCompleted, output.Task.State.Status)
	require.Empty(t, output.Task.State.ErrorMessage)
	require.Equal(t, "file:"+outputs[1], output.Task.OutputRef)
	require.NoFileExists(t, outputs[0])
	require.FileExists(t, outputs[1])
	require.FileExists(t, outputs[2])

	var result JobResult
	require.NoError(t, json.Unmarshal([]byte(output.Output), &result))
	require.True(t, result.Success)
	require.Equal(t, 3, result.Requested)
	require.Equal(t, outputs[1:], result.Outputs)
	require.Equal(t, []ImageVariantFailure{{Variant: 1, Error: "generation rejected"}}, result.Failures)
	require.Equal(t, "flow", result.AuthMode)
}

func TestJobManagerRecoveryMarksActiveJobLostAndPersistsNotification(t *testing.T) {
	workspace := t.TempDir()
	store, err := managedtask.NewStore(filepath.Join(t.TempDir(), "metadata"))
	require.NoError(t, err)
	output := filepath.Join(workspace, "recovered.png")
	require.NoError(t, store.Put(managedtask.Record{
		ID:          "i12345678",
		Type:        managedtask.TypeImage,
		Description: "recovered image",
		Ownership:   managedtask.Ownership{WorkspaceID: workspace, ParentSessionID: "parent", OriginToolCallID: "image-call"},
		State:       managedtask.StateToRecord(managedtask.State{Status: managedtask.StatusRunning, StartedAt: time.Now()}),
		OutputRef:   "file:" + output,
		Image: &managedtask.ImageRecord{
			Mode:        ModeGenerate,
			Backend:     string(BackendFlow),
			Prompt:      "recovered",
			Count:       1,
			OutputPaths: []string{output},
		},
	}))

	manager, err := NewJobManagerWithStore(workspace, store, JobManagerOptions{MaxConcurrent: 1, MaxQueued: 1})
	require.NoError(t, err)
	t.Cleanup(func() {
		manager.StopAll(context.Background())
		require.NoError(t, store.Close())
	})
	job, ok := manager.Get("i12345678")
	require.True(t, ok)
	info := job.Info()
	require.Equal(t, BackendFlow, job.Request.Backend)
	require.Equal(t, managedtask.StatusLost, info.State.Status)
	require.Contains(t, info.State.LostReason, "restarted")

	recovered, err := store.Get("i12345678")
	require.NoError(t, err)
	require.NotNil(t, recovered.Notification)
	require.Equal(t, string(BackendFlow), recovered.Image.Backend)
	require.Equal(t, managedtask.StatusLost, recovered.Notification.Status)
	require.Equal(t, "parent", recovered.Notification.ParentSessionID)
	require.Contains(t, recovered.Notification.FinalOutput, `"success":false`)
	require.False(t, recovered.Image.NotificationEmitted)
}

func TestJobManagerProductionCodexEditWritesRequestedOutputs(t *testing.T) {
	var requests atomic.Int64
	bodies := make(chan map[string]any, 3)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		bodies <- body
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(writer, `{"data":[{"b64_json":%q}]}`, base64.StdEncoding.EncodeToString([]byte("edited-image")))
	}))
	defer server.Close()
	codexBaseURLOverride = server.URL
	defer func() { codexBaseURLOverride = "" }()

	manager, err := NewJobManagerWithStore(t.TempDir(), nil, JobManagerOptions{
		ClientFactory: func() *Client {
			client := NewClient()
			client.authResolver = func(context.Context) (resolvedAuth, error) {
				return resolvedAuth{mode: AuthCodex, token: "codex-token", accountID: "account"}, nil
			}
			return client
		},
		MaxConcurrent: 1,
		MaxQueued:     1,
	})
	require.NoError(t, err)
	t.Cleanup(func() { manager.StopAll(context.Background()) })
	input := filepath.Join(t.TempDir(), "input.png")
	require.NoError(t, os.WriteFile(input, []byte("input-image"), 0o600))
	outputDirectory := t.TempDir()
	outputs := []string{
		filepath.Join(outputDirectory, "image_1.png"),
		filepath.Join(outputDirectory, "image_2.png"),
		filepath.Join(outputDirectory, "image_3.png"),
	}
	view, err := manager.Enqueue(JobRequest{
		Mode:        ModeEdit,
		Prompt:      "create three background variants",
		Count:       3,
		InputPaths:  []string{input},
		OutputPaths: outputs,
	}, "edit three images", managedtask.Ownership{ParentSessionID: "parent"})
	require.NoError(t, err)

	result, err := manager.Output(t.Context(), view.ID, true, 2*time.Second)
	require.NoError(t, err)
	require.Equal(t, managedtask.StatusCompleted, result.Task.State.Status)
	require.Equal(t, int64(3), requests.Load())
	for range 3 {
		body := <-bodies
		_, hasCount := body["n"]
		require.False(t, hasCount)
		require.Equal(t, "create three background variants", body["prompt"])
	}
	for _, output := range outputs {
		written, readErr := os.ReadFile(output)
		require.NoError(t, readErr)
		require.Equal(t, []byte("edited-image"), written)
	}
}

func TestJobManagerProductionEditPath(t *testing.T) {
	t.Setenv("AI_CLI_DIR", t.TempDir())
	t.Setenv(openAIAPIKeyEnv, "environment-openai")
	type receivedRequest struct {
		authorization string
		path          string
		prompt        string
		model         string
		image         string
	}
	received := make(chan receivedRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := request.ParseMultipartForm(1 << 20); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		file, _, err := request.FormFile("image[]")
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		data, err := io.ReadAll(file)
		_ = file.Close()
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		received <- receivedRequest{
			authorization: request.Header.Get("Authorization"),
			path:          request.URL.Path,
			prompt:        request.FormValue("prompt"),
			model:         request.FormValue("model"),
			image:         string(data),
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(writer, `{"data":[{"b64_json":%q}]}`, base64.StdEncoding.EncodeToString([]byte("edited-image")))
	}))
	defer server.Close()
	store := config.NewTestStore(&config.Config{
		Providers: csync.NewMapFrom(map[string]config.ProviderConfig{
			"openai": {APIKey: "configured-openai", BaseURL: server.URL},
		}),
	})

	manager, err := NewJobManagerWithStore(t.TempDir(), nil, JobManagerOptions{
		ClientFactory: func() *Client { return NewProviderClient(store) },
		MaxConcurrent: 1,
		MaxQueued:     1,
	})
	require.NoError(t, err)
	t.Cleanup(func() { manager.StopAll(context.Background()) })
	input := filepath.Join(t.TempDir(), "input.png")
	require.NoError(t, os.WriteFile(input, []byte("input-image"), 0o600))
	output := filepath.Join(t.TempDir(), "edited.png")
	view, err := manager.Enqueue(JobRequest{
		Mode:        ModeEdit,
		Prompt:      "change only the background",
		Count:       1,
		InputPaths:  []string{input},
		OutputPaths: []string{output},
	}, "edit image", managedtask.Ownership{ParentSessionID: "parent"})
	require.NoError(t, err)

	result, err := manager.Output(t.Context(), view.ID, true, 2*time.Second)
	require.NoError(t, err)
	require.Equal(t, managedtask.StatusCompleted, result.Task.State.Status)
	request := <-received
	require.Equal(t, "Bearer configured-openai", request.authorization)
	require.Equal(t, "/images/edits", request.path)
	require.Equal(t, "change only the background", request.prompt)
	require.Equal(t, "gpt-image-1", request.model)
	require.Equal(t, "input-image", request.image)
	written, err := os.ReadFile(output)
	require.NoError(t, err)
	require.Equal(t, []byte("edited-image"), written)
}

func TestWriteJobImagesStreamsExactOutputsAndClearsResponseData(t *testing.T) {
	directory := t.TempDir()
	outputs := []string{
		filepath.Join(directory, "image_1.png"),
		filepath.Join(directory, "image_2.png"),
		filepath.Join(directory, "image_3.png"),
	}
	payloads := [][]byte{
		bytes.Repeat([]byte("first"), 1<<18),
		bytes.Repeat([]byte("second"), 1<<18),
		bytes.Repeat([]byte("third"), 1<<18),
	}
	response := &Response{Data: make([]ImageData, len(payloads))}
	for index, payload := range payloads {
		response.Data[index].B64JSON = base64.StdEncoding.EncodeToString(payload)
	}

	writtenOutputs, err := writeJobImages(t.Context(), JobRequest{OutputPaths: outputs}, response)
	require.NoError(t, err)
	require.Equal(t, outputs, writtenOutputs)
	for index, output := range outputs {
		written, readErr := os.ReadFile(output)
		require.NoError(t, readErr)
		require.Equal(t, payloads[index], written)
		require.Empty(t, response.Data[index].B64JSON)
	}
	entries, err := os.ReadDir(directory)
	require.NoError(t, err)
	for _, entry := range entries {
		require.NotContains(t, entry.Name(), ".crux-image-")
	}
}

func TestWriteJobImagesKeepsSuccessfulForcedOutputs(t *testing.T) {
	directory := t.TempDir()
	outputs := []string{
		filepath.Join(directory, "image_1.png"),
		filepath.Join(directory, "image_2.png"),
	}
	for _, output := range outputs {
		require.NoError(t, os.WriteFile(output, []byte("original"), 0o644))
	}
	response := &Response{Data: []ImageData{
		{B64JSON: base64.StdEncoding.EncodeToString([]byte("replacement"))},
		{B64JSON: "not-base64"},
	}}

	writtenOutputs, err := writeJobImages(t.Context(), JobRequest{OutputPaths: outputs, Force: true}, response)
	require.NoError(t, err)
	require.Equal(t, []string{outputs[0]}, writtenOutputs)
	require.Equal(t, []ImageVariantFailure{{Variant: 2, Error: "decode image data: illegal base64 data at input byte 3"}}, response.Failures)
	for index, output := range outputs {
		written, readErr := os.ReadFile(output)
		require.NoError(t, readErr)
		if index == 0 {
			require.Equal(t, []byte("replacement"), written)
		} else {
			require.Equal(t, []byte("original"), written)
		}
		require.Empty(t, response.Data[index].B64JSON)
	}
	entries, err := os.ReadDir(directory)
	require.NoError(t, err)
	for _, entry := range entries {
		require.NotContains(t, entry.Name(), ".crux-image-")
	}
}

func TestWriteJobImagesHandlesCancellationAndPartialConflict(t *testing.T) {
	t.Run("canceled", func(t *testing.T) {
		directory := t.TempDir()
		output := filepath.Join(directory, "image.png")
		response := testImageResponse("gpt-image-test")
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		writtenOutputs, err := writeJobImages(ctx, JobRequest{OutputPaths: []string{output}}, response)
		require.ErrorIs(t, err, context.Canceled)
		require.Empty(t, writtenOutputs)
		require.NoFileExists(t, output)
		require.Empty(t, response.Data[0].B64JSON)
		entries, readErr := os.ReadDir(directory)
		require.NoError(t, readErr)
		require.Empty(t, entries)
	})

	t.Run("conflict", func(t *testing.T) {
		directory := t.TempDir()
		first := filepath.Join(directory, "first.png")
		second := filepath.Join(directory, "second.png")
		require.NoError(t, os.WriteFile(second, []byte("existing"), 0o644))
		response := &Response{Data: []ImageData{
			{B64JSON: base64.StdEncoding.EncodeToString([]byte("first"))},
			{B64JSON: base64.StdEncoding.EncodeToString([]byte("second"))},
		}}

		writtenOutputs, err := writeJobImages(t.Context(), JobRequest{OutputPaths: []string{first, second}}, response)
		require.NoError(t, err)
		require.Equal(t, []string{first}, writtenOutputs)
		require.Equal(t, []ImageVariantFailure{{Variant: 2, Error: "output already exists: " + second}}, response.Failures)
		require.FileExists(t, first)
		written, readErr := os.ReadFile(second)
		require.NoError(t, readErr)
		require.Equal(t, []byte("existing"), written)
		for _, image := range response.Data {
			require.Empty(t, image.B64JSON)
		}
		entries, readErr := os.ReadDir(directory)
		require.NoError(t, readErr)
		for _, entry := range entries {
			require.NotContains(t, entry.Name(), ".crux-image-")
		}
	})
}

func TestJobManagerCoalescesMemoryReleaseAfterAllExecutionsFinish(t *testing.T) {
	firstRelease := make(chan struct{})
	secondRelease := make(chan struct{})
	started := make(chan string, 2)
	responses := make(chan *Response, 2)
	executor := func(ctx context.Context, request JobRequest) (*Response, error) {
		response := testImageResponse(request.Model)
		responses <- response
		started <- request.Prompt
		var release <-chan struct{}
		if request.Prompt == "first" {
			release = firstRelease
		} else {
			release = secondRelease
		}
		select {
		case <-release:
			return response, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	manager, err := NewJobManagerWithStore(t.TempDir(), nil, JobManagerOptions{
		Executor:      executor,
		MaxConcurrent: 2,
		MaxQueued:     2,
	})
	require.NoError(t, err)
	t.Cleanup(func() { manager.StopAll(context.Background()) })
	memoryReleased := make(chan struct{})
	var releases atomic.Int32
	manager.mu.Lock()
	manager.releaseMemory = func() {
		if releases.Add(1) == 1 {
			close(memoryReleased)
		}
	}
	manager.mu.Unlock()

	directory := t.TempDir()
	first, err := manager.Enqueue(testJobRequest(filepath.Join(directory, "first.png"), "first"), "first", managedtask.Ownership{ParentSessionID: "parent"})
	require.NoError(t, err)
	second, err := manager.Enqueue(testJobRequest(filepath.Join(directory, "second.png"), "second"), "second", managedtask.Ownership{ParentSessionID: "parent"})
	require.NoError(t, err)
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for image execution")
		}
	}
	close(firstRelease)
	_, err = manager.Output(t.Context(), first.ID, true, time.Second)
	require.NoError(t, err)
	require.Equal(t, int32(0), releases.Load())
	close(secondRelease)
	_, err = manager.Output(t.Context(), second.ID, true, time.Second)
	require.NoError(t, err)
	select {
	case <-memoryReleased:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for idle image memory release")
	}
	require.Equal(t, int32(1), releases.Load())
	for range 2 {
		response := <-responses
		require.Empty(t, response.Data[0].B64JSON)
	}
}

func TestJobManagerDoesNotStartExecutionDuringMemoryRelease(t *testing.T) {
	started := make(chan string, 2)
	executor := func(_ context.Context, request JobRequest) (*Response, error) {
		started <- request.Prompt
		return testImageResponse(request.Model), nil
	}
	manager, err := NewJobManagerWithStore(t.TempDir(), nil, JobManagerOptions{
		Executor:      executor,
		MaxConcurrent: 1,
		MaxQueued:     1,
	})
	require.NoError(t, err)
	t.Cleanup(func() { manager.StopAll(context.Background()) })
	releaseStarted := make(chan struct{})
	allowRelease := make(chan struct{})
	var releases atomic.Int32
	manager.mu.Lock()
	manager.releaseMemory = func() {
		if releases.Add(1) == 1 {
			close(releaseStarted)
			<-allowRelease
		}
	}
	manager.mu.Unlock()

	directory := t.TempDir()
	first, err := manager.Enqueue(testJobRequest(filepath.Join(directory, "first.png"), "first"), "first", managedtask.Ownership{ParentSessionID: "parent"})
	require.NoError(t, err)
	select {
	case prompt := <-started:
		require.Equal(t, "first", prompt)
	case <-time.After(time.Second):
		t.Fatal("first image execution did not start")
	}
	_, err = manager.Output(t.Context(), first.ID, true, time.Second)
	require.NoError(t, err)
	select {
	case <-releaseStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for image memory release")
	}
	second, err := manager.Enqueue(testJobRequest(filepath.Join(directory, "second.png"), "second"), "second", managedtask.Ownership{ParentSessionID: "parent"})
	require.NoError(t, err)
	select {
	case prompt := <-started:
		if prompt == "second" {
			t.Fatal("second image execution started during memory release")
		}
	case <-time.After(50 * time.Millisecond):
	}
	close(allowRelease)
	select {
	case prompt := <-started:
		require.Equal(t, "second", prompt)
	case <-time.After(time.Second):
		t.Fatal("second image execution did not start after memory release")
	}
	_, err = manager.Output(t.Context(), second.ID, true, time.Second)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return releases.Load() == 2 }, time.Second, time.Millisecond)
}

func testJobRequest(output, prompt string) JobRequest {
	return JobRequest{
		Mode:        ModeGenerate,
		Prompt:      prompt,
		Count:       1,
		OutputPaths: []string{output},
	}
}

func testImageResponse(model string) *Response {
	return &Response{
		Data:     []ImageData{{B64JSON: base64.StdEncoding.EncodeToString([]byte("image"))}},
		AuthMode: AuthCodex,
		Model:    model,
	}
}
