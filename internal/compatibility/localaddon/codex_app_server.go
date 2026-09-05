package localaddon

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/compatibility"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/example-git/crux/internal/version"
	"github.com/google/uuid"
)

const codexServerReadyTimeout = 10 * time.Second

var codexClientFactory = client.DefaultClient

type codexNativeWorkspace struct {
	client *client.Client
	value  *proto.Workspace
	events <-chan any
	cancel context.CancelFunc
}

type codexThreadBinding struct {
	codexID   string
	nativeID  string
	workspace *codexNativeWorkspace
	policy    codexExecutionPolicy
	ephemeral bool
}

type codexExecutionPolicy struct {
	approvalPolicy    string
	approvalsReviewer string
	sandbox           string
	permissionMode    proto.AgentPermissionMode
}

type codexNativeBridge struct {
	ctx           context.Context
	invocation    compatibility.Invocation
	request       compatibility.Request
	server        *exec.Cmd
	workspaces    map[string]*codexNativeWorkspace
	threads       map[string]*codexThreadBinding
	models        map[*codexNativeWorkspace]map[string]config.SelectedModel
	modelOwners   map[*codexNativeWorkspace]map[string]providerregistry.RegistrationOwner
	selectedModel map[*codexNativeWorkspace]config.SelectedModel
}

