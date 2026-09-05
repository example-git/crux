package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/foundation/catalog"
	anthropicprovider "github.com/example-git/crux/foundation/providers/anthropic"
	"github.com/example-git/crux/foundation/providers/openai"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/imageattachment"
	"github.com/example-git/crux/internal/message"
	codexresponses "github.com/example-git/crux/internal/oauth/codex/responses"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/session"
	"github.com/stretchr/testify/require"

	_ "github.com/joho/godotenv/autoload"
)

func TestMain(m *testing.M) {
	slog.SetLogLoggerLevel(slog.LevelError)
	config.DefaultProviderProfile = string(config.ProviderProfileIntegrated)
	m.Run()
}

type promptCaptureModel struct {
	*finishStreamModel
	prompt fantasy.Prompt
}

func (m *promptCaptureModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.prompt = cloneFantasyMessages(call.Prompt)
	return m.finishStreamModel.Stream(ctx, call)
}

type providerExecutedStreamModel struct {
	finishStreamModel
}

func (m *providerExecutedStreamModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{
			Type:             fantasy.StreamPartTypeToolCall,
			ID:               "server_call",
			ToolCallName:     "web_search",
			ToolCallInput:    `{"query":"current result"}`,
			ProviderExecuted: true,
		}) {
			return
		}
		if !yield(fantasy.StreamPart{
			Type:             fantasy.StreamPartTypeToolResult,
			ID:               "server_call",
			ToolCallName:     "web_search",
			ProviderExecuted: true,
		}) {
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
	}, nil
}

func makeTestTodos(n int) []session.Todo {
	todos := make([]session.Todo, n)
	for i := range n {
		todos[i] = session.Todo{
			Status:  session.TodoStatusPending,
			Content: fmt.Sprintf("Task %d: Implement feature with some description that makes it realistic", i),
		}
	}
	return todos
}

func TestQueuedPromptsListExposesOnlySubmissionMetadata(t *testing.T) {
	sa := &sessionAgent{messageQueue: csync.NewMap[string, []SessionAgentCall]()}
	sa.messageQueue.Set("session", []SessionAgentCall{{
		SubmissionID: "submission-id",
		Prompt:       "queued prompt",
		Attachments:  []message.Attachment{{FileName: "secret.png", Content: []byte("secret")}},
	}})

	require.Equal(t, []QueuedPrompt{{SubmissionID: "submission-id", Prompt: "queued prompt"}}, sa.QueuedPromptsList("session"))
}

func TestAuxiliaryStreamCallUsesAttemptedModelRuntime(t *testing.T) {
	providerOptions := fantasy.ProviderOptions{codexresponses.Name: &codexresponses.ProviderOptions{ReasoningEffort: "low"}}
	refreshCalls := 0
	modelProviderCalls := 0
	smallLanguageModel := &finishStreamModel{text: "small"}
	model := Model{
		Model:              smallLanguageModel,
		ModelCfg:           config.SelectedModel{Provider: "small-provider", Model: "small-model"},
		ProviderOptions:    providerOptions,
		SystemPromptPrefix: "small-provider-prefix",
		InstructionPolicy:  fantasy.InstructionPolicyAnthropic,
		OnAuthRefresh: func(context.Context, *fantasy.ProviderError) error {
			refreshCalls++
			return nil
		},
	}

	instructions := auxiliaryInstructions("auxiliary instructions", model)
	call := auxiliaryStreamCall("prompt", map[string]string{"x-request-purpose": "title"}, model, func() Model {
		modelProviderCalls++
		return model
	}, instructions)

	require.Equal(t, providerOptions, call.ProviderOptions)
	require.Equal(t, "title", call.Headers["x-request-purpose"])
	require.NoError(t, call.OnAuthRefresh(t.Context(), nil))
	require.Equal(t, 1, refreshCalls)
	require.Same(t, smallLanguageModel, call.ModelProvider())
	require.Equal(t, 1, modelProviderCalls)

	_, prepared, err := call.PrepareStep(t.Context(), fantasy.PrepareStepFunctionOptions{
		Messages: []fantasy.Message{fantasy.NewUserMessage("user message")},
	})
	require.NoError(t, err)
	require.Len(t, prepared.Messages, 2)
	require.Equal(t, fantasy.MessageRoleSystem, prepared.Messages[0].Role)
	require.Len(t, prepared.Messages[0].Content, 2)
	prefix, ok := prepared.Messages[0].Content[0].(fantasy.TextPart)
	require.True(t, ok)
	require.Equal(t, "small-provider-prefix", prefix.Text)
	require.False(t, fantasy.InstructionPartOptionsFrom(prefix.ProviderOptions).CacheBoundary)
	auxiliary, ok := prepared.Messages[0].Content[1].(fantasy.TextPart)
	require.True(t, ok)
	require.Equal(t, "auxiliary instructions", auxiliary.Text)
	require.True(t, fantasy.InstructionPartOptionsFrom(auxiliary.ProviderOptions).CacheBoundary)

	largeLanguageModel := &finishStreamModel{text: "large"}
	largeModel := Model{
		Model:    largeLanguageModel,
		ModelCfg: config.SelectedModel{Provider: "large-provider", Model: "large-model"},
	}
	crossProviderCall := auxiliaryStreamCall("prompt", nil, model, func() Model {
		return largeModel
	}, instructions)
	require.ErrorContains(t, crossProviderCall.OnAuthRefresh(t.Context(), nil), "model generation changed")
	require.Same(t, smallLanguageModel, crossProviderCall.ModelProvider())
}

type overloadStreamModel struct {
	finishStreamModel
	attempts int
}

func (m *overloadStreamModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	m.attempts++
	return nil, fantasy.NewServerOverloadError()
}

type authenticationStreamModel struct {
	finishStreamModel
	attempts int
}

func (m *authenticationStreamModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	m.attempts++
	return nil, &fantasy.ProviderError{StatusCode: 401, Message: "unauthorized"}
}

func TestManifestRetryPolicyControlsAgentCall(t *testing.T) {
	refreshCalls := 0
	callback := func(context.Context, *fantasy.ProviderError) error {
		refreshCalls++
		return nil
	}
	languageModel := &overloadStreamModel{}
	model := Model{
		Model:    languageModel,
		ModelCfg: config.SelectedModel{Provider: "manifest-provider", Model: "manifest-model"},
		Retry:    &manifest.RetryPolicy{MaxAttempts: 3, Authentication: "refresh-once"},
	}
	require.Equal(t, 0, *modelMaxRetries(model))
	require.NoError(t, modelAuthRefresh(model, callback)(t.Context(), nil))
	require.Equal(t, 1, refreshCalls)

	call := auxiliaryStreamCall("prompt", nil, model, func() Model { return model }, fantasy.Instructions{})
	_, err := fantasy.NewAgent(languageModel).Stream(t.Context(), call)
	require.Error(t, err)
	require.True(t, fantasy.IsServerOverloadError(err))
	require.Equal(t, 1, languageModel.attempts)

	model.Retry.Authentication = "never"
	require.Nil(t, modelAuthRefresh(model, callback))
}

