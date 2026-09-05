package tools

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/imagegen"
	"github.com/example-git/crux/internal/permission"
	"github.com/example-git/crux/internal/providerplugin"
	managedtask "github.com/example-git/crux/internal/task"
)

const ImagegenToolName = "imagegen"

const imagegenDescription = "Queue image generation or editing in the native background worker pool. Installed image providers define available backends, models, options, output formats, limits, and declared fallback behavior. Specify an installed backend or omit it to use the configured selection. The exact provider owner is captured before permission and retained through execution; unavailable or replaced owners are not substituted. Use generate without inputs or edit with input paths. Exactly one of output or output_directory is required. Output directories may be shared; numbered allocation skips occupied or reserved names. Successful partial variants are preserved and completion is delivered to the originating agent."

type ImagegenParams struct {
	Mode            string              `json:"mode" enum:"generate,edit" description:"Operation to queue: generate creates from text only; edit uses one or more input images"`
	Backend         imagegen.Backend    `json:"backend,omitempty" description:"Installed image backend ID; omitted or auto uses configured selection"`
	Prompt          string              `json:"prompt" description:"Prompt describing the requested image or edit"`
	Images          []string            `json:"images,omitempty" description:"Edit mode only: ordered input image paths used as edit targets or references"`
	Output          string              `json:"output,omitempty" description:"Single output file path; requires n=1 and cannot be combined with output_directory"`
	OutputDirectory string              `json:"output_directory,omitempty" description:"Shared directory for numbered outputs in the selected provider format; required when n is greater than 1"`
	Model           string              `json:"model,omitempty" description:"Optional model ID declared by the selected image provider"`
	N               *int                `json:"n,omitempty" description:"Number of variants for this prompt, from 1 through 10. Defaults to 1"`
	Quality         imagegen.Quality    `json:"quality,omitempty" description:"Rendering quality"`
	Size            string              `json:"size,omitempty" description:"Output size: auto or WIDTHxHEIGHT within supported bounds"`
	Background      imagegen.Background `json:"background,omitempty" description:"Output transparency behavior"`
	Force           bool                `json:"force,omitempty" description:"Replace the exact resolved output paths if they already exist"`
}

type ImagegenPermissionsParams struct {
	Owner      *providerplugin.ImageOwner `json:"owner,omitempty"`
	Mode       string                     `json:"mode"`
	Backend    imagegen.Backend           `json:"backend,omitempty"`
	Prompt     string                     `json:"prompt"`
	Images     []string                   `json:"images,omitempty"`
	Outputs    []string                   `json:"outputs"`
	Model      string                     `json:"model,omitempty"`
	N          int                        `json:"n"`
	Quality    imagegen.Quality           `json:"quality,omitempty"`
	Size       string                     `json:"size,omitempty"`
	Background imagegen.Background        `json:"background,omitempty"`
	Force      bool                       `json:"force,omitempty"`
}

type ImagegenResponseMetadata struct {
	Owner   *providerplugin.ImageOwner `json:"owner,omitempty"`
	TaskID  string                     `json:"task_id"`
	Status  managedtask.Status         `json:"status"`
	Mode    string                     `json:"mode"`
	Outputs []string                   `json:"outputs"`
}

func NewImagegenTool(manager *imagegen.JobManager, permissions permission.Service, workingDir string, interactive ...bool) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		ImagegenToolName,
		imagegenDescription,
		func(ctx context.Context, params ImagegenParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if permission.IsSubagent(ctx) {
				return fantasy.NewTextErrorResponse(permission.ErrSubagentBackgroundTask.Error()), nil
			}
			if manager == nil {
				return fantasy.ToolResponse{}, errors.New("background image job service is unavailable")
			}
			request, err := resolveImagegenRequest(workingDir, params, func(request imagegen.JobRequest) (imagegen.JobRequest, error) {
				return manager.PrepareToolRequest(ctx, request, imagegen.SetupRequest{SessionID: GetSessionFromContext(ctx), ToolCallID: call.ID, Interactive: len(interactive) > 0 && interactive[0]})
			})
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, errors.New("session ID is required for image generation")
			}
			if permissions == nil {
				return fantasy.ToolResponse{}, errors.New("permission service is required for image generation")
			}
			permissionParams := ImagegenPermissionsParams{
				Owner:      request.Owner,
				Mode:       request.Mode,
				Backend:    request.Backend,
				Prompt:     request.Prompt,
				Images:     request.InputPaths,
				Outputs:    request.OutputPaths,
				Model:      request.Model,
				N:          request.Count,
				Quality:    request.Quality,
				Size:       request.Size,
				Background: request.Background,
				Force:      request.Force,
			}
			for _, path := range request.InputPaths {
				granted, permissionErr := authorizeExternalPath(
					ctx,
					permissions,
					workingDir,
					path,
					call.ID,
					ImagegenToolName,
					"read",
					fmt.Sprintf("Read image input outside working directory: %s", path),
					permissionParams,
				)
				if permissionErr != nil {
					return fantasy.ToolResponse{}, permissionErr
				}
				if !granted {
					return NewPermissionDeniedResponse(), nil
				}
			}
			var reservation *imagegen.OutputReservation
			if params.OutputDirectory != "" {
				directory := filepath.Dir(request.OutputPaths[0])
				granted, err := authorizeExternalPath(ctx, permissions, workingDir, directory, call.ID, ImagegenToolName, "read", "Inspect image output directory: "+directory, params)
				if err != nil {
					return fantasy.ToolResponse{}, err
				}
				if !granted {
					return NewPermissionDeniedResponse(), nil
				}
				reservation, err = manager.ReserveNumberedOutputs(request, directory)
				if err != nil {
					return fantasy.NewTextErrorResponse(err.Error()), nil
				}
				defer reservation.Release()
				request.OutputPaths = reservation.Paths()
				permissionParams.Outputs = request.OutputPaths
			}
			description := fmt.Sprintf("Queue %s image job with %d output(s)", request.Mode, request.Count)
			granted, err := permissions.Request(ctx, permission.CreatePermissionRequest{
				SessionID:   sessionID,
				Path:        request.OutputPaths[0],
				ToolCallID:  call.ID,
				ToolName:    ImagegenToolName,
				Action:      request.Mode,
				Description: description,
				Params:      permissionParams,
			})
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if !granted {
				return NewPermissionDeniedResponse(), nil
			}

			ownership := managedtask.OwnershipFromContext(ctx)
			if ownership.ParentSessionID == "" {
				ownership.ParentSessionID = sessionID
			}
			ownership.OriginToolCallID = call.ID
			var view managedtask.View
			if reservation != nil {
				view, err = manager.EnqueueReserved(request, imagegenJobDescription(request), ownership, reservation)
			} else {
				view, err = manager.Enqueue(request, imagegenJobDescription(request), ownership)
			}
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			metadata := ImagegenResponseMetadata{
				Owner:   request.Owner,
				TaskID:  view.ID,
				Status:  view.State.Status,
				Mode:    request.Mode,
				Outputs: append([]string(nil), request.OutputPaths...),
			}
			kind := "generation"
			if request.Mode == imagegen.ModeEdit {
				kind = "edit"
			}
			content := fmt.Sprintf("Queued image %s as task %s. Completion or failure will be delivered automatically.", kind, view.ID)
			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(content), metadata), nil
		},
	)
}

