package client

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/pubsub"
)

func decodeWorkspaceEvent(data []byte) (any, error) {
	var payload pubsub.Payload
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("decode workspace event envelope: %w", err)
	}
	var event any
	switch payload.Type {
	case pubsub.PayloadTypeLSPEvent:
		event = new(pubsub.Event[proto.LSPEvent])
	case pubsub.PayloadTypeMCPEvent:
		event = new(pubsub.Event[proto.MCPEvent])
	case pubsub.PayloadTypePermissionRequest:
		event = new(pubsub.Event[proto.PermissionRequest])
	case pubsub.PayloadTypePermissionNotification:
		event = new(pubsub.Event[proto.PermissionNotification])
	case pubsub.PayloadTypeQuestionRequest:
		event = new(pubsub.Event[proto.QuestionRequest])
	case pubsub.PayloadTypeQuestionNotification:
		event = new(pubsub.Event[proto.QuestionNotification])
	case pubsub.PayloadTypeMessage:
		event = new(pubsub.Event[proto.Message])
	case pubsub.PayloadTypeSession:
		event = new(pubsub.Event[proto.Session])
	case pubsub.PayloadTypeFile:
		event = new(pubsub.Event[proto.File])
	case pubsub.PayloadTypeAgentEvent:
		event = new(pubsub.Event[proto.AgentEvent])
	case pubsub.PayloadTypeConfigChanged:
		event = new(pubsub.Event[proto.ConfigChanged])
	case pubsub.PayloadTypeClientRefresh:
		event = new(pubsub.Event[config.ClientRefreshRequest])
	case pubsub.PayloadTypeSkillsEvent:
		event = new(pubsub.Event[proto.SkillsEvent])
	case pubsub.PayloadTypeTaskNotification:
		event = new(pubsub.Event[proto.TaskNotification])
	case pubsub.PayloadTypeRunComplete:
		event = new(pubsub.Event[proto.RunComplete])
	default:
		return nil, fmt.Errorf("unknown workspace event type %q", payload.Type)
	}
	if err := json.Unmarshal(payload.Payload, event); err != nil {
		return nil, fmt.Errorf("decode workspace event %q: %w", payload.Type, err)
	}
	switch value := event.(type) {
	case *pubsub.Event[proto.LSPEvent]:
		return *value, nil
	case *pubsub.Event[proto.MCPEvent]:
		return *value, nil
	case *pubsub.Event[proto.PermissionRequest]:
		return *value, nil
	case *pubsub.Event[proto.PermissionNotification]:
		return *value, nil
	case *pubsub.Event[proto.QuestionRequest]:
		return *value, nil
	case *pubsub.Event[proto.QuestionNotification]:
		return *value, nil
	case *pubsub.Event[proto.Message]:
		return *value, nil
	case *pubsub.Event[proto.Session]:
		return *value, nil
	case *pubsub.Event[proto.File]:
		return *value, nil
	case *pubsub.Event[proto.AgentEvent]:
		return *value, nil
	case *pubsub.Event[proto.ConfigChanged]:
		return *value, nil
	case *pubsub.Event[config.ClientRefreshRequest]:
		return *value, nil
	case *pubsub.Event[proto.SkillsEvent]:
		return *value, nil
	case *pubsub.Event[proto.TaskNotification]:
		return *value, nil
	case *pubsub.Event[proto.RunComplete]:
		return *value, nil
	default:
		return nil, errors.New("workspace event decoder returned an unsupported type")
	}
}
