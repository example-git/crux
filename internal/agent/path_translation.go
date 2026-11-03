package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/imageattachment"
	"github.com/example-git/crux/internal/pathtranslate"
	"github.com/example-git/crux/internal/session"
)

type pathTranslationResolver func(context.Context) (*pathtranslate.Translator, error)

type pathTranslationOwnerKey struct{}

type pathTranslationCapturedKey struct{}

type pathTranslationOwner struct {
	provider string
	model    string
}

type pathTranslationChain struct {
	sessionID string
	owner     pathTranslationOwner
}

type translatedLanguageModel struct {
	owner pathTranslationOwner
	fantasy.LanguageModel
	resolve     pathTranslationResolver
	observe     func(context.Context, *pathtranslate.Translator)
	imagePolicy *imageattachment.Policy
}

func (c *coordinator) bindPathTranslation(model Model) Model {
	if c.pathTranslations == nil {
		return model
	}
	selected := model.ModelCfg
	model.PathTranslation = func(ctx context.Context) (*pathtranslate.Translator, error) {
		names, err := c.pathTranslations.Names(ctx)
		if err != nil || names == nil {
			return nil, err
		}
		policy, err := c.effectivePathTranslation(ctx, tools.GetSessionFromContext(ctx))
		if err != nil {
			return nil, err
		}
		return pathtranslate.New(names, policy.Settings, c.cfg.WorkingDir())
	}
	owner := pathTranslationOwner{provider: selected.Provider, model: selected.Model}
	original := model.Model
	model.PathTranslationState = func(ctx context.Context, translator *pathtranslate.Translator) {
		sessionID := tools.GetSessionFromContext(ctx)
		if sessionID == "" {
			return
		}
		chain := pathTranslationChain{sessionID: sessionID, owner: owner}
		fingerprint := translator.Fingerprint()
		c.pathTranslationMu.Lock()
		defer c.pathTranslationMu.Unlock()
		previous, known := c.pathTranslationPolicies[chain]
		if previous != fingerprint && (known || fingerprint != "") {
			conversationID := session.HashID(sessionID)
			if c.responsesContinuations != nil {
				c.responsesContinuations.ResetConversation(conversationID)
			}
			if resetter, ok := original.(interface{ ResetConversationChain(string) }); ok {
				resetter.ResetConversationChain(conversationID)
			}
		}
		if c.pathTranslationPolicies == nil {
			c.pathTranslationPolicies = make(map[pathTranslationChain]string)
		}
		c.pathTranslationPolicies[chain] = fingerprint
	}
	model.Model = translatedLanguageModel{LanguageModel: model.Model, resolve: model.PathTranslation, owner: owner, observe: model.PathTranslationState, imagePolicy: model.ImagePolicy}
	return model
}

func (c *coordinator) effectivePathTranslation(ctx context.Context, sessionID string) (pathtranslate.Policy, error) {
	seen := make(map[string]bool)
	for sessionID != "" {
		if seen[sessionID] {
			return pathtranslate.Policy{}, errors.New("path translation session ancestry contains a cycle")
		}
		seen[sessionID] = true
		policy, found, err := c.pathTranslations.Load(ctx, pathtranslate.Scope{Kind: "session", SessionID: sessionID})
		if err != nil {
			return pathtranslate.Policy{}, err
		}
		if found && !policy.Settings.Inherit {
			return policy, nil
		}
		if c.sessions == nil {
			return pathtranslate.Policy{}, errors.New("path translation session owner is unavailable")
		}
		current, err := c.sessions.Get(ctx, sessionID)
		if err != nil {
			return pathtranslate.Policy{}, err
		}
		sessionID = current.ParentSessionID
	}
	return pathtranslate.Policy{}, nil
}

func (m translatedLanguageModel) translator(ctx context.Context) (*pathtranslate.Translator, error) {
	translator := pathtranslate.FromContext(ctx)
	owner, _ := ctx.Value(pathTranslationOwnerKey{}).(pathTranslationOwner)
	captured, _ := ctx.Value(pathTranslationCapturedKey{}).(bool)
	if translator == nil && !captured || owner != m.owner {
		var err error
		translator, err = m.resolve(ctx)
		if err != nil {
			return nil, err
		}
	}
	if m.observe != nil {
		m.observe(ctx, translator)
	}
	return translator, nil
}

func (m translatedLanguageModel) prompt(ctx context.Context, prompt fantasy.Prompt) (fantasy.Prompt, error) {
	translator, err := m.translator(ctx)
	if err != nil || translator == nil {
		return prompt, err
	}
	return m.translatedPrompt(ctx, translator, prompt)
}