func TestManifestRetryPolicyUsesOneOuterAuthenticationRefresh(t *testing.T) {
	expiredModel := &authenticationStreamModel{}
	refreshedModel := &overloadStreamModel{}
	selected := config.SelectedModel{Provider: "manifest-provider", Model: "manifest-model"}
	retryPolicy := &manifest.RetryPolicy{MaxAttempts: 3, Authentication: "refresh-once"}
	current := Model{Model: expiredModel, ModelCfg: selected, Retry: retryPolicy}
	initial := current
	refreshCalls := 0
	initial.OnAuthRefresh = func(context.Context, *fantasy.ProviderError) error {
		refreshCalls++
		current = Model{Model: refreshedModel, ModelCfg: selected, Retry: retryPolicy}
		return nil
	}

	call := auxiliaryStreamCall("prompt", nil, initial, func() Model { return current }, fantasy.Instructions{})
	_, err := fantasy.NewAgent(expiredModel).Stream(t.Context(), call)
	require.Error(t, err)
	require.True(t, fantasy.IsServerOverloadError(err))
	require.Equal(t, 1, refreshCalls)
	require.Equal(t, 1, expiredModel.attempts)
	require.Equal(t, 1, refreshedModel.attempts)
}

func TestShouldIncludeTodoReminderTracksEmptyTransitions(t *testing.T) {
	mainAgent := &sessionAgent{todoReminderActive: make(map[string]bool)}

	require.True(t, mainAgent.shouldIncludeTodoReminder("first", true))
	require.False(t, mainAgent.shouldIncludeTodoReminder("first", true))
	require.False(t, mainAgent.shouldIncludeTodoReminder("first", false))
	require.True(t, mainAgent.shouldIncludeTodoReminder("first", true))
	require.True(t, mainAgent.shouldIncludeTodoReminder("second", true))
	require.False(t, mainAgent.shouldIncludeTodoReminder("second", true))

	subAgent := &sessionAgent{
		isSubAgent:         true,
		todoReminderActive: make(map[string]bool),
	}
	require.False(t, subAgent.shouldIncludeTodoReminder("first", true))
}

func BenchmarkBuildSummaryPrompt(b *testing.B) {
	cases := []struct {
		name     string
		numTodos int
	}{
		{"0todos", 0},
		{"5todos", 5},
		{"10todos", 10},
		{"50todos", 50},
	}

	for _, tc := range cases {
		todos := makeTestTodos(tc.numTodos)

		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				_ = buildSummaryPrompt(todos)
			}
		})
	}
}

func TestPreparePrompt_FiltersImageAttachments(t *testing.T) {
	env := testEnv(t)
	sa := testSessionAgent(env, nil, nil, "test prompt")
	agent := sa.(*sessionAgent)

	ctx := t.Context()
	sess, err := env.sessions.Create(ctx, "test")
	require.NoError(t, err)

	// User message with text, a text attachment, and an image attachment.
	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.User,
		Parts: []message.ContentPart{
			message.TextContent{Text: "hello world"},
			message.BinaryContent{Path: "notes.txt", MIMEType: "text/plain", Data: []byte("important notes")},
			message.BinaryContent{Path: "image.png", MIMEType: "image/png", Data: []byte("fake-image-data")},
		},
	})
	require.NoError(t, err)

	msgs, err := env.messages.List(ctx, sess.ID)
	require.NoError(t, err)

	// New-turn image attachment (not yet stored in the DB).
	imageAtt := message.Attachment{
		FileName: "screenshot.png",
		MimeType: "image/png",
		Content:  []byte("fake-screenshot"),
	}

	// When supportsImages is false, image attachments should be stripped
	// from history AND from the files list.
	history, files := agent.preparePrompt(msgs, preparePromptOptions{
		IncludeTodoReminder: true,
		Attachments:         []message.Attachment{imageAtt},
	})
	// First message is the system reminder, second is the user message.
	require.Len(t, history, 2)
	require.Len(t, history[1].Content, 1)
	text, ok := fantasy.AsMessagePart[fantasy.TextPart](history[1].Content[0])
	require.True(t, ok)
	require.Contains(t, text.Text, "hello world")
	require.Contains(t, text.Text, "important notes")
	require.Empty(t, files, "image files should be excluded when model does not support images")

	// When supportsImages is true, image attachments should remain in
	// history and be included in the files list.
	history, files = agent.preparePrompt(msgs, preparePromptOptions{
		SupportsImages:      true,
		IncludeTodoReminder: true,
		Attachments:         []message.Attachment{imageAtt},
	})
	require.Len(t, history, 2)
	require.Len(t, history[1].Content, 2)
	text, ok = fantasy.AsMessagePart[fantasy.TextPart](history[1].Content[0])
	require.True(t, ok)
	require.Contains(t, text.Text, "hello world")
	file, ok := fantasy.AsMessagePart[fantasy.FilePart](history[1].Content[1])
	require.True(t, ok)
	require.Equal(t, "image.png", file.Filename)
	require.Len(t, files, 1, "new-turn image attachment should be included when model supports images")
	require.Equal(t, "screenshot.png", files[0].Filename)
}

func TestOmitAgedImageHistoryUsesLaterUserTurns(t *testing.T) {
	for laterUserTurns := range retainedImageHistoryTurns + 2 {
		t.Run(fmt.Sprintf("later user turns %d", laterUserTurns), func(t *testing.T) {
			messages := []fantasy.Message{
				{
					Role: fantasy.MessageRoleUser,
					Content: []fantasy.MessagePart{fantasy.FilePart{
						Filename:  "history.png",
						MediaType: "image/png",
						Data:      []byte("history-image"),
					}},
				},
				fantasy.NewSystemMessage("system"),
				{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "assistant"}}},
				{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "tool"}}},
			}
			for index := range laterUserTurns {
				messages = append(messages, fantasy.NewUserMessage(fmt.Sprintf("later %d", index)))
			}

			result := omitAgedImageHistory(messages, nil)

			original, ok := fantasy.AsMessagePart[fantasy.FilePart](messages[0].Content[0])
			require.True(t, ok)
			require.Equal(t, []byte("history-image"), original.Data)
			if laterUserTurns <= retainedImageHistoryTurns {
				file, retained := fantasy.AsMessagePart[fantasy.FilePart](result[0].Content[0])
				require.True(t, retained)
				require.Equal(t, []byte("history-image"), file.Data)
				return
			}
			require.Len(t, result[0].Content, 1)
			marker, ok := fantasy.AsMessagePart[fantasy.TextPart](result[0].Content[0])
			require.True(t, ok)
			require.Equal(t, omittedImageHistoryMessage, marker.Text)
		})
	}
}

