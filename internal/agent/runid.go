package agent

import (
	"context"

	"github.com/example-git/crux/internal/message"
)

type DeliveryMode string

type InputMaterializer func(context.Context) ([]message.ContentPart, []message.Attachment, error)

const (
	DeliveryQueue DeliveryMode = "queue"
	DeliverySteer DeliveryMode = "steer"
)

type (
	deliveryModeContextKey     struct{}
	requireActiveRunContextKey struct{}
)

func WithDeliveryMode(ctx context.Context, mode DeliveryMode) context.Context {
	return context.WithValue(ctx, deliveryModeContextKey{}, mode)
}

func WithRequireActiveRun(ctx context.Context) context.Context {
	return context.WithValue(ctx, requireActiveRunContextKey{}, true)
}

func RequireActiveRunFromContext(ctx context.Context) bool {
	required, _ := ctx.Value(requireActiveRunContextKey{}).(bool)
	return required
}

func DeliveryModeFromContext(ctx context.Context) DeliveryMode {
	mode, _ := ctx.Value(deliveryModeContextKey{}).(DeliveryMode)
	if mode == "" {
		return DeliveryQueue
	}
	return mode
}

// runIDContextKey is the unexported context key used to carry a
// caller-supplied RunID from the workspace HTTP boundary
// (backend.SendMessage) down into coordinator.Run without forcing a
// breaking change to the Coordinator.Run signature. The value is
// then copied onto SessionAgentCall.RunID by the coordinator so the
// agent's terminal RunComplete event can echo it back to the
// originating caller.
type (
	runIDContextKey                     struct{}
	messageRunIDContextKey              struct{}
	userMessageIDContextKey             struct{}
	assistantItemIDContextKey           struct{}
	submissionIDContextKey              struct{}
	turnInstructionsContextKey          struct{}
	instructionIsolationContextKey      struct{}
	instructionIsolationToolsContextKey struct{}
	analysisEffortContextKey            struct{}
	providerOperationContextKey         struct{}
	inputPartsContextKey                struct{}
	inputMaterializerContextKey         struct{}
	admissionCallbackContextKey         struct{}
)

// WithRunID returns ctx tagged with a per-request RunID. It is the
// boundary helper for callers that need their SendMessage→Run
// terminal event to be uniquely correlatable (e.g. `crux run`
// against a session that may be busy). Empty runIDs are stored
// as-is; downstream code treats an empty RunID as "caller did not
// supply one" and falls back to SessionID-only correlation.
func WithRunID(ctx context.Context, runID string) context.Context {
	return context.WithValue(ctx, runIDContextKey{}, runID)
}

// RunIDFromContext returns the RunID set by [WithRunID], or "" if
// none was set or the value is not a string. Exported because the
// coordinator and tests in other packages need to read it; safe to
// call on any context.
func RunIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(runIDContextKey{}).(string); ok {
		return v
	}
	return ""
}

func WithMessageRunID(ctx context.Context, runID string) context.Context {
	return context.WithValue(ctx, messageRunIDContextKey{}, runID)
}

func MessageRunIDFromContext(ctx context.Context) string {
	runID, _ := ctx.Value(messageRunIDContextKey{}).(string)
	return runID
}

func WithUserMessageID(ctx context.Context, messageID string) context.Context {
	return context.WithValue(ctx, userMessageIDContextKey{}, messageID)
}

func UserMessageIDFromContext(ctx context.Context) string {
	messageID, _ := ctx.Value(userMessageIDContextKey{}).(string)
	return messageID
}

func WithAssistantItemID(ctx context.Context, itemID string) context.Context {
	return context.WithValue(ctx, assistantItemIDContextKey{}, itemID)
}

func AssistantItemIDFromContext(ctx context.Context) string {
	itemID, _ := ctx.Value(assistantItemIDContextKey{}).(string)
	return itemID
}

func WithAdmissionCallback(ctx context.Context, callback func(error)) context.Context {
	return context.WithValue(ctx, admissionCallbackContextKey{}, callback)
}

func AdmissionCallbackFromContext(ctx context.Context) func(error) {
	callback, _ := ctx.Value(admissionCallbackContextKey{}).(func(error))
	return callback
}

func WithSubmissionID(ctx context.Context, submissionID string) context.Context {
	return context.WithValue(ctx, submissionIDContextKey{}, submissionID)
}

func SubmissionIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(submissionIDContextKey{}).(string); ok {
		return v
	}
	return ""
}

func WithTurnInstructions(ctx context.Context, instructions string) context.Context {
	return context.WithValue(ctx, turnInstructionsContextKey{}, instructions)
}

func TurnInstructionsFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(turnInstructionsContextKey{}).(string); ok {
		return v
	}
	return ""
}

func WithInstructionIsolation(ctx context.Context) context.Context {
	return context.WithValue(ctx, instructionIsolationContextKey{}, true)
}

func InstructionIsolationFromContext(ctx context.Context) bool {
	isolated, _ := ctx.Value(instructionIsolationContextKey{}).(bool)
	return isolated
}

func WithInstructionIsolationTools(ctx context.Context, names []string) context.Context {
	return context.WithValue(ctx, instructionIsolationToolsContextKey{}, append([]string(nil), names...))
}

func InstructionIsolationToolsFromContext(ctx context.Context) []string {
	names, _ := ctx.Value(instructionIsolationToolsContextKey{}).([]string)
	return append([]string(nil), names...)
}

func WithAnalysisEffort(ctx context.Context, effort string) context.Context {
	return context.WithValue(ctx, analysisEffortContextKey{}, effort)
}

func AnalysisEffortFromContext(ctx context.Context) string {
	effort, _ := ctx.Value(analysisEffortContextKey{}).(string)
	return effort
}

func WithInputParts(ctx context.Context, parts []message.ContentPart) context.Context {
	return context.WithValue(ctx, inputPartsContextKey{}, parts)
}

func InputPartsFromContext(ctx context.Context) []message.ContentPart {
	parts, _ := ctx.Value(inputPartsContextKey{}).([]message.ContentPart)
	return parts
}

func WithInputMaterializer(ctx context.Context, materializer InputMaterializer) context.Context {
	return context.WithValue(ctx, inputMaterializerContextKey{}, materializer)
}

func InputMaterializerFromContext(ctx context.Context) InputMaterializer {
	materializer, _ := ctx.Value(inputMaterializerContextKey{}).(InputMaterializer)
	return materializer
}

func WithProviderOperation(ctx context.Context, operation string) context.Context {
	return context.WithValue(ctx, providerOperationContextKey{}, operation)
}

func ProviderOperationFromContext(ctx context.Context) string {
	operation, _ := ctx.Value(providerOperationContextKey{}).(string)
	return operation
}
