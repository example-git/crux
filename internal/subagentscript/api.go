package subagentscript

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	SessionEnv = "CRUX_SESSION_ID"
	ParentEnv  = "CRUX_PARENT_SESSION_ID"
	URLEnv     = "CRUX_SUBAGENT_URL"
	TokenEnv   = "CRUX_SUBAGENT_TOKEN"
)

type Request struct {
	Action       string `json:"action"`
	TaskID       string `json:"task_id,omitempty"`
	Prompt       string `json:"prompt,omitempty"`
	SubagentType string `json:"subagent_type,omitempty"`
	TimeoutMS    *int64 `json:"timeout_ms,omitempty"`
}

type Caller interface {
	Call(context.Context, Request) (any, error)
}

func Decode(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("expected one JSON value")
	}
	return nil
}

func (r Request) Validate() error {
	switch r.Action {
	case "list", "types":
		if r.TaskID != "" {
			return errors.New("task_id is not accepted for this action")
		}
	case "start":
		if r.TaskID != "" {
			return errors.New("start does not accept task_id")
		}
	case "output", "wait", "stop", "continue":
		if r.TaskID == "" {
			return errors.New("task_id is required")
		}
	default:
		return fmt.Errorf("unknown subagent action %q", r.Action)
	}
	if r.Action == "start" || r.Action == "continue" {
		if strings.TrimSpace(r.Prompt) == "" {
			return errors.New("prompt is required")
		}
	} else if r.Prompt != "" {
		return errors.New("prompt is only accepted by start and continue")
	}
	if r.SubagentType != "" && r.Action != "start" {
		return errors.New("subagent_type is only accepted by start")
	}
	if r.TimeoutMS != nil && (r.Action != "wait" || *r.TimeoutMS < 0 || *r.TimeoutMS > 600000) {
		return errors.New("timeout_ms is only accepted by wait and must be between 0 and 600000")
	}
	return nil
}

type environmentKey struct{}

func WithEnvironment(ctx context.Context, environment []string) context.Context {
	return context.WithValue(ctx, environmentKey{}, environment)
}

func Environment(ctx context.Context, base []string) []string {
	if base == nil {
		base = os.Environ()
	}
	result := make([]string, 0, len(base)+4)
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		if key != SessionEnv && key != ParentEnv && key != URLEnv && key != TokenEnv {
			result = append(result, entry)
		}
	}
	bound, _ := ctx.Value(environmentKey{}).([]string)
	return append(result, bound...)
}