func (m translatedLanguageModel) translatedPrompt(ctx context.Context, translator *pathtranslate.Translator, prompt fantasy.Prompt) (fantasy.Prompt, error) {
	restoredImage := false
	for _, msg := range prompt {
		for _, part := range msg.Content {
			if file, ok := fantasy.AsMessagePart[fantasy.FilePart](part); ok && file.PathTranslationOriginal != nil && file.PathTranslationPolicy != translator.Fingerprint() {
				restoredImage = true
			}
		}
	}
	translated, err := translator.Prompt(ctx, prompt)
	if err != nil {
		return nil, err
	}
	if restoredImage {
		return normalizeProviderPromptImages(translated, m.imagePolicy)
	}
	for _, msg := range translated {
		for _, part := range msg.Content {
			if file, ok := fantasy.AsMessagePart[fantasy.FilePart](part); ok && file.NeedsImageNormalization {
				return normalizeProviderPromptImages(translated, m.imagePolicy)
			}
			if result, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
				if _, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](result.Output); ok {
					return normalizeProviderPromptImages(translated, m.imagePolicy)
				}
			}
		}
	}
	return translated, nil
}

func (m translatedLanguageModel) call(ctx context.Context, call fantasy.Call) (fantasy.Call, error) {
	translator, err := m.translator(ctx)
	if err != nil || translator == nil {
		return call, err
	}
	call.Prompt, err = m.translatedPrompt(ctx, translator, call.Prompt)
	if err != nil {
		return call, err
	}
	call.Tools, err = translatedFunctionTools(ctx, translator, call.Tools)
	return call, err
}

func (m translatedLanguageModel) captureContext(ctx context.Context) (context.Context, *pathtranslate.Translator, error) {
	translator, err := m.translator(ctx)
	if err != nil {
		return ctx, nil, err
	}
	ctx = context.WithValue(ctx, pathTranslationOwnerKey{}, m.owner)
	ctx = context.WithValue(ctx, pathTranslationCapturedKey{}, true)
	return pathtranslate.WithTranslator(ctx, translator), translator, nil
}

func (m translatedLanguageModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	ctx, translator, err := m.captureContext(ctx)
	if err != nil {
		return nil, err
	}
	call, err = m.call(ctx, call)
	if err != nil {
		return nil, err
	}
	response, err := m.LanguageModel.Generate(ctx, call)
	if err != nil {
		return response, err
	}
	return response, translator.RememberResponse(ctx, response)
}

func (m translatedLanguageModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	ctx, translator, err := m.captureContext(ctx)
	if err != nil {
		return nil, err
	}
	call, err = m.call(ctx, call)
	if err != nil {
		return nil, err
	}
	stream, err := m.LanguageModel.Stream(ctx, call)
	if err != nil {
		return nil, err
	}
	return func(yield func(fantasy.StreamPart) bool) {
		for part := range stream {
			if err := translator.RememberMetadata(ctx, part.ProviderMetadata); err != nil {
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: err})
				return
			}
			if !yield(part) {
				return
			}
		}
	}, nil
}

func (m translatedLanguageModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	ctx, translator, err := m.captureContext(ctx)
	if err != nil {
		return nil, err
	}
	call, err = m.objectCall(ctx, translator, call)
	if err != nil {
		return nil, err
	}
	response, err := m.LanguageModel.GenerateObject(ctx, call)
	if err != nil || response == nil {
		return response, err
	}
	return response, translator.RememberMetadata(ctx, response.ProviderMetadata)
}

func (m translatedLanguageModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	ctx, translator, err := m.captureContext(ctx)
	if err != nil {
		return nil, err
	}
	call, err = m.objectCall(ctx, translator, call)
	if err != nil {
		return nil, err
	}
	stream, err := m.LanguageModel.StreamObject(ctx, call)
	if err != nil {
		return nil, err
	}
	return func(yield func(fantasy.ObjectStreamPart) bool) {
		for part := range stream {
			if err := translator.RememberMetadata(ctx, part.ProviderMetadata); err != nil {
				yield(fantasy.ObjectStreamPart{Type: fantasy.ObjectStreamPartTypeError, Error: err})
				return
			}
			if !yield(part) {
				return
			}
		}
	}, nil
}

func (m translatedLanguageModel) HandlesAuthenticationRefresh() bool {
	owner, ok := m.LanguageModel.(interface{ HandlesAuthenticationRefresh() bool })
	return ok && owner.HandlesAuthenticationRefresh()
}

