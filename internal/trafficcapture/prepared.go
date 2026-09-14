package trafficcapture

import (
	"context"
	"fmt"
	"os"
)

type PreparedRequest struct {
	request            Request
	target             resolvedTarget
	executableIdentity pathIdentity
	workingDirIdentity pathIdentity
	output             *outputBinding
}

func Prepare(ctx context.Context, request Request) (*PreparedRequest, error) {
	target, err := resolveTarget(ctx, request)
	if err != nil {
		return nil, err
	}
	if len(target.Command) == 0 {
		return nil, fmt.Errorf("traffic capture target command is empty")
	}
	executableIdentity, err := capturePathIdentity(target.Command[0], 0)
	if err != nil {
		return nil, fmt.Errorf("capture target executable identity: %w", err)
	}
	workingDirIdentity, err := capturePathIdentity(target.WorkingDir, os.ModeDir)
	if err != nil {
		return nil, fmt.Errorf("capture target working directory identity: %w", err)
	}
	output, err := captureOutput(request.CapturePath)
	if err != nil {
		return nil, err
	}
	return &PreparedRequest{
		request:            request,
		target:             target,
		executableIdentity: executableIdentity,
		workingDirIdentity: workingDirIdentity,
		output:             output,
	}, nil
}

func (prepared *PreparedRequest) Close() {
	if prepared != nil {
		prepared.output.close()
	}
}

func (prepared *PreparedRequest) Request() Request {
	request := prepared.request
	if request.Executable != "" {
		request.Executable = prepared.target.Command[0]
	}
	request.WorkingDir = prepared.target.WorkingDir
	return request
}

func (prepared *PreparedRequest) Launch(ctx context.Context) (Metadata, error) {
	return launchPrepared(ctx, prepared)
}