func TestOmitAgedImageHistoryPreservesTextAndNonImageFiles(t *testing.T) {
	messages := []fantasy.Message{{
		Role: fantasy.MessageRoleUser,
		Content: []fantasy.MessagePart{
			fantasy.TextPart{Text: "before"},
			fantasy.FilePart{Filename: "first.png", MediaType: "image/png", Data: []byte("first")},
			fantasy.FilePart{Filename: "notes.pdf", MediaType: "application/pdf", Data: []byte("notes")},
			fantasy.FilePart{Filename: "second.PNG", MediaType: "IMAGE/PNG", Data: []byte("second")},
			fantasy.TextPart{Text: "after"},
		},
	}}
	for index := range retainedImageHistoryTurns + 1 {
		messages = append(messages, fantasy.NewUserMessage(fmt.Sprintf("later %d", index)))
	}

	result := omitAgedImageHistory(messages, nil)

	require.Len(t, result[0].Content, 4)
	before, ok := fantasy.AsMessagePart[fantasy.TextPart](result[0].Content[0])
	require.True(t, ok)
	require.Equal(t, "before", before.Text)
	marker, ok := fantasy.AsMessagePart[fantasy.TextPart](result[0].Content[1])
	require.True(t, ok)
	require.Equal(t, omittedImageHistoryMessage, marker.Text)
	document, ok := fantasy.AsMessagePart[fantasy.FilePart](result[0].Content[2])
	require.True(t, ok)
	require.Equal(t, "notes.pdf", document.Filename)
	after, ok := fantasy.AsMessagePart[fantasy.TextPart](result[0].Content[3])
	require.True(t, ok)
	require.Equal(t, "after", after.Text)
	require.Len(t, messages[0].Content, 5)
	_, firstImageStillPresent := fantasy.AsMessagePart[fantasy.FilePart](messages[0].Content[1])
	require.True(t, firstImageStillPresent)
	_, secondImageStillPresent := fantasy.AsMessagePart[fantasy.FilePart](messages[0].Content[3])
	require.True(t, secondImageStillPresent)
}

func TestRunOmitsAgedImagesOnlyFromProviderPrompt(t *testing.T) {
	env := testEnv(t)
	ctx := t.Context()
	currentSession, err := env.sessions.Create(ctx, "image history")
	require.NoError(t, err)
	old, err := env.messages.Create(ctx, currentSession.ID, message.CreateMessageParams{
		Role: message.User,
		Parts: []message.ContentPart{
			message.TextContent{Text: "old image"},
			message.BinaryContent{Path: "old.png", MIMEType: "image/png", Data: []byte("old-image-data")},
		},
	})
	require.NoError(t, err)
	for index := range retainedImageHistoryTurns {
		_, err = env.messages.Create(ctx, currentSession.ID, message.CreateMessageParams{
			Role:  message.User,
			Parts: []message.ContentPart{message.TextContent{Text: fmt.Sprintf("later %d", index)}},
		})
		require.NoError(t, err)
	}

	model := &promptCaptureModel{finishStreamModel: &finishStreamModel{text: "done"}}
	configuredModel := Model{
		Model: model,
		CatalogModel: catalog.Model{
			SupportsImages:   true,
			ContextWindow:    200000,
			DefaultMaxTokens: 10000,
		},
	}
	agent := NewSessionAgent(SessionAgentOptions{
		LargeModel:   configuredModel,
		SmallModel:   configuredModel,
		SystemPrompt: "system",
		IsYolo:       true,
		Sessions:     env.sessions,
		Messages:     env.messages,
	})

	_, err = agent.Run(ctx, SessionAgentCall{SessionID: currentSession.ID, Prompt: "fourth later turn"})
	require.NoError(t, err)

	markerFound := false
	for _, promptMessage := range model.prompt {
		for _, part := range promptMessage.Content {
			if file, ok := fantasy.AsMessagePart[fantasy.FilePart](part); ok {
				require.NotEqual(t, []byte("old-image-data"), file.Data)
			}
			if text, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok && text.Text == omittedImageHistoryMessage {
				markerFound = true
			}
		}
	}
	require.True(t, markerFound)

	persisted, err := env.messages.Get(ctx, old.ID)
	require.NoError(t, err)
	require.Len(t, persisted.BinaryContent(), 1)
	require.Equal(t, []byte("old-image-data"), persisted.BinaryContent()[0].Data)
}

