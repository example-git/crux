package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/jpeg"
	"log/slog"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/message"
	codexresponses "github.com/example-git/crux/internal/oauth/codex/responses"
	"github.com/example-git/crux/internal/session"
	"github.com/stretchr/testify/require"

	_ "github.com/joho/godotenv/autoload"
)

func TestMain(m *testing.M) {
	slog.SetLogLoggerLevel(slog.LevelError)
	config.DefaultProviderProfile = string(config.ProviderProfileIntegrated)
	m.Run()
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
	model := Model{
		ProviderOptions:    providerOptions,
		SystemPromptPrefix: "small-provider-prefix",
		OnAuthRefresh: func(context.Context, *fantasy.ProviderError) error {
			refreshCalls++
			return nil
		},
	}

	call := auxiliaryStreamCall("prompt", map[string]string{"x-request-purpose": "title"}, model, func() fantasy.LanguageModel {
		modelProviderCalls++
		return nil
	})

	require.Equal(t, providerOptions, call.ProviderOptions)
	require.Equal(t, "title", call.Headers["x-request-purpose"])
	require.NoError(t, call.OnAuthRefresh(t.Context(), nil))
	require.Equal(t, 1, refreshCalls)
	require.Nil(t, call.ModelProvider())
	require.Equal(t, 1, modelProviderCalls)

	_, prepared, err := call.PrepareStep(t.Context(), fantasy.PrepareStepFunctionOptions{
		Messages: []fantasy.Message{fantasy.NewUserMessage("user message")},
	})
	require.NoError(t, err)
	require.Len(t, prepared.Messages, 2)
	require.Equal(t, fantasy.MessageRoleSystem, prepared.Messages[0].Role)
	prefix, ok := prepared.Messages[0].Content[0].(fantasy.TextPart)
	require.True(t, ok)
	require.Equal(t, "small-provider-prefix", prefix.Text)
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
		CatwalkCfg: catwalk.Model{
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
		CatwalkCfg: catwalk.Model{
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
	largeModel := Model{
		ModelCfg: config.SelectedModel{Provider: "codex"},
		CatwalkCfg: catwalk.Model{
			SupportsImages: true,
		},
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
