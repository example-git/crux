package proto

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"

	"github.com/example-git/crux/internal/config"
)

const (
	WorkspaceChannelProtocol      = "crux-workspace-channel-v1"
	MaxWorkspaceChannelFrameBytes = config.MaxRemoteRuntimeBytes + (1 << 20)
	MaxWorkspaceChannelEventBytes = 16 << 20
	MaxWorkspaceChannelCommandID  = 128
	MaxWorkspaceChannelMessage    = 1024
)

type WorkspaceChannelFrameType string

const (
	WorkspaceChannelEventFrame           WorkspaceChannelFrameType = "event"
	WorkspaceChannelRuntimeReplaceFrame  WorkspaceChannelFrameType = "runtime.replace"
	WorkspaceChannelRefreshCompleteFrame WorkspaceChannelFrameType = "runtime.refresh_complete"
	WorkspaceChannelAcknowledgementFrame WorkspaceChannelFrameType = "ack"
)

type WorkspaceChannelStatus string

const (
	WorkspaceChannelStatusOK          WorkspaceChannelStatus = "ok"
	WorkspaceChannelStatusInvalid     WorkspaceChannelStatus = "invalid"
	WorkspaceChannelStatusConflict    WorkspaceChannelStatus = "conflict"
	WorkspaceChannelStatusForbidden   WorkspaceChannelStatus = "forbidden"
	WorkspaceChannelStatusNotFound    WorkspaceChannelStatus = "not_found"
	WorkspaceChannelStatusUnavailable WorkspaceChannelStatus = "unavailable"
	WorkspaceChannelStatusInternal    WorkspaceChannelStatus = "internal"
)

type WorkspaceChannelFrame struct {
	Type            WorkspaceChannelFrameType        `json:"type"`
	CommandID       string                           `json:"command_id,omitempty"`
	Event           json.RawMessage                  `json:"event,omitempty"`
	RuntimeReplace  *UpdateRemoteRuntimeRequest      `json:"runtime_replace,omitempty"`
	RefreshComplete *config.ClientRefreshCompletion  `json:"runtime_refresh_complete,omitempty"`
	Acknowledgement *WorkspaceChannelAcknowledgement `json:"acknowledgement,omitempty"`
}

type WorkspaceChannelAcknowledgement struct {
	Status    WorkspaceChannelStatus  `json:"status"`
	Authority *config.RemoteAuthority `json:"authority,omitempty"`
	Message   string                  `json:"message,omitempty"`
}

func DecodeWorkspaceChannelFrame(data []byte) (WorkspaceChannelFrame, error) {
	var frame WorkspaceChannelFrame
	if len(data) == 0 || len(data) > MaxWorkspaceChannelFrameBytes {
		return frame, errors.New("workspace channel frame exceeds byte limit")
	}
	if err := validateWorkspaceChannelJSON(data); err != nil {
		return frame, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&frame); err != nil {
		return frame, errors.New("invalid workspace channel frame")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return frame, errors.New("invalid workspace channel frame")
	}
	if err := ValidateWorkspaceChannelFrame(frame); err != nil {
		return frame, err
	}
	return frame, nil
}

func ValidateWorkspaceChannelFrame(frame WorkspaceChannelFrame) error {
	payloads := 0
	for _, present := range []bool{len(frame.Event) != 0, frame.RuntimeReplace != nil, frame.RefreshComplete != nil, frame.Acknowledgement != nil} {
		if present {
			payloads++
		}
	}
	if payloads != 1 {
		return errors.New("workspace channel frame requires exactly one payload")
	}
	requireCommandID := func() error {
		if frame.CommandID == "" || len(frame.CommandID) > MaxWorkspaceChannelCommandID || !utf8.ValidString(frame.CommandID) {
			return errors.New("workspace channel command ID is invalid")
		}
		return nil
	}
	switch frame.Type {
	case WorkspaceChannelEventFrame:
		if frame.CommandID != "" || len(frame.Event) == 0 || len(frame.Event) > MaxWorkspaceChannelEventBytes {
			return errors.New("invalid workspace channel event frame")
		}
	case WorkspaceChannelRuntimeReplaceFrame:
		if err := requireCommandID(); err != nil {
			return err
		}
		if frame.RuntimeReplace == nil || frame.Event != nil || frame.RefreshComplete != nil || frame.Acknowledgement != nil {
			return errors.New("invalid workspace runtime replacement frame")
		}
		request := frame.RuntimeReplace
		if request.ExpectedRevision == ^uint64(0) || request.Runtime.Revision != request.ExpectedRevision+1 {
			return errors.New("workspace runtime replacement revision is invalid")
		}
		digest, err := config.RemoteRuntimeDigest(request.Runtime)
		if err != nil || request.Runtime.Digest == "" || digest != request.Runtime.Digest {
			return errors.New("workspace runtime replacement digest is invalid")
		}
	case WorkspaceChannelRefreshCompleteFrame:
		if err := requireCommandID(); err != nil {
			return err
		}
		if frame.RefreshComplete == nil || frame.Event != nil || frame.RuntimeReplace != nil || frame.Acknowledgement != nil || frame.RefreshComplete.RequestID == "" {
			return errors.New("invalid workspace refresh completion frame")
		}
	case WorkspaceChannelAcknowledgementFrame:
		if err := requireCommandID(); err != nil {
			return err
		}
		if frame.Acknowledgement == nil || frame.Event != nil || frame.RuntimeReplace != nil || frame.RefreshComplete != nil {
			return errors.New("invalid workspace channel acknowledgement")
		}
		ack := frame.Acknowledgement
		switch ack.Status {
		case WorkspaceChannelStatusOK:
			if ack.Message != "" {
				return errors.New("successful workspace channel acknowledgement contains an error")
			}
		case WorkspaceChannelStatusInvalid, WorkspaceChannelStatusConflict, WorkspaceChannelStatusForbidden, WorkspaceChannelStatusNotFound, WorkspaceChannelStatusUnavailable, WorkspaceChannelStatusInternal:
			if ack.Authority != nil {
				return errors.New("failed workspace channel acknowledgement contains authority")
			}
		default:
			return errors.New("workspace channel acknowledgement status is invalid")
		}
		if len(ack.Message) > MaxWorkspaceChannelMessage || !utf8.ValidString(ack.Message) {
			return errors.New("workspace channel acknowledgement message is invalid")
		}
	default:
		return fmt.Errorf("unknown workspace channel frame type %q", frame.Type)
	}
	return nil
}

func validateWorkspaceChannelJSON(data []byte) error {
	if !utf8.Valid(data) {
		return errors.New("workspace channel frame must be valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	remaining := 1_000_000
	var walk func(int) error
	walk = func(depth int) error {
		remaining--
		if depth > 64 || remaining < 0 {
			return errors.New("workspace channel frame exceeds structural limits")
		}
		token, err := decoder.Token()
		if err != nil {
			return errors.New("invalid workspace channel JSON")
		}
		delim, nested := token.(json.Delim)
		if !nested {
			return nil
		}
		switch delim {
		case '{':
			keys := map[string]bool{}
			for decoder.More() {
				token, err := decoder.Token()
				key, ok := token.(string)
				if err != nil || !ok || len(key) > 1024 || keys[key] {
					return errors.New("duplicate or invalid workspace channel JSON field")
				}
				keys[key] = true
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("invalid workspace channel JSON")
		}
		if _, err := decoder.Token(); err != nil {
			return errors.New("invalid workspace channel JSON")
		}
		return nil
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("workspace channel frame must contain exactly one JSON value")
	}
	return nil
}