func TestRunNormalizesImagesOnlyForProviderPrompt(t *testing.T) {
	env := testEnv(t)
	ctx := t.Context()
	currentSession, err := env.sessions.Create(ctx, "full-size image persistence")
	require.NoError(t, err)

	pixels := image.NewNRGBA(image.Rect(0, 0, 2400, 1200))
	var source bytes.Buffer
	require.NoError(t, png.Encode(&source, pixels))
	sourceBytes := append([]byte(nil), source.Bytes()...)

	registration, registered := integratedRegistration(t, codexresponses.Name)
	require.True(t, registered)
	policy, ok := imageattachment.PolicyFor(registration)
	require.True(t, ok)
	model := &promptCaptureModel{finishStreamModel: &finishStreamModel{text: "done"}}
	configuredModel := Model{
		Model: model,
		CatalogModel: catalog.Model{
			SupportsImages:   true,
			ContextWindow:    200000,
			DefaultMaxTokens: 10000,
		},
		ImagePolicy: &policy,
	}
	agent := NewSessionAgent(SessionAgentOptions{
		LargeModel:   configuredModel,
		SmallModel:   configuredModel,
		SystemPrompt: "system",
		IsYolo:       true,
		Sessions:     env.sessions,
		Messages:     env.messages,
	})

	_, err = agent.Run(ctx, SessionAgentCall{
		SessionID: currentSession.ID,
		Prompt:    "inspect the original",
		Attachments: []message.Attachment{{
			FileName: "original.png",
			FilePath: "original.png",
			MimeType: "image/png",
			Content:  sourceBytes,
		}},
	})
	require.NoError(t, err)

	var providerImage *fantasy.FilePart
	for _, promptMessage := range model.prompt {
		for _, part := range promptMessage.Content {
			if file, fileOK := fantasy.AsMessagePart[fantasy.FilePart](part); fileOK && file.Filename == "original.jpg" {
				fileCopy := file
				providerImage = &fileCopy
			}
		}
	}
	require.NotNil(t, providerImage)
	require.Equal(t, "image/jpeg", providerImage.MediaType)
	require.NotEqual(t, sourceBytes, providerImage.Data)
	require.LessOrEqual(t, len(providerImage.Data), policy.MaxRawBytes)
	providerConfig, format, err := image.DecodeConfig(bytes.NewReader(providerImage.Data))
	require.NoError(t, err)
	require.Equal(t, "jpeg", format)
	require.LessOrEqual(t, max(providerConfig.Width, providerConfig.Height), policy.MaxSide)
	require.LessOrEqual(t, ((providerConfig.Width+31)/32)*((providerConfig.Height+31)/32), policy.MaxPatches)

	storedMessages, err := env.messages.List(ctx, currentSession.ID)
	require.NoError(t, err)
	var storedImage *message.BinaryContent
	for _, storedMessage := range storedMessages {
		for _, binary := range storedMessage.BinaryContent() {
			if binary.Path == "original.png" {
				binaryCopy := binary
				storedImage = &binaryCopy
			}
		}
	}
	require.NotNil(t, storedImage)
	require.Equal(t, "image/png", storedImage.MIMEType)
	require.Equal(t, sourceBytes, storedImage.Data)
	storedConfig, storedFormat, err := image.DecodeConfig(bytes.NewReader(storedImage.Data))
	require.NoError(t, err)
	require.Equal(t, "png", storedFormat)
	require.Equal(t, 2400, storedConfig.Width)
	require.Equal(t, 1200, storedConfig.Height)
}

func TestPreparePrompt_TodoReminderModes(t *testing.T) {
	env := testEnv(t)
	agent := testSessionAgent(env, nil, nil, "test prompt").(*sessionAgent)

	history, _ := agent.preparePrompt(nil, preparePromptOptions{IncludeTodoReminder: true})
	require.Len(t, history, 1)
	require.Equal(t, fantasy.MessageRoleUser, history[0].Role)

	history, _ = agent.preparePrompt(nil, preparePromptOptions{})
	require.Empty(t, history)

	agent.isSubAgent = true
	history, _ = agent.preparePrompt(nil, preparePromptOptions{IncludeTodoReminder: true})
	require.Empty(t, history)
}

func TestGetSessionMessagesStartsAtSummaryCheckpoint(t *testing.T) {
	env := testEnv(t)
	agent := testSessionAgent(env, nil, nil, "test prompt").(*sessionAgent)
	ctx := t.Context()
	sess, err := env.sessions.Create(ctx, "test")
	require.NoError(t, err)

	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "old"}}})
	require.NoError(t, err)
	summary, err := env.messages.Create(ctx, sess.ID, message.CreateMessageParams{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "summary"}}, IsSummaryMessage: true})
	require.NoError(t, err)
	last, err := env.messages.Create(ctx, sess.ID, message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "new"}}})
	require.NoError(t, err)

	sess.SummaryMessageID = summary.ID
	msgs, err := agent.getSessionMessages(ctx, sess)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.Equal(t, summary.ID, msgs[0].ID)
	require.Equal(t, message.User, msgs[0].Role)
	require.Equal(t, last.ID, msgs[1].ID)
}

func TestGetSessionMessagesFallsBackForStaleCheckpoint(t *testing.T) {
	env := testEnv(t)
	agent := testSessionAgent(env, nil, nil, "test prompt").(*sessionAgent)
	ctx := t.Context()
	sess, err := env.sessions.Create(ctx, "test")
	require.NoError(t, err)

	first, err := env.messages.Create(ctx, sess.ID, message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "first"}}})
	require.NoError(t, err)
	last, err := env.messages.Create(ctx, sess.ID, message.CreateMessageParams{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "last"}}})
	require.NoError(t, err)

	sess.SummaryMessageID = "missing"
	msgs, err := agent.getSessionMessages(ctx, sess)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.Equal(t, first.ID, msgs[0].ID)
	require.Equal(t, last.ID, msgs[1].ID)
}

func TestStepProviderMetadataPersistsContinuationAndMessageScopes(t *testing.T) {
	env := testEnv(t)
	ctx := t.Context()
	sess, err := env.sessions.Create(ctx, "metadata scopes")
	require.NoError(t, err)

	const continuationNamespace = "example.responses.continuation"
	metadata, err := stepProviderMetadata(
		[]manifest.MetadataContract{{Namespace: continuationNamespace, Version: 1, Scope: "continuation", Schema: map[string]any{"$schema": "https://json-schema.org/draft/2020-12/schema", "type": "object", "properties": map[string]any{"response_id": map[string]any{"type": "string"}}, "required": []string{"response_id"}, "additionalProperties": false}}},
		fantasy.ProviderMetadata{
			continuationNamespace: &openai.ResponsesProviderMetadata{ResponseID: "resp_continuation"},
			openai.Name:           &openai.ResponsesProviderMetadata{ResponseID: "resp_message"},
		},
	)
	require.NoError(t, err)

	created, err := env.messages.Create(ctx, sess.ID, message.CreateMessageParams{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "answer"}}})
	require.NoError(t, err)
	created.SetMessageProviderMetadata(metadata)
	require.NoError(t, env.messages.Update(ctx, created))
	require.NoError(t, env.messages.FlushAll(ctx))

	loaded, err := env.messages.Get(ctx, created.ID)
	require.NoError(t, err)
	stored := loaded.MetadataContent()
	require.Len(t, stored, 2)
	require.Equal(t, openai.Name, stored[0].Namespace)
	require.Equal(t, message.ProviderMetadataScopeMessage, stored[0].Scope)
	require.Equal(t, continuationNamespace, stored[1].Namespace)
	require.Equal(t, message.ProviderMetadataScopeContinuation, stored[1].Scope)

	replayed := loaded.ToAIMessage()
	require.Len(t, replayed, 1)
	require.Contains(t, replayed[0].ProviderOptions, openai.Name)
	require.Contains(t, replayed[0].ProviderOptions, continuationNamespace)
}