func newCodexNativeBridge(ctx context.Context, invocation compatibility.Invocation, request compatibility.Request) (*codexNativeBridge, error) {
	b := &codexNativeBridge{ctx: ctx, invocation: invocation, request: request, workspaces: make(map[string]*codexNativeWorkspace), threads: make(map[string]*codexThreadBinding), models: make(map[*codexNativeWorkspace]map[string]config.SelectedModel), modelOwners: make(map[*codexNativeWorkspace]map[string]providerregistry.RegistrationOwner), selectedModel: make(map[*codexNativeWorkspace]config.SelectedModel)}
	probe, err := codexClientFactory(request.WorkingDir)
	if err != nil {
		return nil, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	err = probe.Health(probeCtx)
	cancel()
	if err == nil {
		if _, workspaceErr := b.workspace(request.WorkingDir); workspaceErr != nil {
			return nil, workspaceErr
		}
		return b, nil
	}
	cmd := exec.CommandContext(context.Background(), invocation.Executable, "server")
	cmd.Dir = request.WorkingDir
	cmd.Env = invocation.Env
	cmd.Stdout = invocation.Stderr
	cmd.Stderr = invocation.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start Crux server: %w", err)
	}
	b.server = cmd
	go func() { _ = cmd.Wait() }()
	deadline := time.Now().Add(codexServerReadyTimeout)
	for {
		attemptCtx, attemptCancel := context.WithTimeout(ctx, 200*time.Millisecond)
		err = probe.Health(attemptCtx)
		attemptCancel()
		if err == nil {
			if _, workspaceErr := b.workspace(request.WorkingDir); workspaceErr != nil {
				b.Close()
				return nil, workspaceErr
			}
			return b, nil
		}
		if time.Now().After(deadline) {
			b.Close()
			return nil, fmt.Errorf("wait for Crux server: %w", err)
		}
		select {
		case <-ctx.Done():
			b.Close()
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (b *codexNativeBridge) Close() {
	for _, binding := range b.threads {
		if binding.ephemeral {
			_ = binding.workspace.client.DeleteSession(context.Background(), binding.workspace.value.ID, binding.nativeID)
		}
	}
	for _, workspace := range b.workspaces {
		workspace.cancel()
		if err := workspace.client.RetireClient(context.Background()); err != nil {
			_ = workspace.client.DeleteWorkspace(context.Background(), workspace.value.ID)
		}
	}
	if b.server != nil && b.server.Process != nil {
		_ = b.server.Process.Kill()
	}
}

func (b *codexNativeBridge) workspace(cwd string) (*codexNativeWorkspace, error) {
	if cwd == "" {
		cwd = b.request.WorkingDir
	}
	if !filepath.IsAbs(cwd) {
		return nil, errors.New("cwd must be an absolute path")
	}
	cwd = filepath.Clean(cwd)
	if existing := b.workspaces[cwd]; existing != nil {
		return existing, nil
	}
	c, err := codexClientFactory(cwd)
	if err != nil {
		return nil, err
	}
	ws, err := c.CreateWorkspace(b.ctx, proto.Workspace{Path: cwd, Env: b.invocation.Env, Version: version.Version})
	if err != nil {
		return nil, err
	}
	eventCtx, cancel := context.WithCancel(b.ctx)
	events, err := c.SubscribeEvents(eventCtx, ws.ID)
	if err != nil {
		cancel()
		_ = c.DeleteWorkspace(context.Background(), ws.ID)
		return nil, err
	}
	result := &codexNativeWorkspace{client: c, value: ws, events: events, cancel: cancel}
	b.workspaces[cwd] = result
	return result, nil
}

func codexUUIDv7(workspacePath string, sess *proto.Session) string {
	digest := sha256.Sum256([]byte(workspacePath + "\x00" + sess.ID))
	var value uuid.UUID
	timestamp := uint64(max(sess.CreatedAt, 0)) * 1000
	value[0] = byte(timestamp >> 40)
	value[1] = byte(timestamp >> 32)
	value[2] = byte(timestamp >> 24)
	value[3] = byte(timestamp >> 16)
	value[4] = byte(timestamp >> 8)
	value[5] = byte(timestamp)
	value[6] = 0x70 | digest[0]&0x0f
	value[7] = digest[1]
	value[8] = 0x80 | digest[2]&0x3f
	copy(value[9:], digest[3:10])
	return value.String()
}

func (b *codexNativeBridge) bindThread(workspace *codexNativeWorkspace, sess *proto.Session) *codexThreadBinding {
	codexID := codexUUIDv7(workspace.value.Path, sess)
	if binding := b.threads[codexID]; binding != nil {
		return binding
	}
	binding := &codexThreadBinding{codexID: codexID, nativeID: sess.ID, workspace: workspace, policy: defaultCodexExecutionPolicy()}
	b.threads[codexID] = binding
	return binding
}

func (b *codexNativeBridge) findThread(ctx context.Context, threadID, cwd string) (*codexThreadBinding, *proto.Session, error) {
	if binding := b.threads[threadID]; binding != nil {
		sess, err := binding.workspace.client.GetSession(ctx, binding.workspace.value.ID, binding.nativeID)
		return binding, sess, err
	}
	workspaces := make([]*codexNativeWorkspace, 0, len(b.workspaces))
	if cwd != "" {
		workspace, err := b.workspace(cwd)
		if err != nil {
			return nil, nil, err
		}
		workspaces = append(workspaces, workspace)
	} else {
		for _, workspace := range b.workspaces {
			workspaces = append(workspaces, workspace)
		}
	}
	for _, workspace := range workspaces {
		sessions, err := workspace.client.ListSessions(ctx, workspace.value.ID)
		if err != nil {
			continue
		}
		for i := range sessions {
			sess := &sessions[i]
			binding := b.bindThread(workspace, sess)
			// Accept the formerly exposed native ID during the compatibility
			// transition, but always return the canonical Codex UUIDv7.
			if binding.codexID == threadID || binding.nativeID == threadID {
				return binding, sess, nil
			}
		}
	}
	return nil, nil, errors.New("thread not found")
}

func defaultCodexExecutionPolicy() codexExecutionPolicy {
	return codexExecutionPolicy{
		approvalPolicy:    "on-request",
		approvalsReviewer: "user",
		sandbox:           "workspace-write",
		permissionMode:    proto.AgentPermissionInteractive,
	}
}

func codexSandboxResponseType(sandbox string) string {
	switch sandbox {
	case "read-only":
		return "readOnly"
	case "danger-full-access":
		return "dangerFullAccess"
	default:
		return "workspaceWrite"
	}
}

func parseCodexExecutionPolicy(approvalPolicy, approvalsReviewer string, sandbox json.RawMessage, inherited codexExecutionPolicy) (codexExecutionPolicy, error) {
	policy := inherited
	if policy.approvalPolicy == "" {
		policy = defaultCodexExecutionPolicy()
	}
	if approvalPolicy != "" {
		policy.approvalPolicy = approvalPolicy
	}
	if approvalsReviewer != "" {
		policy.approvalsReviewer = approvalsReviewer
	}
	if len(sandbox) != 0 && string(sandbox) != "null" {
		var value string
		if err := json.Unmarshal(sandbox, &value); err != nil {
			var object struct {
				Type string `json:"type"`
			}
			if objectErr := json.Unmarshal(sandbox, &object); objectErr != nil || object.Type == "" {
				return codexExecutionPolicy{}, errors.New("sandbox must be a supported string or object with a type")
			}
			value = object.Type
		}
		switch value {
		case "read-only", "readOnly":
			policy.sandbox = "read-only"
		case "workspace-write", "workspaceWrite":
			policy.sandbox = "workspace-write"
		case "danger-full-access", "dangerFullAccess":
			policy.sandbox = "danger-full-access"
		default:
			return codexExecutionPolicy{}, fmt.Errorf("sandbox policy %q is not supported", value)
		}
	}
	if policy.approvalsReviewer != "user" {
		return codexExecutionPolicy{}, fmt.Errorf("approvals reviewer %q is not supported", policy.approvalsReviewer)
	}
	switch {
	case policy.approvalPolicy == "on-request" && policy.sandbox == "workspace-write":
		policy.permissionMode = proto.AgentPermissionInteractive
	case policy.approvalPolicy == "never" && policy.sandbox == "read-only":
		policy.permissionMode = proto.AgentPermissionDeny
	case policy.approvalPolicy == "never" && policy.sandbox == "danger-full-access":
		policy.permissionMode = proto.AgentPermissionBypass
	default:
		return codexExecutionPolicy{}, fmt.Errorf("approval policy %q with sandbox %q cannot be enforced by Crux", policy.approvalPolicy, policy.sandbox)
	}
	return policy, nil
}

func codexThread(binding *codexThreadBinding, sess *proto.Session, selection config.SelectedModel) map[string]any {
	status := map[string]any{"type": "idle"}
	if sess.IsBusy {
		status = map[string]any{"type": "active", "activeFlags": []any{}}
	}
	return map[string]any{
		"id": binding.codexID, "sessionId": binding.codexID, "cliVersion": version.Version,
		"createdAt": sess.CreatedAt, "updatedAt": sess.UpdatedAt,
		"cwd": binding.workspace.value.Path, "ephemeral": binding.ephemeral, "modelProvider": selection.Provider,
		"preview": sess.Title, "projectId": nil, "source": "appServer",
		"status": status, "turns": []any{},
	}
}

func codexThreadStartResult(thread map[string]any, selection config.SelectedModel, policy codexExecutionPolicy) map[string]any {
	return map[string]any{
		"thread": thread, "model": selection.Model, "modelProvider": selection.Provider,
		"approvalPolicy": policy.approvalPolicy, "approvalsReviewer": policy.approvalsReviewer,
		"cwd": thread["cwd"], "sandbox": map[string]any{"type": codexSandboxResponseType(policy.sandbox)},
	}
}

func codexModelID(providerID, modelID string) string {
	value := strings.ToUpper(providerID + "-" + modelID)
	var out strings.Builder
	dash := false
	for _, r := range value {
		valid := (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if valid {
			out.WriteRune(r)
			dash = false
		} else if out.Len() > 0 && !dash {
			out.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(out.String(), "-")
}

func (b *codexNativeBridge) listModels(ctx context.Context, workspace *codexNativeWorkspace) ([]any, error) {
	surfaces, err := workspace.client.ProviderSurfaces(ctx, workspace.value.ID)
	if err != nil {
		return nil, err
	}
	cfg, err := workspace.client.GetConfig(ctx, workspace.value.ID)
	if err != nil {
		return nil, err
	}
	selected := cfg.Models[config.SelectedModelTypeLarge]
	b.selectedModel[workspace] = selected
	catalog := make(map[string]config.SelectedModel)
	owners := make(map[string]providerregistry.RegistrationOwner)
	models := make([]map[string]any, 0)
	for _, surface := range surfaces {
		if !surface.Available || surface.Owner == nil {
			continue
		}
		if surface.Owner.ProviderID != surface.ID {
			return nil, fmt.Errorf("provider owner %s does not match surface provider %s", surface.Owner.ProviderID, surface.ID)
		}
		owners[surface.ID] = *surface.Owner
		for _, model := range surface.Models {
			externalID := codexModelID(surface.ID, model.ID)
			selection := config.SelectedModel{Provider: surface.ID, Model: model.ID}
			if previous, exists := catalog[externalID]; exists && (previous.Provider != selection.Provider || previous.Model != selection.Model) {
				digest := sha256.Sum256([]byte(surface.ID + "\x00" + model.ID))
				externalID += fmt.Sprintf("-%X", digest[:4])
			}
			catalog[externalID] = selection
			displayName := model.Name
			if displayName == "" {
				displayName = model.ID
			}
			effort := model.DefaultReasoningEffort
			if effort == "" {
				effort = "medium"
			}
			levels := model.ReasoningLevels
			if len(levels) == 0 {
				levels = []string{effort}
			}
			reasoning := make([]any, 0, len(levels))
			for _, level := range levels {
				reasoning = append(reasoning, map[string]any{"reasoningEffort": level, "description": level})
			}
			modalities := []string{"text"}
			if model.SupportsImages {
				modalities = append(modalities, "image")
			}
			models = append(models, map[string]any{
				"id": externalID, "model": externalID, "displayName": displayName,
				"description": surface.Name + " — " + displayName,
				"hidden":      false, "isDefault": selected.Provider == surface.ID && selected.Model == model.ID,
				"defaultReasoningEffort": effort, "supportedReasoningEfforts": reasoning,
				"inputModalities": modalities,
			})
		}
	}
	b.models[workspace] = catalog
	b.modelOwners[workspace] = owners
	sort.Slice(models, func(i, j int) bool { return models[i]["id"].(string) < models[j]["id"].(string) })
	result := make([]any, len(models))
	for i := range models {
		result[i] = models[i]
	}
	return result, nil
}

func (b *codexNativeBridge) selectModel(ctx context.Context, workspace *codexNativeWorkspace, modelID, providerID string) (config.SelectedModel, error) {
	if b.models[workspace] == nil {
		if _, err := b.listModels(ctx, workspace); err != nil {
			return config.SelectedModel{}, err
		}
	}
	if modelID == "" {
		return b.selectedModel[workspace], nil
	}
	catalog := b.models[workspace]
	var selection config.SelectedModel
	if providerID != "" {
		for _, candidate := range catalog {
			if candidate.Provider == providerID && candidate.Model == modelID {
				selection = candidate
				break
			}
		}
	} else if candidate, ok := catalog[modelID]; ok {
		selection = candidate
	} else {
		if slash := strings.IndexByte(modelID, '/'); slash > 0 && slash < len(modelID)-1 {
			providerID, modelID = modelID[:slash], modelID[slash+1:]
		}
		matches := make([]config.SelectedModel, 0, 1)
		for _, candidate := range catalog {
			if candidate.Model == modelID && (providerID == "" || candidate.Provider == providerID) {
				matches = append(matches, candidate)
			}
		}
		switch len(matches) {
		case 1:
			selection = matches[0]
		case 0:
		default:
			current := b.selectedModel[workspace]
			if current.Model == modelID {
				selection = current
			} else if providerID == "" {
				return config.SelectedModel{}, fmt.Errorf("model %q is available from multiple providers; provide modelProvider", modelID)
			}
		}
	}
	if selection.Model == "" || selection.Provider == "" {
		if providerID != "" {
			return config.SelectedModel{}, fmt.Errorf("model %q is not available from provider %q", modelID, providerID)
		}
		return config.SelectedModel{}, fmt.Errorf("model %q is not available", modelID)
	}
	current := b.selectedModel[workspace]
	if current.Provider == selection.Provider && current.Model == selection.Model {
		return selection, nil
	}
	owner, ok := b.modelOwners[workspace][selection.Provider]
	if !ok {
		return config.SelectedModel{}, fmt.Errorf("provider owner is unavailable for %s", selection.Provider)
	}
	state, err := workspace.client.UpdatePreferredModel(ctx, workspace.value.ID, config.ScopeWorkspace, config.SelectedModelTypeLarge, selection, owner)
	if err != nil {
		return config.SelectedModel{}, err
	}
	if err := workspace.client.UpdateAgent(ctx, workspace.value.ID, state); err != nil {
		return config.SelectedModel{}, err
	}
	b.selectedModel[workspace] = selection
	return selection, nil
}

func (b *codexNativeBridge) ensureAgent(ctx context.Context, workspace *codexNativeWorkspace) error {
	if err := workspace.client.InitiateAgentProcessing(ctx, workspace.value.ID, false); err != nil {
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		info, err := workspace.client.GetAgentInfo(ctx, workspace.value.ID)
		if err == nil && info.IsReady {
			current, workspaceErr := workspace.client.GetWorkspace(ctx, workspace.value.ID)
			if workspaceErr != nil {
				return workspaceErr
			}
			if current.Config == nil {
				return errors.New("workspace configuration is missing")
			}
			return workspace.client.UpdateAgent(ctx, workspace.value.ID, current.Config.AgentModelState())
		}
		if time.Now().After(deadline) {
			if err != nil {
				return err
			}
			return errors.New("timeout waiting for agent readiness")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

type codexTurnResult struct {
	threadID, turnID, itemID, text string
	cancelled                      bool
	err                            error
}

func (b *codexNativeBridge) runTurn(ctx context.Context, binding *codexThreadBinding, turnID, itemID, prompt string, permissionMode proto.AgentPermissionMode, output *protocolOutput) codexTurnResult {
	workspace := binding.workspace
	result := codexTurnResult{threadID: binding.codexID, turnID: turnID, itemID: itemID}
	if err := b.ensureAgent(ctx, workspace); err != nil {
		result.err = err
		return result
	}
	runID := uuid.NewString()
	if err := workspace.client.SendMessageWithPermissionMode(ctx, workspace.value.ID, binding.nativeID, runID, prompt, permissionMode); err != nil {
		result.err = err
		return result
	}
	read := make(map[string]int)
	for {
		select {
		case <-ctx.Done():
			_ = workspace.client.CancelAgentSession(context.Background(), workspace.value.ID, binding.nativeID)
			result.cancelled = true
			return result
		case event, ok := <-workspace.events:
			if !ok {
				result.err = errors.New("Crux event stream closed")
				return result
			}
			switch value := event.(type) {
			case pubsub.Event[proto.Message]:
				msg := value.Payload
				if msg.SessionID != binding.nativeID || msg.Role != proto.Assistant {
					continue
				}
				text := msg.Content().String()
				offset := read[msg.ID]
				if offset > len(text) {
					offset = 0
				}
				delta := text[offset:]
				read[msg.ID] = len(text)
				if delta != "" {
					result.text += delta
					if err := output.write(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"threadId": binding.codexID, "turnId": turnID, "itemId": itemID, "delta": delta}}); err != nil {
						result.err = err
						return result
					}
				}
			case pubsub.Event[proto.RunComplete]:
				complete := value.Payload
				if complete.RunID != runID {
					continue
				}
				if len(result.text) < len(complete.Text) {
					delta := complete.Text[len(result.text):]
					result.text = complete.Text
					if delta != "" {
						_ = output.write(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"threadId": binding.codexID, "turnId": turnID, "itemId": itemID, "delta": delta}})
					}
				}
				result.cancelled = complete.Cancelled
				if complete.Error != "" && !complete.Cancelled {
					result.err = errors.New(complete.Error)
				}
				return result
			case pubsub.Event[proto.AgentEvent]:
				if value.Payload.Error != nil && value.Payload.RunID == runID {
					result.err = value.Payload.Error
					return result
				}
			}
		}
	}
}

func runCodexAppServerNative(ctx context.Context, invocation compatibility.Invocation, request compatibility.Request) error {
	bridge, err := newCodexNativeBridge(ctx, invocation, request)
	if err != nil {
		return err
	}
	defer bridge.Close()
	output := &protocolOutput{writer: invocation.Stdout}
	lines := make(chan []byte)
	scanErrors := make(chan error, 1)
	go func() {
		defer close(lines)
		scanner := scanProtocolInput(request.Prompt.Stdin)
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			select {
			case lines <- line:
			case <-ctx.Done():
				return
			}
		}
		scanErrors <- scanner.Err()
	}()
	initialized := false
	turnDone := make(chan codexTurnResult, 1)
	type activeTurn struct {
		threadID, turnID string
		binding          *codexThreadBinding
		cancel           context.CancelFunc
	}
	var active *activeTurn
	for lines != nil || active != nil {
		select {
		case <-ctx.Done():
			if active != nil {
				active.cancel()
			}
			return ctx.Err()
		case result := <-turnDone:
			status := "completed"
			var turnError any
			if result.cancelled {
				status = "interrupted"
			} else if result.err != nil {
				status = "failed"
				turnError = map[string]any{"message": result.err.Error()}
			}
			itemStatus := "completed"
			if result.cancelled {
				itemStatus = "interrupted"
			} else if result.err != nil {
				itemStatus = "failed"
			}
			item := map[string]any{"id": result.itemID, "type": "agentMessage", "status": itemStatus, "text": result.text}
			if err := output.write(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": result.threadID, "turnId": result.turnID, "item": item}}); err != nil {
				return err
			}
			turn := map[string]any{"id": result.turnID, "status": status, "items": []any{item}, "error": turnError}
			if err := output.write(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": result.threadID, "turn": turn}}); err != nil {
				return err
			}
			active = nil
		case line, ok := <-lines:
			if !ok {
				lines = nil
				if active != nil {
					active.cancel()
				}
				continue
			}
			var message struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if json.Unmarshal(line, &message) != nil {
				if err := writeCodexError(output, nil, -32700, "Parse error"); err != nil {
					return err
				}
				continue
			}
			if message.Method != "initialize" && !initialized {
				if len(message.ID) != 0 {
					_ = writeCodexError(output, message.ID, -32002, "Not initialized")
				}
				continue
			}
			switch message.Method {
			case "initialize":
				if initialized {
					_ = writeCodexError(output, message.ID, -32600, "Already initialized")
					continue
				}
				initialized = true
				platformFamily := "unix"
				if runtime.GOOS == "windows" {
					platformFamily = "windows"
				}
				if err := writeCodexResult(output, message.ID, map[string]any{"userAgent": "crux-codex-compatibility", "codexHome": request.WorkingDir, "platformFamily": platformFamily, "platformOs": runtime.GOOS}); err != nil {
					return err
				}
			case "initialized":
			case "account/read":
				if err := writeCodexResult(output, message.ID, map[string]any{"account": nil, "requiresOpenaiAuth": false}); err != nil {
					return err
				}
			case "config/read":
				var params struct {
					CWD string `json:"cwd"`
				}
				_ = json.Unmarshal(message.Params, &params)
				workspace, wsErr := bridge.workspace(params.CWD)
				if wsErr != nil {
					_ = writeCodexError(output, message.ID, -32000, wsErr.Error())
					continue
				}
				if _, listErr := bridge.listModels(ctx, workspace); listErr != nil {
					_ = writeCodexError(output, message.ID, -32000, listErr.Error())
					continue
				}
				selection := bridge.selectedModel[workspace]
				result := map[string]any{
					"config":  map[string]any{"model": selection.Model, "model_provider": selection.Provider},
					"origins": map[string]any{}, "layers": []any{},
				}
				if err := writeCodexResult(output, message.ID, result); err != nil {
					return err
				}
			case "plugin/list":
				if err := writeCodexResult(output, message.ID, map[string]any{"marketplaces": []any{}, "marketplaceLoadErrors": []any{}, "featuredPluginIds": []any{}}); err != nil {
					return err
				}
			case "model/list":
				workspace, wsErr := bridge.workspace(request.WorkingDir)
				if wsErr != nil {
					_ = writeCodexError(output, message.ID, -32000, wsErr.Error())
					continue
				}
				models, listErr := bridge.listModels(ctx, workspace)
				if listErr != nil {
					_ = writeCodexError(output, message.ID, -32000, listErr.Error())
					continue
				}
				if err := writeCodexResult(output, message.ID, map[string]any{"data": models, "nextCursor": nil}); err != nil {
					return err
				}
			case "thread/list":
				data := make([]any, 0)
				listFailed := false
				for _, workspace := range bridge.workspaces {
					if _, modelErr := bridge.listModels(ctx, workspace); modelErr != nil {
						_ = writeCodexError(output, message.ID, -32000, modelErr.Error())
						listFailed = true
						break
					}
					sessions, listErr := workspace.client.ListSessions(ctx, workspace.value.ID)
					if listErr != nil {
						_ = writeCodexError(output, message.ID, -32000, listErr.Error())
						listFailed = true
						break
					}
					for i := range sessions {
						sess := sessions[i]
						binding := bridge.bindThread(workspace, &sess)
						data = append(data, codexThread(binding, &sess, bridge.selectedModel[workspace]))
					}
				}
				if listFailed {
					continue
				}
				if err := writeCodexResult(output, message.ID, map[string]any{"data": data, "nextCursor": nil}); err != nil {
					return err
				}
			case "thread/read", "thread/resume":
				var params struct {
					ThreadID      string `json:"threadId"`
					CWD           string `json:"cwd"`
					Model         string `json:"model"`
					ModelProvider string `json:"modelProvider"`
				}
				_ = json.Unmarshal(message.Params, &params)
				binding, sess, findErr := bridge.findThread(ctx, params.ThreadID, params.CWD)
				if findErr != nil {
					_ = writeCodexError(output, message.ID, -32002, "Thread not found")
					continue
				}
				selection, selectErr := bridge.selectModel(ctx, binding.workspace, params.Model, params.ModelProvider)
				if selectErr != nil {
					_ = writeCodexError(output, message.ID, -32602, selectErr.Error())
					continue
				}
				thread := codexThread(binding, sess, selection)
				result := map[string]any{"thread": thread}
				if message.Method == "thread/resume" {
					result = codexThreadStartResult(thread, selection, binding.policy)
				}
				if err := writeCodexResult(output, message.ID, result); err != nil {
					return err
				}
			case "thread/start":
				var params struct {
					CWD               string          `json:"cwd"`
					Model             string          `json:"model"`
					ModelProvider     string          `json:"modelProvider"`
					ApprovalPolicy    string          `json:"approvalPolicy"`
					ApprovalsReviewer string          `json:"approvalsReviewer"`
					Sandbox           json.RawMessage `json:"sandbox"`
					Ephemeral         *bool           `json:"ephemeral"`
				}
				if json.Unmarshal(message.Params, &params) != nil {
					_ = writeCodexError(output, message.ID, -32602, "Invalid thread/start params")
					continue
				}
				policy, policyErr := parseCodexExecutionPolicy(params.ApprovalPolicy, params.ApprovalsReviewer, params.Sandbox, defaultCodexExecutionPolicy())
				if policyErr != nil {
					_ = writeCodexError(output, message.ID, -32602, policyErr.Error())
					continue
				}
				workspace, wsErr := bridge.workspace(params.CWD)
				if wsErr != nil {
					_ = writeCodexError(output, message.ID, -32602, wsErr.Error())
					continue
				}
				model := params.Model
				if model == "" {
					model = request.Model
				}
				selection, selectErr := bridge.selectModel(ctx, workspace, model, params.ModelProvider)
				if selectErr != nil {
					_ = writeCodexError(output, message.ID, -32602, selectErr.Error())
					continue
				}
				sess, createErr := workspace.client.CreateSession(ctx, workspace.value.ID, "Codex")
				if createErr != nil {
					_ = writeCodexError(output, message.ID, -32000, createErr.Error())
					continue
				}
				binding := bridge.bindThread(workspace, sess)
				binding.policy = policy
				binding.ephemeral = !request.Session.Persistent
				if params.Ephemeral != nil {
					binding.ephemeral = binding.ephemeral || *params.Ephemeral
				}
				thread := codexThread(binding, sess, selection)
				if err := writeCodexResult(output, message.ID, codexThreadStartResult(thread, selection, policy)); err != nil {
					return err
				}
				if err := output.write(map[string]any{"method": "thread/started", "params": map[string]any{"thread": thread}}); err != nil {
					return err
				}
			case "thread/fork":
				var params struct {
					ThreadID string `json:"threadId"`
					CWD      string `json:"cwd"`
				}
				_ = json.Unmarshal(message.Params, &params)
				binding, _, findErr := bridge.findThread(ctx, params.ThreadID, params.CWD)
				if findErr != nil {
					_ = writeCodexError(output, message.ID, -32002, "Thread not found")
					continue
				}
				forked, forkErr := binding.workspace.client.ForkSession(ctx, binding.workspace.value.ID, binding.nativeID)
				if forkErr != nil {
					_ = writeCodexError(output, message.ID, -32000, forkErr.Error())
					continue
				}
				selection, selectErr := bridge.selectModel(ctx, binding.workspace, "", "")
				if selectErr != nil {
					_ = writeCodexError(output, message.ID, -32000, selectErr.Error())
					continue
				}
				forkBinding := bridge.bindThread(binding.workspace, forked)
				forkBinding.policy = binding.policy
				forkBinding.ephemeral = binding.ephemeral
				thread := codexThread(forkBinding, forked, selection)
				if err := writeCodexResult(output, message.ID, codexThreadStartResult(thread, selection, forkBinding.policy)); err != nil {
					return err
				}
				if err := output.write(map[string]any{"method": "thread/started", "params": map[string]any{"thread": thread}}); err != nil {
					return err
				}
			case "turn/start":
				if active != nil {
					_ = writeCodexError(output, message.ID, -32600, "A turn is already running")
					continue
				}
				var params struct {
					ThreadID          string          `json:"threadId"`
					Model             string          `json:"model"`
					ApprovalPolicy    string          `json:"approvalPolicy"`
					ApprovalsReviewer string          `json:"approvalsReviewer"`
					Sandbox           json.RawMessage `json:"sandbox"`
					Input             []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"input"`
				}
				if json.Unmarshal(message.Params, &params) != nil {
					_ = writeCodexError(output, message.ID, -32602, "Invalid turn/start params")
					continue
				}
				binding, _, findErr := bridge.findThread(ctx, params.ThreadID, "")
				if findErr != nil {
					_ = writeCodexError(output, message.ID, -32002, "Thread not found")
					continue
				}
				turnPolicy, policyErr := parseCodexExecutionPolicy(params.ApprovalPolicy, params.ApprovalsReviewer, params.Sandbox, binding.policy)
				if policyErr != nil {
					_ = writeCodexError(output, message.ID, -32602, policyErr.Error())
					continue
				}
				if params.Model != "" {
					if _, selectErr := bridge.selectModel(ctx, binding.workspace, params.Model, ""); selectErr != nil {
						_ = writeCodexError(output, message.ID, -32602, selectErr.Error())
						continue
					}
				}
				parts := make([]string, 0, len(params.Input))
				valid := true
				for _, input := range params.Input {
					if input.Type != "text" {
						valid = false
						break
					}
					parts = append(parts, input.Text)
				}
				if !valid {
					_ = writeCodexError(output, message.ID, -32602, "Only text turn input is supported")
					continue
				}
				turnID, itemID := uuid.Must(uuid.NewV7()).String(), uuid.Must(uuid.NewV7()).String()
				turn := map[string]any{"id": turnID, "status": "inProgress", "items": []any{}, "error": nil}
				if err := writeCodexResult(output, message.ID, map[string]any{"turn": turn}); err != nil {
					return err
				}
				if err := output.write(map[string]any{"method": "turn/started", "params": map[string]any{"threadId": binding.codexID, "turn": turn}}); err != nil {
					return err
				}
				item := map[string]any{"id": itemID, "type": "agentMessage", "status": "inProgress", "text": ""}
				if err := output.write(map[string]any{"method": "item/started", "params": map[string]any{"threadId": binding.codexID, "turnId": turnID, "item": item}}); err != nil {
					return err
				}
				turnCtx, cancel := context.WithCancel(ctx)
				active = &activeTurn{threadID: binding.codexID, turnID: turnID, binding: binding, cancel: cancel}
				go func() {
					turnDone <- bridge.runTurn(turnCtx, binding, turnID, itemID, strings.Join(parts, "\n"), turnPolicy.permissionMode, output)
				}()
			case "turn/interrupt":
				var params struct {
					ThreadID string `json:"threadId"`
					TurnID   string `json:"turnId"`
				}
				_ = json.Unmarshal(message.Params, &params)
				if active != nil && (active.threadID == params.ThreadID || active.binding.nativeID == params.ThreadID) && active.turnID == params.TurnID {
					if cancelErr := active.binding.workspace.client.CancelAgentSession(ctx, active.binding.workspace.value.ID, active.binding.nativeID); cancelErr != nil {
						_ = writeCodexError(output, message.ID, -32000, cancelErr.Error())
						continue
					}
				}
				if len(message.ID) != 0 {
					if err := writeCodexResult(output, message.ID, map[string]any{}); err != nil {
						return err
					}
				}
			default:
				if len(message.ID) != 0 {
					if err := writeCodexError(output, message.ID, -32601, "Method not found"); err != nil {
						return err
					}
				}
			}
		}
	}
	return <-scanErrors
}