func (m translatedLanguageModel) ResetConversationChain(sessionID string) {
	if owner, ok := m.LanguageModel.(interface{ ResetConversationChain(string) }); ok {
		owner.ResetConversationChain(sessionID)
	}
}

type translatedTool struct {
	fantasy.AgentTool
}

func (t translatedTool) Run(ctx context.Context, call fantasy.ToolCall) (response fantasy.ToolResponse, resultErr error) {
	translator := pathtranslate.FromContext(ctx)
	if translator == nil {
		return t.AgentTool.Run(ctx, call)
	}
	toolName := call.Name
	if toolName == "" {
		toolName = t.Info().Name
	}
	var resolvedInput string
	defer func() {
		if translator.Fingerprint() == "" {
			return
		}
		if recovered := recover(); recovered != nil {
			response = fantasy.NewTextErrorResponse(fmt.Sprintf("tool %q panicked: %v\n\n%s", toolName, recovered, debug.Stack()))
			resultErr = nil
		}
		if resultErr != nil {
			response.Type, response.Content, response.IsError = "text", resultErr.Error(), true
		}
		translated, err := translator.ToolResponse(ctx, toolName, resolvedInput, response)
		if err != nil {
			response = fantasy.ToolResponse{}
			text, translationErr := translator.Text(ctx, err.Error())
			if translationErr != nil {
				text = "could not prepare the tool result"
			}
			resultErr = translatedToolError{err: err, text: "translate tool result: " + text}
			return
		}
		response = translated
		if resultErr != nil {
			resultErr = translatedToolError{err: resultErr, text: response.Content}
		}
	}()
	input, err := translator.JSON(ctx, call.Input, true)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("resolve tool paths: %v", err)), nil
	}
	if err := translator.CheckToolInput(input); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	resolvedInput = input
	call.Input = input
	return t.AgentTool.Run(ctx, call)
}

type translatedToolError struct {
	err  error
	text string
}

func (e translatedToolError) Error() string { return e.text }

func (e translatedToolError) Unwrap() error { return e.err }

func translateTools(source []fantasy.AgentTool) []fantasy.AgentTool {
	result := make([]fantasy.AgentTool, len(source))
	for index, tool := range source {
		result[index] = translatedTool{AgentTool: tool}
	}
	return result
}

func preparePathTranslation(ctx context.Context, model Model) (context.Context, error) {
	if model.PathTranslation == nil {
		return ctx, nil
	}
	translator, err := model.PathTranslation(ctx)
	if err != nil {
		return ctx, err
	}
	ctx = context.WithValue(ctx, pathTranslationOwnerKey{}, pathTranslationOwner{provider: model.ModelCfg.Provider, model: model.ModelCfg.Model})
	return pathtranslate.WithTranslator(ctx, translator), nil
}

func translatedFunctionTools(ctx context.Context, translator *pathtranslate.Translator, source []fantasy.Tool) ([]fantasy.Tool, error) {
	result := append([]fantasy.Tool(nil), source...)
	for index, tool := range source {
		var function fantasy.FunctionTool
		switch value := tool.(type) {
		case fantasy.FunctionTool:
			function = value
		case *fantasy.FunctionTool:
			function = *value
		default:
			continue
		}
		var err error
		function.Description, err = translator.Text(ctx, function.Description)
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(function.InputSchema)
		if err != nil {
			return nil, err
		}
		translated, err := translator.JSON(ctx, string(encoded), false)
		if err != nil {
			return nil, err
		}
		function.InputSchema = nil
		if err := json.Unmarshal([]byte(translated), &function.InputSchema); err != nil {
			return nil, err
		}
		result[index] = function
	}
	return result, nil
}

func (m translatedLanguageModel) objectCall(ctx context.Context, translator *pathtranslate.Translator, call fantasy.ObjectCall) (fantasy.ObjectCall, error) {
	if translator == nil {
		return call, nil
	}
	var err error
	call.Prompt, err = m.translatedPrompt(ctx, translator, call.Prompt)
	if err != nil {
		return call, err
	}
	call.SchemaDescription, err = translator.Text(ctx, call.SchemaDescription)
	if err != nil {
		return call, err
	}
	encoded, err := json.Marshal(call.Schema)
	if err != nil {
		return call, err
	}
	translated, err := translator.JSON(ctx, string(encoded), false)
	if err != nil {
		return call, err
	}
	call.Schema = fantasy.Schema{}
	err = json.Unmarshal([]byte(translated), &call.Schema)
	return call, err
}