func TestCreateUserMessage_RetainsAllAttachments(t *testing.T) {
	env := testEnv(t)
	sa := testSessionAgent(env, nil, nil, "test prompt")
	agent := sa.(*sessionAgent)

	ctx := t.Context()
	sess, err := env.sessions.Create(ctx, "test")
	require.NoError(t, err)

	// Mix of text and image attachments — all should be stored.
	call := SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "look at this image",
		Attachments: []message.Attachment{
			{FileName: "notes.txt", FilePath: "notes.txt", MimeType: "text/plain", Content: []byte("notes")},
			{FileName: "photo.png", FilePath: "photo.png", MimeType: "image/png", Content: []byte("fake-png")},
		},
	}

	msg, err := agent.createUserMessage(ctx, call)
	require.NoError(t, err)

	// All attachments should be present as BinaryContent parts.
	binaryParts := msg.BinaryContent()
	require.Len(t, binaryParts, 2, "both text and image attachments should be stored in the user message")
	require.Equal(t, "notes.txt", binaryParts[0].Path)
	require.Equal(t, "text/plain", binaryParts[0].MIMEType)
	require.Equal(t, "photo.png", binaryParts[1].Path)
	require.Equal(t, "image/png", binaryParts[1].MIMEType)

	// Reload from DB to verify persistence.
	reloaded, err := env.messages.Get(ctx, msg.ID)
	require.NoError(t, err)
	binaryParts = reloaded.BinaryContent()
	require.Len(t, binaryParts, 2, "attachments should survive DB round-trip")
	require.Equal(t, "photo.png", binaryParts[1].Path)
}

func TestPreparePrompt_OrphanedToolUse(t *testing.T) {
	env := testEnv(t)
	sa := testSessionAgent(env, nil, nil, "test prompt")
	agent := sa.(*sessionAgent)

	ctx := t.Context()
	sess, err := env.sessions.Create(ctx, "test")
	require.NoError(t, err)

	// Create a user message.
	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.User,
		Parts: []message.ContentPart{
			message.TextContent{Text: "hello"},
		},
	})
	require.NoError(t, err)

	// Create an assistant message with a tool call but no tool result —
	// this simulates a cancelled/interrupted agent tool call.
	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "let me check"},
			message.ToolCall{
				ID:       "call_orphaned_1",
				Name:     "agent",
				Input:    `{"prompt":"do something"}`,
				Finished: true,
			},
		},
	})
	require.NoError(t, err)

	// Create the next user message (the one that interrupted the tool call).
	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.User,
		Parts: []message.ContentPart{
			message.TextContent{Text: "Fix #2"},
		},
	})
	require.NoError(t, err)

	msgs, err := env.messages.List(ctx, sess.ID)
	require.NoError(t, err)

	history, _ := agent.preparePrompt(msgs, preparePromptOptions{SupportsImages: true})

	// The history must contain a synthetic tool result for the orphaned call.
	found := false
	for _, msg := range history {
		if msg.Role != fantasy.MessageRoleTool {
			continue
		}
		for _, part := range msg.Content {
			if tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
				if tr.ToolCallID == "call_orphaned_1" {
					found = true
					_, isError := tr.Output.(fantasy.ToolResultOutputContentError)
					require.True(t, isError, "orphaned tool result should be an error")
				}
			}
		}
	}
	require.True(t, found, "expected synthetic tool result for orphaned tool call")
}

func TestPreparePrompt_OrphanedToolUseMixed(t *testing.T) {
	env := testEnv(t)
	sa := testSessionAgent(env, nil, nil, "test prompt")
	agent := sa.(*sessionAgent)

	ctx := t.Context()
	sess, err := env.sessions.Create(ctx, "test")
	require.NoError(t, err)

	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.User,
		Parts: []message.ContentPart{
			message.TextContent{Text: "hello"},
		},
	})
	require.NoError(t, err)

	// Assistant with 2 tool calls: one has a result, one is orphaned.
	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ToolCall{
				ID:       "call_ok",
				Name:     "view",
				Input:    `{"path":"/foo"}`,
				Finished: true,
			},
			message.ToolCall{
				ID:       "call_orphaned",
				Name:     "agent",
				Input:    `{"prompt":"search"}`,
				Finished: true,
			},
		},
	})
	require.NoError(t, err)

	// Only one tool result — for call_ok.
	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Tool,
		Parts: []message.ContentPart{
			message.ToolResult{
				ToolCallID: "call_ok",
				Name:       "view",
				Content:    "file contents",
			},
		},
	})
	require.NoError(t, err)

	msgs, err := env.messages.List(ctx, sess.ID)
	require.NoError(t, err)

	history, _ := agent.preparePrompt(msgs, preparePromptOptions{SupportsImages: true})

	// Should have a synthetic result only for the orphaned call.
	var syntheticCount int
	for _, msg := range history {
		if msg.Role != fantasy.MessageRoleTool {
			continue
		}
		for _, part := range msg.Content {
			if tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
				if tr.ToolCallID == "call_orphaned" {
					syntheticCount++
				}
			}
		}
	}
	require.Equal(t, 1, syntheticCount, "expected exactly one synthetic result for the orphaned call")
}

