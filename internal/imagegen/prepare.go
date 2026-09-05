package imagegen

import (
	"context"
	"errors"
)

func (m *JobManager) PrepareToolRequest(ctx context.Context, request JobRequest, setup SetupRequest) (JobRequest, error) {
	if request.Owner == nil && m.setup != nil {
		if err := m.setup.Ensure(ctx, setup); err != nil {
			return request, err
		}
	}
	return m.PrepareRequest(ctx, request)
}

func (m *JobManager) PrepareRequest(ctx context.Context, request JobRequest) (JobRequest, error) {
	if m.pluginRuntime == nil {
		if request.Owner != nil {
			return request, errors.New("image plugin runtime is unavailable")
		}
		return request, ValidateGenerateRequest(GenerateRequest{Backend: request.Backend, Prompt: request.Prompt, Model: request.Model, N: request.Count, Quality: request.Quality, Size: request.Size, Background: request.Background})
	}
	runtime := m.pluginRuntime
	if runtime.Manager == nil {
		return request, errors.New("image plugin manager is unavailable")
	}
	if request.Owner == nil {
		if _, err := runtime.Manager.Rescan(ctx, 0); err != nil {
			return request, err
		}
		if request.Backend == "" || request.Backend == BackendAuto {
			if runtime.Select == nil {
				return request, errors.New("configure an exact image provider selection or specify an installed backend")
			}
			owner, err := runtime.Select(ctx)
			if err != nil {
				return request, err
			}
			request.Owner = &owner
		} else {
			resolve := runtime.ResolveOwner
			if resolve == nil {
				resolve = runtime.Manager.CaptureImageOwner
			}
			owner, err := resolve(string(request.Backend))
			if err != nil {
				return request, err
			}
			request.Owner = &owner
		}
	}
	owner := *request.Owner
	request.Owner = &owner
	prepared, value, err := runtime.Prepare(owner, request)
	if err != nil {
		return request, err
	}
	if err := runtime.Manager.ValidateImageOwner(ctx, owner); err != nil {
		return request, err
	}
	if runtime.Configuration != nil {
		configuration, err := runtime.Configuration(owner)
		if err != nil {
			return request, err
		}
		if _, _, err := imageConfiguration(value, configuration); err != nil {
			return request, err
		}
	}
	prepared.OutputExtension = value.Options.OutputExtension
	return prepared, nil
}