func resolveImagegenRequest(workingDir string, params ImagegenParams, prepare ...func(imagegen.JobRequest) (imagegen.JobRequest, error)) (imagegen.JobRequest, error) {
	count := 1
	if params.N != nil {
		count = *params.N
	}
	request := imagegen.JobRequest{
		Mode:       params.Mode,
		Backend:    params.Backend,
		Prompt:     params.Prompt,
		Model:      params.Model,
		Count:      count,
		Quality:    params.Quality,
		Size:       params.Size,
		Background: params.Background,
		Force:      params.Force,
	}
	if params.Mode != imagegen.ModeGenerate && params.Mode != imagegen.ModeEdit {
		return request, fmt.Errorf(`mode must be %q or %q`, imagegen.ModeGenerate, imagegen.ModeEdit)
	}
	if params.Mode == imagegen.ModeGenerate && len(params.Images) > 0 {
		return request, errors.New("images is only valid in edit mode")
	}
	if params.Mode == imagegen.ModeEdit && len(params.Images) == 0 {
		return request, errors.New("at least one image path is required in edit mode")
	}
	if params.Output == "" && params.OutputDirectory == "" {
		return request, errors.New("exactly one of output or output_directory is required")
	}
	if params.Output != "" && params.OutputDirectory != "" {
		return request, errors.New("output and output_directory cannot be used together")
	}
	if params.Output != "" && count != 1 {
		return request, errors.New("output requires n=1; use output_directory for multiple images")
	}
	if len(prepare) > 0 {
		var err error
		request.InputPaths = append([]string(nil), params.Images...)
		request, err = prepare[0](request)
		if err != nil {
			return request, err
		}
		request.InputPaths = nil
	} else {
		if err := imagegen.ValidateGenerateRequest(imagegen.GenerateRequest{
			Backend:    request.Backend,
			Prompt:     request.Prompt,
			Model:      request.Model,
			N:          request.Count,
			Quality:    request.Quality,
			Size:       request.Size,
			Background: request.Background,
		}); err != nil {
			return request, err
		}
	}
	for _, path := range params.Images {
		resolved, err := canonicalToolPath(workingDir, path)
		if err != nil {
			return request, fmt.Errorf("resolve input image %q: %w", path, err)
		}
		request.InputPaths = append(request.InputPaths, resolved)
	}
	if params.Output != "" {
		resolved, err := canonicalToolPath(workingDir, params.Output)
		if err != nil {
			return request, fmt.Errorf("resolve output path %q: %w", params.Output, err)
		}
		request.OutputPaths = []string{resolved}
		return request, nil
	}
	resolvedDirectory, err := canonicalToolPath(workingDir, params.OutputDirectory)
	if err != nil {
		return request, fmt.Errorf("resolve output directory %q: %w", params.OutputDirectory, err)
	}
	request.OutputPaths, err = resolveImagegenOutputPaths(resolvedDirectory, request.Backend, count, params.Force, request.OutputExtension)
	if err != nil {
		return request, err
	}
	return request, nil
}

func resolveImagegenOutputPaths(directory string, backend imagegen.Backend, count int, _ bool, extension ...string) ([]string, error) {
	paths := make([]string, 0, count)
	for index := 1; len(paths) < count; index++ {
		name := imagegen.NumberedOutputName(backend, index)
		if len(extension) > 0 && extension[0] != "" {
			name = fmt.Sprintf("image_%d%s", index, extension[0])
		}
		path := filepath.Join(directory, name)
		paths = append(paths, path)
	}
	return paths, nil
}

func imagegenJobDescription(request imagegen.JobRequest) string {
	prompt := strings.Join(strings.Fields(request.Prompt), " ")
	runes := []rune(prompt)
	if len(runes) > 80 {
		prompt = string(runes[:77]) + "..."
	}
	if prompt == "" {
		return request.Mode + " image"
	}
	return request.Mode + " image: " + prompt
}