func TestPreparePromptMovesDelayedParallelToolResultsNextToCalls(t *testing.T) {
	body := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		encoded, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(response, err.Error(), http.StatusInternalServerError)
			return
		}
		body <- encoded
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	provider, err := anthropicprovider.New(
		anthropicprovider.WithAPIKey("test-api-key"),
		anthropicprovider.WithBaseURL(server.URL),
	)
	require.NoError(t, err)
	model, err := provider.LanguageModel(t.Context(), "claude-test")
	require.NoError(t, err)
	agent := testSessionAgent(testEnv(t), nil, nil, "test prompt").(*sessionAgent)
	messages := []message.Message{
		{
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.ToolCall{ID: "call_image", Name: "view", Input: `{}`, Finished: true},
				message.ToolCall{ID: "call_error", Name: "search", Input: `{}`, Finished: true},
			},
		},
		{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "interrupting user turn"}}},
		{
			Role: message.Tool,
			Parts: []message.ContentPart{message.ToolResult{
				ToolCallID: "call_image",
				Name:       "view",
				Content:    "loaded image",
				Data:       base64.StdEncoding.EncodeToString([]byte("image")),
				MIMEType:   "image/png",
				Metadata:   `{"source":"tool"}`,
			}},
		},
		{
			Role: message.Tool,
			Parts: []message.ContentPart{message.ToolResult{
				ToolCallID: "call_error",
				Name:       "search",
				Content:    "search failed",
				IsError:    true,
			}},
		},
	}

	history, _ := agent.preparePrompt(messages, preparePromptOptions{SupportsImages: true})
	require.Len(t, history, 4)
	require.Equal(t, fantasy.MessageRoleAssistant, history[0].Role)
	require.Equal(t, fantasy.MessageRoleTool, history[1].Role)
	require.Equal(t, fantasy.MessageRoleTool, history[2].Role)
	require.Equal(t, fantasy.MessageRoleUser, history[3].Role)
	imageResult, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](history[1].Content[0])
	require.True(t, ok)
	require.Equal(t, "call_image", imageResult.ToolCallID)
	media, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](imageResult.Output)
	require.True(t, ok)
	require.Equal(t, base64.StdEncoding.EncodeToString([]byte("image")), media.Data)
	require.Equal(t, "loaded image", media.Text)
	require.Equal(t, `{"source":"tool"}`, imageResult.ClientMetadata)
	errorResult, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](history[2].Content[0])
	require.True(t, ok)
	require.Equal(t, "call_error", errorResult.ToolCallID)
	providerError, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](errorResult.Output)
	require.True(t, ok)
	require.EqualError(t, providerError.Error, "search failed")

	prepared, err := agent.workaroundProviderMediaLimitations(history, Model{
		CatalogModel:      catalog.Model{SupportsImages: true},
		InstructionPolicy: fantasy.InstructionPolicyAnthropic,
	})
	require.NoError(t, err)
	_, err = model.Generate(t.Context(), fantasy.Call{Prompt: prepared})
	require.NoError(t, err)

	var request struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type          string `json:"type"`
				ToolUseID     string `json:"tool_use_id"`
				IsError       bool   `json:"is_error"`
				ResultContent []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"content"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(<-body, &request))
	require.Len(t, request.Messages, 2)
	require.Equal(t, "assistant", request.Messages[0].Role)
	require.Equal(t, "user", request.Messages[1].Role)
	require.GreaterOrEqual(t, len(request.Messages[1].Content), 5)
	require.Equal(t, "tool_result", request.Messages[1].Content[0].Type)
	require.Equal(t, "call_image", request.Messages[1].Content[0].ToolUseID)
	require.Len(t, request.Messages[1].Content[0].ResultContent, 1)
	require.Contains(t, request.Messages[1].Content[0].ResultContent[0].Text, "loaded image")
	require.Contains(t, request.Messages[1].Content[0].ResultContent[0].Text, "see attached file")
	require.Equal(t, "tool_result", request.Messages[1].Content[1].Type)
	require.Equal(t, "call_error", request.Messages[1].Content[1].ToolUseID)
	require.True(t, request.Messages[1].Content[1].IsError)
	require.Equal(t, "image", request.Messages[1].Content[3].Type)
	require.Equal(t, message.User, messages[1].Role)
	require.Equal(t, "loaded image", messages[2].ToolResults()[0].Content)
	require.Equal(t, `{"source":"tool"}`, messages[2].ToolResults()[0].Metadata)
	require.Equal(t, base64.StdEncoding.EncodeToString([]byte("image")), messages[2].ToolResults()[0].Data)
}

func TestPreparePromptKeepsRealAndSyntheticParallelResultsAdjacent(t *testing.T) {
	agent := testSessionAgent(testEnv(t), nil, nil, "test prompt").(*sessionAgent)
	messages := []message.Message{
		{
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.ToolCall{ID: "call_real", Name: "view", Input: `{}`, Finished: true},
				message.ToolCall{ID: "call_missing", Name: "search", Input: `{}`, Finished: true},
			},
		},
		{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "next turn"}}},
		{
			Role: message.Tool,
			Parts: []message.ContentPart{message.ToolResult{
				ToolCallID: "call_real",
				Name:       "view",
				Content:    "real result",
			}},
		},
	}

	history, _ := agent.preparePrompt(messages, preparePromptOptions{SupportsImages: true})
	require.Len(t, history, 4)
	require.Equal(t, fantasy.MessageRoleAssistant, history[0].Role)
	require.Equal(t, fantasy.MessageRoleTool, history[1].Role)
	require.Equal(t, fantasy.MessageRoleTool, history[2].Role)
	require.Equal(t, fantasy.MessageRoleUser, history[3].Role)

	real, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](history[1].Content[0])
	require.True(t, ok)
	require.Equal(t, "call_real", real.ToolCallID)
	realOutput, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](real.Output)
	require.True(t, ok)
	require.Equal(t, "real result", realOutput.Text)
	synthetic, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](history[2].Content[0])
	require.True(t, ok)
	require.Equal(t, "call_missing", synthetic.ToolCallID)
	_, ok = fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](synthetic.Output)
	require.True(t, ok)
	require.Equal(t, message.User, messages[1].Role)
	require.Equal(t, message.Tool, messages[2].Role)
}

func TestPreparePromptRepairsLegacyProviderExecutedToolRoundTrip(t *testing.T) {
	agent := testSessionAgent(testEnv(t), nil, nil, "test prompt").(*sessionAgent)
	messages := []message.Message{
		{
			Role: message.Assistant,
			Parts: []message.ContentPart{message.ToolCall{
				ID:       "server_call",
				Name:     "web_search",
				Input:    `{"query":"current result"}`,
				Finished: true,
			}},
		},
		{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "later turn"}}},
		{
			Role: message.Tool,
			Parts: []message.ContentPart{message.ToolResult{
				ToolCallID:       "server_call",
				Name:             "web_search",
				ProviderExecuted: true,
			}},
		},
	}

	history, _ := agent.preparePrompt(messages, preparePromptOptions{SupportsImages: true})
	require.Len(t, history, 2)
	require.Equal(t, fantasy.MessageRoleAssistant, history[0].Role)
	require.Len(t, history[0].Content, 2)
	call, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](history[0].Content[0])
	require.True(t, ok)
	require.True(t, call.ProviderExecuted)
	result, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](history[0].Content[1])
	require.True(t, ok)
	require.True(t, result.ProviderExecuted)
	require.Equal(t, fantasy.MessageRoleUser, history[1].Role)
	require.False(t, messages[0].ToolCalls()[0].ProviderExecuted)
	require.Equal(t, message.Tool, messages[2].Role)
}

func TestRunPersistsProviderExecutedToolRoundTripInAssistant(t *testing.T) {
	env := testEnv(t)
	model := &providerExecutedStreamModel{}
	agent := testSessionAgent(env, model, model, "system")
	sess, err := env.sessions.Create(t.Context(), "provider tool")
	require.NoError(t, err)

	_, err = agent.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "search"})
	require.NoError(t, err)
	persisted, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, persisted, 2)
	require.Equal(t, message.User, persisted[0].Role)
	require.Equal(t, message.Assistant, persisted[1].Role)
	require.Len(t, persisted[1].ToolCalls(), 1)
	require.True(t, persisted[1].ToolCalls()[0].ProviderExecuted)
	require.Len(t, persisted[1].ToolResults(), 1)
	require.True(t, persisted[1].ToolResults()[0].ProviderExecuted)

	projected := persisted[1].ToAIMessage()
	require.Len(t, projected, 1)
	require.Equal(t, fantasy.MessageRoleAssistant, projected[0].Role)
	require.Len(t, projected[0].Content, 2)
	call, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](projected[0].Content[0])
	require.True(t, ok)
	require.True(t, call.ProviderExecuted)
	result, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](projected[0].Content[1])
	require.True(t, ok)
	require.True(t, result.ProviderExecuted)
}

func TestWorkaroundProviderMediaLimitations_TextOnlyModel(t *testing.T) {
	env := testEnv(t)
	sa := testSessionAgent(env, nil, nil, "test prompt")
	agent := sa.(*sessionAgent)

	pngBase64 := base64.StdEncoding.EncodeToString([]byte("fake-png-data"))

	messages := []fantasy.Message{
		{
			Role: fantasy.MessageRoleTool,
			Content: []fantasy.MessagePart{
				fantasy.ToolResultPart{
					ToolCallID: "call_1",
					Output: fantasy.ToolResultOutputContentMedia{
						Data:      pngBase64,
						MediaType: "image/png",
					},
				},
			},
		},
	}

	// Non-Anthropic provider, no image support — should replace media with
	// a text placeholder and not create a synthetic user message.
	largeModel := Model{
		ModelCfg: config.SelectedModel{Provider: "openai"},
		CatalogModel: catalog.Model{
			SupportsImages: false,
		},
	}

	result, err := agent.workaroundProviderMediaLimitations(messages, largeModel)
	require.NoError(t, err)

	// Should produce exactly one message: the tool message with a text
	// placeholder. No synthetic user message with FilePart.
	require.Len(t, result, 1)
	require.Equal(t, fantasy.MessageRoleTool, result[0].Role)

	tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](result[0].Content[0])
	require.True(t, ok)
	_, ok = fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](tr.Output)
	require.True(t, ok)
}

func TestWorkaroundProviderMediaLimitations_VisionModel(t *testing.T) {
	env := testEnv(t)
	sa := testSessionAgent(env, nil, nil, "test prompt")
	agent := sa.(*sessionAgent)

	pngBase64 := base64.StdEncoding.EncodeToString([]byte("fake-png-data"))

	messages := []fantasy.Message{
		{
			Role: fantasy.MessageRoleTool,
			Content: []fantasy.MessagePart{
				fantasy.ToolResultPart{
					ToolCallID: "call_1",
					Output: fantasy.ToolResultOutputContentMedia{
						Data:      pngBase64,
						MediaType: "image/png",
					},
				},
			},
		},
	}

	// Non-Anthropic provider, image support — should create a synthetic
	// user message with FilePart.
	largeModel := Model{
		ModelCfg: config.SelectedModel{Provider: "openai"},
		CatalogModel: catalog.Model{
			SupportsImages: true,
		},
	}

	result, err := agent.workaroundProviderMediaLimitations(messages, largeModel)
	require.NoError(t, err)

	// Should produce two messages: tool message with placeholder text,
	// and synthetic user message with FilePart.
	require.Len(t, result, 2)
	require.Equal(t, fantasy.MessageRoleTool, result[0].Role)
	require.Equal(t, fantasy.MessageRoleUser, result[1].Role)

	// The tool message should have text placeholder.
	tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](result[0].Content[0])
	require.True(t, ok)
	textOutput, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](tr.Output)
	require.True(t, ok)
	require.Contains(t, textOutput.Text, "see attached file")

	// The synthetic user message should contain a TextPart and a FilePart.
	require.Len(t, result[1].Content, 2)
	file, ok := fantasy.AsMessagePart[fantasy.FilePart](result[1].Content[1])
	require.True(t, ok)
	require.Equal(t, "image/png", file.MediaType)
}

func TestWorkaroundProviderMediaLimitationsKeepsParallelAnthropicToolResultsFirst(t *testing.T) {
	body := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		encoded, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(response, err.Error(), http.StatusInternalServerError)
			return
		}
		body <- encoded
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	provider, err := anthropicprovider.New(
		anthropicprovider.WithAPIKey("test-api-key"),
		anthropicprovider.WithBaseURL(server.URL),
	)
	require.NoError(t, err)
	model, err := provider.LanguageModel(t.Context(), "claude-test")
	require.NoError(t, err)
	agent := testSessionAgent(testEnv(t), nil, nil, "test prompt").(*sessionAgent)
	messages := []fantasy.Message{
		{
			Role: fantasy.MessageRoleAssistant,
			Content: []fantasy.MessagePart{
				fantasy.ToolCallPart{ToolCallID: "call_image", ToolName: "view", Input: `{}`},
				fantasy.ToolCallPart{ToolCallID: "call_text", ToolName: "search", Input: `{}`},
			},
		},
		{
			Role: fantasy.MessageRoleTool,
			Content: []fantasy.MessagePart{fantasy.ToolResultPart{
				ToolCallID: "call_image",
				Output: fantasy.ToolResultOutputContentMedia{
					Data:      base64.StdEncoding.EncodeToString([]byte("image")),
					MediaType: "image/png",
				},
			}},
		},
		{
			Role: fantasy.MessageRoleTool,
			Content: []fantasy.MessagePart{fantasy.ToolResultPart{
				ToolCallID: "call_text",
				Output:     fantasy.ToolResultOutputContentText{Text: "text result"},
			}},
		},
	}

	prepared, err := agent.workaroundProviderMediaLimitations(messages, Model{
		CatalogModel:      catalog.Model{SupportsImages: true},
		InstructionPolicy: fantasy.InstructionPolicyAnthropic,
	})
	require.NoError(t, err)
	_, err = model.Generate(t.Context(), fantasy.Call{Prompt: prepared})
	require.NoError(t, err)

	var request struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string `json:"type"`
				ToolUseID string `json:"tool_use_id"`
			} `json:"content"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(<-body, &request))
	require.Len(t, request.Messages, 2)
	require.Equal(t, "assistant", request.Messages[0].Role)
	require.Equal(t, "user", request.Messages[1].Role)
	require.GreaterOrEqual(t, len(request.Messages[1].Content), 2)
	require.Equal(t, "tool_result", request.Messages[1].Content[0].Type)
	require.Equal(t, "call_image", request.Messages[1].Content[0].ToolUseID)
	require.Equal(t, "tool_result", request.Messages[1].Content[1].Type)
	require.Equal(t, "call_text", request.Messages[1].Content[1].ToolUseID)
}

func TestWorkaroundProviderMediaLimitationsAgesToolImagesByUserTurns(t *testing.T) {
	env := testEnv(t)
	agent := testSessionAgent(env, nil, nil, "test prompt").(*sessionAgent)
	largeModel := Model{CatalogModel: catalog.Model{SupportsImages: true}}
	toolMessage := func() fantasy.Message {
		return fantasy.Message{
			Role: fantasy.MessageRoleTool,
			Content: []fantasy.MessagePart{fantasy.ToolResultPart{
				ToolCallID: "call_1",
				Output: fantasy.ToolResultOutputContentMedia{
					Data:      base64.StdEncoding.EncodeToString([]byte("tool-image")),
					MediaType: "image/png",
				},
			}},
		}
	}
	imageCount := func(message fantasy.Message) int {
		count := 0
		for _, part := range message.Content {
			if file, ok := fantasy.AsMessagePart[fantasy.FilePart](part); ok && file.MediaType == "image/png" {
				count++
			}
		}
		return count
	}

	t.Run("synthetic user message does not consume a turn", func(t *testing.T) {
		messages := []fantasy.Message{
			{
				Role: fantasy.MessageRoleUser,
				Content: []fantasy.MessagePart{
					fantasy.TextPart{Text: "direct image"},
					fantasy.FilePart{Filename: "direct.png", MediaType: "image/png", Data: []byte("direct")},
				},
			},
			toolMessage(),
		}
		for index := range retainedImageHistoryTurns {
			messages = append(messages, fantasy.NewUserMessage(fmt.Sprintf("later %d", index)))
		}

		result, err := agent.workaroundProviderMediaLimitations(messages, largeModel)
		require.NoError(t, err)
		require.Equal(t, 1, imageCount(result[0]))
		require.Equal(t, fantasy.MessageRoleUser, result[2].Role)
		require.Equal(t, 1, imageCount(result[2]))
	})

	t.Run("tool image is omitted after four later user turns", func(t *testing.T) {
		messages := []fantasy.Message{toolMessage()}
		for index := range retainedImageHistoryTurns + 1 {
			messages = append(messages, fantasy.NewUserMessage(fmt.Sprintf("later %d", index)))
		}

		result, err := agent.workaroundProviderMediaLimitations(messages, largeModel)
		require.NoError(t, err)
		require.Equal(t, fantasy.MessageRoleUser, result[1].Role)
		require.Zero(t, imageCount(result[1]))
		marker, ok := fantasy.AsMessagePart[fantasy.TextPart](result[1].Content[1])
		require.True(t, ok)
		require.Equal(t, omittedImageHistoryMessage, marker.Text)
	})
}

func TestWorkaroundProviderMediaLimitationsNormalizesCodexToolImages(t *testing.T) {
	env := testEnv(t)
	sa := testSessionAgent(env, nil, nil, "test prompt")
	agent := sa.(*sessionAgent)

	var encoded bytes.Buffer
	require.NoError(t, jpeg.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2048, 2048)), nil))
	messages := []fantasy.Message{{
		Role: fantasy.MessageRoleTool,
		Content: []fantasy.MessagePart{fantasy.ToolResultPart{
			ToolCallID: "call_1",
			Output: fantasy.ToolResultOutputContentMedia{
				Data:      base64.StdEncoding.EncodeToString(encoded.Bytes()),
				MediaType: "image/jpeg",
			},
		}},
	}}
	registration, registered := integratedRegistration(t, "codex")
	require.True(t, registered)
	policy, ok := imageattachment.PolicyFor(registration)
	require.True(t, ok)
	largeModel := Model{
		ModelCfg:     config.SelectedModel{Provider: "codex"},
		CatalogModel: catalog.Model{SupportsImages: true},
		ImagePolicy:  &policy,
	}

	result, err := agent.workaroundProviderMediaLimitations(messages, largeModel)
	require.NoError(t, err)
	require.Len(t, result, 2)
	file, ok := fantasy.AsMessagePart[fantasy.FilePart](result[1].Content[1])
	require.True(t, ok)
	config, _, err := image.DecodeConfig(bytes.NewReader(file.Data))
	require.NoError(t, err)
	require.LessOrEqual(t, ((config.Width+31)/32)*((config.Height+31)/32), 2500)
}

func TestWorkaroundProviderMediaLimitationsUsesStoredImagePolicy(t *testing.T) {
	env := testEnv(t)
	sa := testSessionAgent(env, nil, nil, "test prompt")
	agent := sa.(*sessionAgent)

	var encoded bytes.Buffer
	require.NoError(t, jpeg.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2048, 2048)), nil))
	messages := []fantasy.Message{{
		Role: fantasy.MessageRoleTool,
		Content: []fantasy.MessagePart{fantasy.ToolResultPart{
			ToolCallID: "call_1",
			Output: fantasy.ToolResultOutputContentMedia{
				Data:      base64.StdEncoding.EncodeToString(encoded.Bytes()),
				MediaType: "image/jpeg",
			},
		}},
	}}
	largeModel := Model{
		ModelCfg:     config.SelectedModel{Provider: "codex"},
		CatalogModel: catalog.Model{SupportsImages: true},
	}

	result, err := agent.workaroundProviderMediaLimitations(messages, largeModel)
	require.NoError(t, err)
	require.Len(t, result, 2)
	file, ok := fantasy.AsMessagePart[fantasy.FilePart](result[1].Content[1])
	require.True(t, ok)
	decoded, _, err := image.DecodeConfig(bytes.NewReader(file.Data))
	require.NoError(t, err)
	require.Equal(t, 2048, decoded.Width)
	require.Equal(t, 2048, decoded.Height)
}

func TestProviderRetryLogFields(t *testing.T) {
	t.Run("nil provider error", func(t *testing.T) {
		fields := providerRetryLogFields(nil, 2*time.Second)
		require.Equal(t, []any{"retry_delay", "2s"}, fields)
	})

	t.Run("provider error with title and message", func(t *testing.T) {
		fields := providerRetryLogFields(&fantasy.ProviderError{
			StatusCode: 429,
			Title:      "rate limit",
			Message:    "too many requests",
		}, 1500*time.Millisecond)
		require.Equal(t, []any{
			"retry_delay", "1.5s",
			"status_code", 429,
			"title", "rate limit",
			"message", "too many requests",
		}, fields)
	})

	t.Run("provider error without optional strings", func(t *testing.T) {
		fields := providerRetryLogFields(&fantasy.ProviderError{
			StatusCode: 503,
		}, time.Second)
		require.Equal(t, []any{
			"retry_delay", "1s",
			"status_code", 503,
		}, fields)
	})
}
