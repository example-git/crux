package localaddon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/example-git/crux/internal/agent"
	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/compatibility"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/example-git/crux/internal/session"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/example-git/crux/internal/version"
	"github.com/google/uuid"
	"golang.org/x/net/http/httpguts"
)

const (
	codexServerReadyTimeout = 10 * time.Second
	codexThreadUnloadDelay  = 30 * time.Minute
)

var (
	codexClientFactory              = client.DefaultClient
	codexThreadUnloadDelayForBridge = codexThreadUnloadDelay
)

type codexConnectionState struct {
	initialized                    bool
	clientName                     string
	experimentalAPI                bool
	requestAttestation             bool
	mcpServerOpenAIFormElicitation bool
	optOutNotificationMethods      map[string]struct{}
	extensions                     map[string]any
}

func (s *codexConnectionState) writeNotification(output *protocolOutput, method string, params any) error {
	if _, excluded := s.optOutNotificationMethods[method]; excluded {
		return nil
	}
	return output.write(map[string]any{"method": method, "params": params})
}

func (s *codexConnectionState) writeThreadNotification(output *protocolOutput, binding *codexThreadBinding, method string, params any) error {
	if !binding.subscribed.Load() {
		return nil
	}
	return s.writeNotification(output, method, params)
}

func decodeOptionalNullableString(raw json.RawMessage, field string) (bool, string, error) {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return false, "", err
	}
	value, ok := values[field]
	if !ok {
		return false, "", nil
	}
	if string(value) == "null" {
		return true, "", nil
	}
	var decoded string
	if err := json.Unmarshal(value, &decoded); err != nil {
		return false, "", fmt.Errorf("%s must be a string or null", field)
	}
	if decoded == "" {
		return false, "", fmt.Errorf("%s must not be empty", field)
	}
	return true, decoded, nil
}

func decodeCodexCapability(raw json.RawMessage, field string) (bool, error) {
	if len(raw) == 0 {
		return false, nil
	}
	if string(raw) == "null" {
		return false, fmt.Errorf("%s must be a boolean", field)
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, fmt.Errorf("%s must be a boolean", field)
	}
	return value, nil
}

type codexClientInfoNameError string

func (name codexClientInfoNameError) Error() string {
	return fmt.Sprintf("Invalid clientInfo.name: '%s'. Must be a valid HTTP header value.", string(name))
}

func parseCodexInitializeParams(raw json.RawMessage) (*codexConnectionState, string, string, error) {
	var params struct {
		ClientInfo *struct {
			Name    *string `json:"name"`
			Version *string `json:"version"`
			Title   *string `json:"title"`
		} `json:"clientInfo"`
		Capabilities json.RawMessage `json:"capabilities"`
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, "", "", errors.New("missing field `clientInfo`")
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, "", "", err
	}
	if params.ClientInfo == nil {
		return nil, "", "", errors.New("missing field `clientInfo`")
	}
	if params.ClientInfo.Name == nil {
		return nil, "", "", errors.New("missing field `clientInfo.name`")
	}
	if params.ClientInfo.Version == nil {
		return nil, "", "", errors.New("missing field `clientInfo.version`")
	}
	if !httpguts.ValidHeaderFieldValue(*params.ClientInfo.Name) {
		return nil, "", "", codexClientInfoNameError(*params.ClientInfo.Name)
	}
	state := &codexConnectionState{optOutNotificationMethods: make(map[string]struct{})}
	if len(params.Capabilities) == 0 || string(params.Capabilities) == "null" {
		return state, *params.ClientInfo.Name, *params.ClientInfo.Version, nil
	}
	var capabilities struct {
		ExperimentalAPI                json.RawMessage `json:"experimentalApi"`
		RequestAttestation             json.RawMessage `json:"requestAttestation"`
		MCPServerOpenAIFormElicitation json.RawMessage `json:"mcpServerOpenaiFormElicitation"`
		OptOutNotificationMethods      json.RawMessage `json:"optOutNotificationMethods"`
		Extensions                     map[string]any  `json:"extensions"`
	}
	if err := json.Unmarshal(params.Capabilities, &capabilities); err != nil {
		return nil, "", "", err
	}
	var err error
	state.experimentalAPI, err = decodeCodexCapability(capabilities.ExperimentalAPI, "capabilities.experimentalApi")
	if err != nil {
		return nil, "", "", err
	}
	state.requestAttestation, err = decodeCodexCapability(capabilities.RequestAttestation, "capabilities.requestAttestation")
	if err != nil {
		return nil, "", "", err
	}
	state.mcpServerOpenAIFormElicitation, err = decodeCodexCapability(capabilities.MCPServerOpenAIFormElicitation, "capabilities.mcpServerOpenaiFormElicitation")
	if err != nil {
		return nil, "", "", err
	}
	if len(capabilities.OptOutNotificationMethods) != 0 && string(capabilities.OptOutNotificationMethods) != "null" {
		var methods []string
		if err := json.Unmarshal(capabilities.OptOutNotificationMethods, &methods); err != nil {
			return nil, "", "", errors.New("capabilities.optOutNotificationMethods must be an array of strings")
		}
		for _, method := range methods {
			state.optOutNotificationMethods[method] = struct{}{}
		}
	}
	state.extensions = capabilities.Extensions
	return state, *params.ClientInfo.Name, *params.ClientInfo.Version, nil
}

func codexHome(environment []string, workingDir string) (string, error) {
	lookup := func(key string) (string, bool) {
		prefix := key + "="
		for i := len(environment) - 1; i >= 0; i-- {
			if strings.HasPrefix(environment[i], prefix) {
				return strings.TrimPrefix(environment[i], prefix), true
			}
		}
		return "", false
	}
	home, explicit := lookup("CODEX_HOME")
	if explicit && home == "" {
		return "", errors.New("CODEX_HOME is empty")
	}
	if !explicit {
		var ok bool
		home, ok = lookup("HOME")
		if !ok || home == "" {
			home, ok = lookup("USERPROFILE")
		}
		if !ok || home == "" {
			var err error
			home, err = os.UserHomeDir()
			if err != nil {
				return "", err
			}
		}
		home = filepath.Join(home, ".codex")
	}
	if !filepath.IsAbs(home) {
		home = filepath.Join(workingDir, home)
	}
	return filepath.Abs(filepath.Clean(home))
}

type codexNativeWorkspace struct {
	client          *client.Client
	value           *proto.Workspace
	keepaliveCancel context.CancelFunc
	keepaliveDone   chan struct{}
	modelIDsMu      sync.RWMutex
	modelIDs        map[codexNativeModel]string
}

type codexThreadBinding struct {
	codexID          string
	nativeID         string
	forkedFromID     string
	parentThreadID   string
	projectID        string
	workspace        *codexNativeWorkspace
	policy           codexExecutionPolicy
	model            config.SelectedModel
	ephemeral        bool
	loaded           bool
	active           bool
	systemError      bool
	subscribed       atomic.Bool
	unloadTimer      *time.Timer
	unloadGeneration uint64
}

type codexExecutionPolicy struct {
	approvalPolicy    string
	approvalsReviewer string
	sandbox           string
	permissionMode    proto.AgentPermissionMode
}

type codexThreadUnload struct {
	threadID   string
	generation uint64
}

type codexNativeBridge struct {
	ctx                context.Context
	invocation         compatibility.Invocation
	request            compatibility.Request
	server             *exec.Cmd
	serverDone         chan error
	ready              bool
	defaultCWD         string
	activeCWD          string
	workspaces         map[string]*codexNativeWorkspace
	threads            map[string]*codexThreadBinding
	models             map[*codexNativeWorkspace]map[string]config.SelectedModel
	modelOwners        map[*codexNativeWorkspace]map[string]providerregistry.RegistrationOwner
	selectedModel      map[*codexNativeWorkspace]config.SelectedModel
	terminalProcessIDs map[string]int32
	terminalTaskIDs    map[int32]string
	nextTerminalID     int32
	unloadDelay        time.Duration
	unloadReady        chan codexThreadUnload
}

func codexNativeServerEnvironment(environment []string) []string {
	result := make([]string, 0, len(environment)+1)
	for _, value := range environment {
		key, _, _ := strings.Cut(value, "=")
		if strings.EqualFold(key, compatibility.BypassEnvironment) {
			continue
		}
		result = append(result, value)
	}
	return append(result, compatibility.BypassEnvironment+"=1")
}

func newCodexNativeBridgeState(ctx context.Context, invocation compatibility.Invocation, request compatibility.Request) *codexNativeBridge {
	return &codexNativeBridge{ctx: ctx, invocation: invocation, request: request, workspaces: make(map[string]*codexNativeWorkspace), threads: make(map[string]*codexThreadBinding), models: make(map[*codexNativeWorkspace]map[string]config.SelectedModel), modelOwners: make(map[*codexNativeWorkspace]map[string]providerregistry.RegistrationOwner), selectedModel: make(map[*codexNativeWorkspace]config.SelectedModel), terminalProcessIDs: make(map[string]int32), terminalTaskIDs: make(map[int32]string), unloadDelay: codexThreadUnloadDelayForBridge, unloadReady: make(chan codexThreadUnload, 16)}
}

func newCodexNativeBridge(ctx context.Context, invocation compatibility.Invocation, request compatibility.Request) (*codexNativeBridge, error) {
	b, err := connectCodexNativeBridge(ctx, invocation, request)
	if err != nil {
		return nil, err
	}
	if _, err := b.workspace(request.WorkingDir); err != nil {
		return nil, errors.Join(err, b.Close())
	}
	return b, nil
}

func connectCodexNativeBridge(ctx context.Context, invocation compatibility.Invocation, request compatibility.Request) (*codexNativeBridge, error) {
	b := newCodexNativeBridgeState(ctx, invocation, request)
	probe, err := codexClientFactory(request.WorkingDir)
	if err != nil {
		return nil, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	err = probe.Health(probeCtx)
	cancel()
	if err == nil {
		b.ready = true
		return b, nil
	}
	cmd := exec.CommandContext(context.Background(), invocation.Executable, "server")
	cmd.Dir = request.WorkingDir
	cmd.Env = codexNativeServerEnvironment(invocation.Env)
	cmd.Stdout = invocation.Stderr
	cmd.Stderr = invocation.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start Crux server: %w", err)
	}
	b.server = cmd
	b.serverDone = make(chan error, 1)
	go func() { b.serverDone <- cmd.Wait() }()
	deadline := time.Now().Add(codexServerReadyTimeout)
	for {
		attemptCtx, attemptCancel := context.WithTimeout(ctx, 200*time.Millisecond)
		err = probe.Health(attemptCtx)
		attemptCancel()
		if err == nil {
			b.ready = true
			return b, nil
		}
		if time.Now().After(deadline) {
			return nil, errors.Join(fmt.Errorf("wait for Crux server: %w", err), b.Close())
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(ctx.Err(), b.Close())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (b *codexNativeBridge) deleteEphemeralThread(ctx context.Context, binding *codexThreadBinding) error {
	if !binding.ephemeral {
		return nil
	}
	if err := binding.workspace.client.DeleteSession(ctx, binding.workspace.value.ID, binding.nativeID); err != nil {
		return err
	}
	delete(b.threads, binding.codexID)
	return nil
}

func (b *codexNativeBridge) Close() error {
	var cleanupErrors []error
	for _, binding := range b.threads {
		if binding.unloadTimer != nil {
			binding.unloadTimer.Stop()
		}
		if binding.ephemeral {
			cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), codexServerReadyTimeout)
			if err := b.deleteEphemeralThread(cleanupCtx, binding); err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("delete ephemeral thread %s: %w", binding.codexID, err))
			}
			cancelCleanup()
		}
	}
	for _, workspace := range b.workspaces {
		if workspace.keepaliveCancel != nil {
			workspace.keepaliveCancel()
		}
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), codexServerReadyTimeout)
		retireErr := workspace.client.RetireClient(cleanupCtx)
		cancelCleanup()
		if workspace.keepaliveDone != nil {
			waitTimer := time.NewTimer(codexServerReadyTimeout)
			select {
			case <-workspace.keepaliveDone:
				if !waitTimer.Stop() {
					select {
					case <-waitTimer.C:
					default:
					}
				}
			case <-waitTimer.C:
				cleanupErrors = append(cleanupErrors, fmt.Errorf("timed out waiting for compatibility keepalive for workspace %s", workspace.value.ID))
			}
		}
		if retireErr != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("retire compatibility client for workspace %s: %w", workspace.value.ID, retireErr))
			cleanupCtx, cancelCleanup = context.WithTimeout(context.Background(), codexServerReadyTimeout)
			if deleteErr := workspace.client.DeleteWorkspace(cleanupCtx, workspace.value.ID); deleteErr != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("delete compatibility workspace %s: %w", workspace.value.ID, deleteErr))
			}
			cancelCleanup()
		}
	}
	if b.server != nil && b.server.Process != nil && b.serverDone != nil {
		select {
		case waitErr := <-b.serverDone:
			if waitErr != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("wait for compatibility server: %w", waitErr))
			}
		default:
			killErr := b.server.Process.Kill()
			killed := killErr == nil
			if killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("terminate compatibility server: %w", killErr))
			}
			waitTimer := time.NewTimer(codexServerReadyTimeout)
			select {
			case waitErr := <-b.serverDone:
				if waitErr != nil && !killed {
					cleanupErrors = append(cleanupErrors, fmt.Errorf("wait for compatibility server: %w", waitErr))
				}
				if !waitTimer.Stop() {
					select {
					case <-waitTimer.C:
					default:
					}
				}
			case <-waitTimer.C:
				cleanupErrors = append(cleanupErrors, errors.New("timed out waiting for compatibility server to exit"))
			}
		}
	}
	return errors.Join(cleanupErrors...)
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
	keepaliveCtx, keepaliveCancel := context.WithCancel(b.ctx)
	events, err := c.SubscribeEvents(keepaliveCtx, ws.ID)
	if err != nil {
		keepaliveCancel()
		return nil, err
	}
	keepaliveDone := make(chan struct{})
	go func() {
		defer close(keepaliveDone)
		for range events {
		}
	}()
	result := &codexNativeWorkspace{client: c, value: ws, keepaliveCancel: keepaliveCancel, keepaliveDone: keepaliveDone}
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

func codexSessionModel(sess *proto.Session, fallback config.SelectedModel) config.SelectedModel {
	if sess != nil && sess.Model != "" && sess.Provider != "" {
		return config.SelectedModel{Model: sess.Model, Provider: sess.Provider}
	}
	return fallback
}

func (b *codexNativeBridge) saveSessionModel(ctx context.Context, workspace *codexNativeWorkspace, sess *proto.Session, selection config.SelectedModel) (*proto.Session, error) {
	updated := *sess
	updated.Model = selection.Model
	updated.Provider = selection.Provider
	return workspace.client.SaveSession(ctx, workspace.value.ID, updated)
}

func (b *codexNativeBridge) bindThread(workspace *codexNativeWorkspace, sess *proto.Session) *codexThreadBinding {
	codexID := codexUUIDv7(workspace.value.Path, sess)
	if binding := b.threads[codexID]; binding != nil {
		return binding
	}
	binding := &codexThreadBinding{codexID: codexID, nativeID: sess.ID, workspace: workspace, policy: defaultCodexExecutionPolicy(), model: codexSessionModel(sess, b.selectedModel[workspace])}
	b.threads[codexID] = binding
	return binding
}

func (b *codexNativeBridge) cancelThreadUnload(binding *codexThreadBinding) {
	binding.unloadGeneration++
	if binding.unloadTimer != nil {
		binding.unloadTimer.Stop()
		binding.unloadTimer = nil
	}
}

func (b *codexNativeBridge) subscribeThread(binding *codexThreadBinding) {
	b.cancelThreadUnload(binding)
	binding.subscribed.Store(true)
}

func (b *codexNativeBridge) scheduleThreadUnload(binding *codexThreadBinding) {
	b.cancelThreadUnload(binding)
	if !binding.loaded || binding.active || binding.subscribed.Load() {
		return
	}
	generation := binding.unloadGeneration
	binding.unloadTimer = time.AfterFunc(b.unloadDelay, func() {
		select {
		case b.unloadReady <- codexThreadUnload{threadID: binding.codexID, generation: generation}:
		case <-b.ctx.Done():
		}
	})
}

func (b *codexNativeBridge) unloadThread(signal codexThreadUnload) *codexThreadBinding {
	binding := b.threads[signal.threadID]
	if binding == nil || binding.unloadGeneration != signal.generation || !binding.loaded || binding.active || binding.subscribed.Load() {
		return nil
	}
	binding.unloadTimer = nil
	binding.loaded = false
	binding.active = false
	return binding
}

func paginateCodexLoadedThreads(threads map[string]*codexThreadBinding, cursor *string, limit *uint32) ([]string, *string, error) {
	data := make([]string, 0, len(threads))
	for threadID, binding := range threads {
		if binding.loaded {
			data = append(data, threadID)
		}
	}
	sort.Strings(data)
	if len(data) == 0 {
		return data, nil, nil
	}
	start := 0
	if cursor != nil {
		parsed, err := uuid.Parse(*cursor)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid cursor: %s", *cursor)
		}
		normalized := parsed.String()
		start = sort.SearchStrings(data, normalized)
		if start < len(data) && data[start] == normalized {
			start++
		}
	}
	effectiveLimit := len(data)
	if limit != nil {
		requested := max(*limit, 1)
		if uint64(requested) < uint64(effectiveLimit) {
			effectiveLimit = int(requested)
		}
	}
	end := min(start+effectiveLimit, len(data))
	page := data[start:end]
	var nextCursor *string
	if len(page) != 0 && end < len(data) {
		value := page[len(page)-1]
		nextCursor = &value
	}
	return page, nextCursor, nil
}

type codexBackgroundTerminal struct {
	ItemID        string   `json:"itemId"`
	ProcessID     string   `json:"processId"`
	Command       string   `json:"command"`
	CWD           string   `json:"cwd"`
	OSPID         *uint32  `json:"osPid"`
	CPUPercent    *float64 `json:"cpuPercent"`
	RSSKB         *uint64  `json:"rssKb"`
	taskID        string
	processNumber int32
}

func (b *codexNativeBridge) terminalProcessID(taskID string) (int32, error) {
	if b.terminalProcessIDs == nil {
		b.terminalProcessIDs = make(map[string]int32)
	}
	if b.terminalTaskIDs == nil {
		b.terminalTaskIDs = make(map[int32]string)
	}
	if processID := b.terminalProcessIDs[taskID]; processID != 0 {
		return processID, nil
	}
	if b.nextTerminalID == 1<<31-1 {
		return 0, errors.New("background terminal process id space exhausted")
	}
	b.nextTerminalID++
	processID := b.nextTerminalID
	b.terminalProcessIDs[taskID] = processID
	b.terminalTaskIDs[processID] = taskID
	return processID, nil
}

func (b *codexNativeBridge) backgroundTerminals(ctx context.Context, binding *codexThreadBinding) ([]codexBackgroundTerminal, error) {
	tasks, err := binding.workspace.client.ListTasks(ctx, binding.workspace.value.ID)
	if err != nil {
		return nil, err
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].ID < tasks[j].ID })
	terminals := make([]codexBackgroundTerminal, 0)
	for _, task := range tasks {
		if task.Type != managedtask.TypeShell || task.State.Status.Terminal() {
			continue
		}
		detail, err := binding.workspace.client.TaskOutput(ctx, binding.workspace.value.ID, task.ID, false, 0)
		if err != nil {
			return nil, err
		}
		task = detail.Task
		if task.Type != managedtask.TypeShell || task.State.Status.Terminal() || task.Ownership.ParentSessionID != binding.nativeID {
			continue
		}
		processID, err := b.terminalProcessID(task.ID)
		if err != nil {
			return nil, err
		}
		itemID := task.Ownership.OriginToolCallID
		if itemID == "" {
			itemID = task.ID
		}
		cwd := task.WorkingDirectory
		if cwd == "" {
			cwd = binding.workspace.value.Path
		}
		terminals = append(terminals, codexBackgroundTerminal{
			ItemID: itemID, ProcessID: strconv.FormatInt(int64(processID), 10), Command: task.Command, CWD: cwd,
			taskID: task.ID, processNumber: processID,
		})
	}
	sort.Slice(terminals, func(i, j int) bool { return terminals[i].processNumber < terminals[j].processNumber })
	return terminals, nil
}

func paginateCodexBackgroundTerminals(terminals []codexBackgroundTerminal, cursor *string, limit *uint32) ([]codexBackgroundTerminal, *string, error) {
	start := 0
	if cursor != nil {
		parsed, err := strconv.ParseInt(*cursor, 10, 32)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid cursor: %w", err)
		}
		start = sort.Search(len(terminals), func(index int) bool { return int64(terminals[index].processNumber) > parsed })
	}
	effectiveLimit := len(terminals)
	if limit != nil {
		effectiveLimit = int(max(*limit, 1))
	}
	end := min(start+effectiveLimit, len(terminals))
	page := terminals[start:end]
	var nextCursor *string
	if len(page) != 0 && end < len(terminals) {
		value := page[len(page)-1].ProcessID
		nextCursor = &value
	}
	return page, nextCursor, nil
}

func (b *codexNativeBridge) hydrateThreadLineage(ctx context.Context, binding *codexThreadBinding, sess *proto.Session) error {
	if sess.ForkedFromSessionID != "" && binding.forkedFromID == "" {
		source, err := binding.workspace.client.GetSession(ctx, binding.workspace.value.ID, sess.ForkedFromSessionID)
		if err != nil {
			if !errors.Is(err, client.ErrNotFound) {
				return fmt.Errorf("resolve fork source: %w", err)
			}
		} else {
			binding.forkedFromID = codexUUIDv7(binding.workspace.value.Path, source)
		}
	}
	if sess.ParentSessionID != "" && binding.parentThreadID == "" {
		parent, err := binding.workspace.client.GetSession(ctx, binding.workspace.value.ID, sess.ParentSessionID)
		if err != nil {
			return fmt.Errorf("resolve parent thread: %w", err)
		}
		binding.parentThreadID = codexUUIDv7(binding.workspace.value.Path, parent)
	}
	return nil
}

func (b *codexNativeBridge) findThread(ctx context.Context, threadID, cwd string) (*codexThreadBinding, *proto.Session, error) {
	if binding := b.threads[threadID]; binding != nil {
		if cwd != "" {
			path, err := canonicalProjectDirectory(cwd)
			if err != nil {
				return nil, nil, err
			}
			if path != binding.workspace.value.Path {
				return nil, nil, errors.New("thread cwd cannot move an existing native session to another project")
			}
		}
		sess, err := binding.workspace.client.GetSession(ctx, binding.workspace.value.ID, binding.nativeID)
		return binding, sess, err
	}
	workspaces, err := b.lookupProjectWorkspaces(ctx, cwd)
	if err != nil {
		return nil, nil, err
	}
	for _, workspace := range workspaces {
		sessions, err := workspace.client.ListSessions(ctx, workspace.value.ID)
		if err != nil {
			return nil, nil, err
		}
		for i := range sessions {
			sess := &sessions[i]
			if codexUUIDv7(workspace.value.Path, sess) != threadID && sess.ID != threadID {
				continue
			}
			binding := b.bindThread(workspace, sess)
			binding.projectID, err = b.projectForDirectory(ctx, workspace.value.Path)
			if err != nil {
				return nil, nil, err
			}
			return binding, sess, nil
		}
	}
	return nil, nil, errors.New("thread not found")
}

func (b *codexNativeBridge) findStoredThread(ctx context.Context, threadID string) (*codexThreadBinding, *proto.Session, error) {
	if binding := b.threads[threadID]; binding != nil {
		sess, err := binding.workspace.client.GetSession(ctx, binding.workspace.value.ID, binding.nativeID)
		return binding, sess, err
	}
	workspaces, err := b.lookupProjectWorkspaces(ctx, "")
	if err != nil {
		return nil, nil, err
	}
	for _, workspace := range workspaces {
		for _, archived := range []bool{false, true} {
			var sessions []proto.Session
			var err error
			if archived {
				sessions, err = workspace.client.ListArchivedSessions(ctx, workspace.value.ID)
			} else {
				sessions, err = workspace.client.ListSessions(ctx, workspace.value.ID)
			}
			if err != nil {
				continue
			}
			for index := range sessions {
				sess := &sessions[index]
				codexID := codexUUIDv7(workspace.value.Path, sess)
				if codexID == threadID || sess.ID == threadID {
					return &codexThreadBinding{codexID: codexID, nativeID: sess.ID, workspace: workspace, policy: defaultCodexExecutionPolicy(), model: codexSessionModel(sess, b.selectedModel[workspace])}, sess, nil
				}
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

func parseCodexTurnSandboxPolicy(sandboxPolicy json.RawMessage, inherited codexExecutionPolicy) (codexExecutionPolicy, error) {
	if len(sandboxPolicy) == 0 || string(sandboxPolicy) == "null" {
		return inherited, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(sandboxPolicy, &object); err != nil || object == nil {
		return codexExecutionPolicy{}, errors.New("sandboxPolicy must be an object with a type")
	}
	var policyType string
	if err := json.Unmarshal(object["type"], &policyType); err != nil || policyType == "" {
		return codexExecutionPolicy{}, errors.New("sandboxPolicy.type is required")
	}
	allowed := map[string]bool{"type": true}
	switch policyType {
	case "dangerFullAccess":
	case "readOnly":
		allowed["networkAccess"] = true
	case "workspaceWrite":
		allowed["writableRoots"] = true
		allowed["networkAccess"] = true
		allowed["excludeTmpdirEnvVar"] = true
		allowed["excludeSlashTmp"] = true
	case "externalSandbox":
		return codexExecutionPolicy{}, errors.New("sandbox policy \"externalSandbox\" is not supported")
	default:
		return codexExecutionPolicy{}, fmt.Errorf("sandbox policy %q is not supported", policyType)
	}
	for field := range object {
		if !allowed[field] {
			return codexExecutionPolicy{}, fmt.Errorf("sandboxPolicy.%s is not supported", field)
		}
	}
	for _, field := range []string{"networkAccess", "excludeTmpdirEnvVar", "excludeSlashTmp"} {
		value, exists := object[field]
		if !exists {
			continue
		}
		var enabled bool
		if err := json.Unmarshal(value, &enabled); err != nil {
			return codexExecutionPolicy{}, fmt.Errorf("sandboxPolicy.%s must be a boolean", field)
		}
		if enabled {
			return codexExecutionPolicy{}, fmt.Errorf("sandboxPolicy.%s is not supported", field)
		}
	}
	if value, exists := object["writableRoots"]; exists {
		var roots []string
		if err := json.Unmarshal(value, &roots); err != nil {
			return codexExecutionPolicy{}, errors.New("sandboxPolicy.writableRoots must be an array of paths")
		}
		if len(roots) != 0 {
			return codexExecutionPolicy{}, errors.New("sandboxPolicy.writableRoots is not supported")
		}
	}
	policy := inherited
	if policy.approvalPolicy == "" {
		policy = defaultCodexExecutionPolicy()
	}
	switch policyType {
	case "readOnly":
		policy.sandbox = "read-only"
	case "workspaceWrite":
		policy.sandbox = "workspace-write"
	case "dangerFullAccess":
		policy.sandbox = "danger-full-access"
	}
	return policy, nil
}

func codexThreadStatus(binding *codexThreadBinding, nativeBusy bool) map[string]any {
	status := map[string]any{"type": "notLoaded"}
	if binding.loaded {
		status = map[string]any{"type": "idle"}
	}
	if binding.systemError {
		status = map[string]any{"type": "systemError"}
	}
	if binding.active || nativeBusy {
		status = map[string]any{"type": "active", "activeFlags": []any{}}
	}
	return status
}

func codexThread(binding *codexThreadBinding, sess *proto.Session, selection config.SelectedModel) map[string]any {
	var forkedFromID any
	if binding.forkedFromID != "" {
		forkedFromID = binding.forkedFromID
	}
	var parentThreadID any
	if binding.parentThreadID != "" {
		parentThreadID = binding.parentThreadID
	}
	var name any
	if sess.Title != "" {
		name = sess.Title
	}
	var model any
	if selection.Model != "" {
		model = binding.workspace.modelID(selection)
	}
	return map[string]any{
		"id": binding.codexID, "sessionId": binding.codexID, "forkedFromId": forkedFromID, "parentThreadId": parentThreadID, "name": name, "cliVersion": version.Version,
		"createdAt": sess.CreatedAt, "updatedAt": sess.UpdatedAt,
		"cwd": binding.workspace.value.Path, "ephemeral": binding.ephemeral, "model": model, "modelProvider": "crux",
		"preview": sess.Title, "projectId": codexNullableProjectID(binding.projectID), "source": "appServer",
		"status": codexThreadStatus(binding, sess.IsBusy), "turns": []any{},
	}
}

func codexShellCommandMessage(message proto.Message) bool {
	if message.Role != proto.User || len(message.Parts) == 0 {
		return false
	}
	hasShellCommand := false
	for _, part := range message.Parts {
		switch part.(type) {
		case proto.ShellCommand:
			hasShellCommand = true
		case proto.ProviderMetadataContent, proto.Finish:
		default:
			return false
		}
	}
	return hasShellCommand
}

func codexEffectiveUserMessage(message proto.Message) bool {
	return !message.IsSummaryMessage && message.Role == proto.User && !codexShellCommandMessage(message)
}

func codexThreadTurns(messages []proto.Message, active bool) ([]any, error) {
	type projectedTurn struct {
		id                 string
		runID              string
		items              []any
		assistantText      map[string]int
		assistantReasoning map[string]int
		status             string
		err                any
		startedAt          int64
		completedAt        int64
		terminal           bool
	}
	projected := make([]projectedTurn, 0)
	runTurns := make(map[string]int)
	legacyTurn := -1
	for _, message := range messages {
		if message.IsSummaryMessage || message.Role == proto.System || codexShellCommandMessage(message) {
			continue
		}
		turnIndex := -1
		if message.RunID != "" {
			if index, ok := runTurns[message.RunID]; ok {
				turnIndex = index
			}
		} else if message.Role != proto.User {
			turnIndex = legacyTurn
		}
		if message.Role == proto.User && turnIndex == -1 {
			if message.RunID == "" {
				for _, turn := range projected {
					if turn.runID != "" && !turn.terminal {
						return nil, fmt.Errorf("cannot project native user message %q without a RunID while run %q is active", message.ID, turn.runID)
					}
				}
			}
			turnIndex = len(projected)
			projected = append(projected, projectedTurn{
				id:                 message.ID,
				runID:              message.RunID,
				items:              []any{},
				assistantText:      make(map[string]int),
				assistantReasoning: make(map[string]int),
				status:             "interrupted",
				startedAt:          message.CreatedAt,
				completedAt:        message.UpdatedAt,
			})
			if message.RunID != "" {
				runTurns[message.RunID] = turnIndex
			} else {
				legacyTurn = turnIndex
			}
		}
		if turnIndex < 0 || turnIndex >= len(projected) {
			if message.RunID != "" {
				return nil, fmt.Errorf("cannot project native %s message %q without a user turn for RunID %q", message.Role, message.ID, message.RunID)
			}
			return nil, fmt.Errorf("cannot project native %s message %q without an open legacy user turn", message.Role, message.ID)
		}
		turn := &projected[turnIndex]
		if message.UpdatedAt > turn.completedAt {
			turn.completedAt = message.UpdatedAt
		}
		switch message.Role {
		case proto.User:
			content := make([]any, 0, len(message.Parts))
			for _, part := range message.Parts {
				switch value := part.(type) {
				case proto.TextContent:
					elements := value.TextElements
					if elements == nil {
						elements = []any{}
					}
					content = append(content, map[string]any{"type": "text", "text": value.Text, "textElements": elements})
				case proto.SkillContent:
					content = append(content, map[string]any{"type": "skill", "name": value.Name, "path": value.Path})
				case proto.ImageURLContent:
					input := map[string]any{"type": "image", "url": value.URL}
					if value.Detail != "" {
						input["detail"] = value.Detail
					}
					content = append(content, input)
				case proto.BinaryContent:
					if !strings.HasPrefix(value.MIMEType, "image/") || value.Path == "" {
						return nil, fmt.Errorf("cannot project native user message %q binary content", message.ID)
					}
					content = append(content, map[string]any{"type": "localImage", "path": value.Path})
				case proto.ProviderMetadataContent, proto.Finish:
				default:
					return nil, fmt.Errorf("cannot project native user message %q content %T", message.ID, part)
				}
			}
			var clientID any
			if message.SubmissionID != "" {
				clientID = message.SubmissionID
			}
			turn.items = append(turn.items, map[string]any{"type": "userMessage", "id": message.ID, "clientId": clientID, "content": content})
		case proto.Assistant:
			itemID := message.ItemID
			if itemID == "" {
				itemID = message.ID
			}
			var text strings.Builder
			reasoning := make([]string, 0)
			for _, part := range message.Parts {
				switch value := part.(type) {
				case proto.TextContent:
					text.WriteString(value.Text)
				case proto.ReasoningContent:
					if value.Thinking != "" {
						reasoning = append(reasoning, value.Thinking)
					}
				case proto.Finish:
					turn.terminal = true
					switch value.Reason {
					case proto.FinishReasonCanceled:
						turn.status = "interrupted"
					case proto.FinishReasonError, proto.FinishReasonContentFilter:
						turn.status = "failed"
						turn.err = map[string]any{"message": value.Message, "codexErrorInfo": nil, "additionalDetails": value.Details}
					default:
						turn.status = "completed"
					}
				case proto.ProviderMetadataContent, proto.RetryingContent:
				default:
					return nil, fmt.Errorf("cannot project native assistant message %q content %T", message.ID, part)
				}
			}
			if len(reasoning) != 0 {
				if index, ok := turn.assistantReasoning[itemID]; ok {
					item := turn.items[index].(map[string]any)
					item["content"] = append(item["content"].([]string), reasoning...)
				} else {
					turn.assistantReasoning[itemID] = len(turn.items)
					turn.items = append(turn.items, map[string]any{"type": "reasoning", "id": itemID + "-reasoning", "summary": []string{}, "content": reasoning})
				}
			}
			if text.Len() != 0 {
				if index, ok := turn.assistantText[itemID]; ok {
					item := turn.items[index].(map[string]any)
					item["text"] = item["text"].(string) + text.String()
				} else {
					turn.assistantText[itemID] = len(turn.items)
					turn.items = append(turn.items, map[string]any{"type": "agentMessage", "id": itemID, "text": text.String()})
				}
			}
		case proto.Tool:
			return nil, fmt.Errorf("cannot project native tool message %q without a faithful Codex item mapping", message.ID)
		default:
			return nil, fmt.Errorf("cannot project native message %q with role %q", message.ID, message.Role)
		}
		if message.RunID == "" && turnIndex == legacyTurn && turn.terminal {
			legacyTurn = -1
		}
	}
	turns := make([]any, 0, len(projected))
	for index, turn := range projected {
		status := turn.status
		completedAt := any(turn.completedAt)
		if active && index == len(projected)-1 && !turn.terminal {
			status = "inProgress"
			completedAt = nil
		}
		duration := any(nil)
		if completed, ok := completedAt.(int64); ok && completed >= turn.startedAt {
			duration = completed - turn.startedAt
		}
		turns = append(turns, map[string]any{
			"id": turn.id, "items": turn.items, "itemsView": "full", "status": status, "error": turn.err,
			"startedAt": turn.startedAt, "completedAt": completedAt, "durationMs": duration,
		})
	}
	return turns, nil
}

type codexThreadTurnsCursor struct {
	TurnID        string `json:"turnId"`
	IncludeAnchor *bool  `json:"includeAnchor"`
}

func applyCodexThreadTurnsItemsView(turns []any, itemsView string) error {
	for _, value := range turns {
		turn, ok := value.(map[string]any)
		if !ok {
			return errors.New("cannot project native turn page")
		}
		items, ok := turn["items"].([]any)
		if !ok {
			return fmt.Errorf("cannot project native turn %q items", turn["id"])
		}
		switch itemsView {
		case "notLoaded":
			turn["items"] = []any{}
		case "summary":
			var firstUser any
			var finalAgent any
			for _, itemValue := range items {
				item, itemOK := itemValue.(map[string]any)
				if !itemOK {
					return fmt.Errorf("cannot project native turn %q item", turn["id"])
				}
				switch item["type"] {
				case "userMessage":
					if firstUser == nil {
						firstUser = itemValue
					}
				case "agentMessage":
					finalAgent = itemValue
				}
			}
			summary := make([]any, 0, 2)
			if firstUser != nil {
				summary = append(summary, firstUser)
			}
			if finalAgent != nil {
				summary = append(summary, finalAgent)
			}
			turn["items"] = summary
		case "full":
		default:
			return fmt.Errorf("invalid itemsView: %s", itemsView)
		}
		turn["itemsView"] = itemsView
	}
	return nil
}

func parseCodexThreadTurnsCursor(raw string) (codexThreadTurnsCursor, error) {
	var cursor codexThreadTurnsCursor
	if json.Unmarshal([]byte(raw), &cursor) != nil || cursor.TurnID == "" || cursor.IncludeAnchor == nil {
		return codexThreadTurnsCursor{}, fmt.Errorf("invalid cursor: %s", raw)
	}
	return cursor, nil
}

func encodeCodexThreadTurnsCursor(turnID string, includeAnchor bool) (*string, error) {
	value, err := json.Marshal(codexThreadTurnsCursor{TurnID: turnID, IncludeAnchor: &includeAnchor})
	if err != nil {
		return nil, fmt.Errorf("failed to serialize cursor: %w", err)
	}
	encoded := string(value)
	return &encoded, nil
}

func paginateCodexThreadTurns(turns []any, rawCursor *string, limit *uint32, sortDirection string) ([]any, *string, *string, error) {
	if len(turns) == 0 {
		return []any{}, nil, nil, nil
	}
	var cursor *codexThreadTurnsCursor
	if rawCursor != nil {
		parsed, err := parseCodexThreadTurnsCursor(*rawCursor)
		if err != nil {
			return nil, nil, nil, err
		}
		cursor = &parsed
	}
	pageSize := 25
	if limit != nil {
		pageSize = int(*limit)
		if pageSize < 1 {
			pageSize = 1
		}
		if pageSize > 100 {
			pageSize = 100
		}
	}
	anchorIndex := -1
	if cursor != nil {
		for index, value := range turns {
			turn, ok := value.(map[string]any)
			if ok && turn["id"] == cursor.TurnID {
				anchorIndex = index
				break
			}
		}
		if anchorIndex < 0 {
			return nil, nil, nil, errors.New("invalid cursor: anchor turn is no longer present")
		}
	}
	indexes := make([]int, 0, len(turns))
	if sortDirection == "asc" {
		for index := range turns {
			if cursor == nil || index > anchorIndex || *cursor.IncludeAnchor && index == anchorIndex {
				indexes = append(indexes, index)
			}
		}
	} else {
		for index := len(turns) - 1; index >= 0; index-- {
			if cursor == nil || index < anchorIndex || *cursor.IncludeAnchor && index == anchorIndex {
				indexes = append(indexes, index)
			}
		}
	}
	more := len(indexes) > pageSize
	if more {
		indexes = indexes[:pageSize]
	}
	page := make([]any, 0, len(indexes))
	for _, index := range indexes {
		page = append(page, turns[index])
	}
	if len(page) == 0 {
		return page, nil, nil, nil
	}
	first := page[0].(map[string]any)["id"].(string)
	backwardsCursor, err := encodeCodexThreadTurnsCursor(first, true)
	if err != nil {
		return nil, nil, nil, err
	}
	var nextCursor *string
	if more {
		last := page[len(page)-1].(map[string]any)["id"].(string)
		nextCursor, err = encodeCodexThreadTurnsCursor(last, false)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	return page, nextCursor, backwardsCursor, nil
}

func codexThreadStartResult(thread map[string]any, selection config.SelectedModel, policy codexExecutionPolicy) map[string]any {
	return map[string]any{
		"thread": thread, "model": thread["model"], "modelProvider": "crux",
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

func codexAccountReadResult() map[string]any {
	return map[string]any{"account": map[string]any{"type": "apiKey"}, "requiresOpenaiAuth": false}
}

func codexSkillScope(source string) (string, error) {
	switch source {
	case "system", "user":
		return source, nil
	case "project":
		return "repo", nil
	default:
		return "", fmt.Errorf("unsupported native skill source %q", source)
	}
}

func codexSkillsListEntry(cwd string, catalog proto.SkillCatalogResponse) (map[string]any, error) {
	skillValues := make([]any, 0, len(catalog.Skills))
	for _, skill := range catalog.Skills {
		scope, err := codexSkillScope(skill.Source)
		if err != nil {
			return nil, err
		}
		skillValues = append(skillValues, map[string]any{
			"name":        skill.Name,
			"description": skill.Description,
			"path":        skill.ID,
			"scope":       scope,
			"enabled":     skill.Enabled,
			"pluginId":    nil,
		})
	}
	errorValues := make([]any, 0)
	for _, state := range catalog.States {
		if state.State != proto.SkillStateError {
			continue
		}
		errorValues = append(errorValues, map[string]any{"path": state.Path, "message": state.Error})
	}
	return map[string]any{"cwd": cwd, "skills": skillValues, "errors": errorValues}, nil
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
	workspace.registerModelIDs(surfaces)
	for _, surface := range surfaces {
		if surface.Owner != nil {
			if surface.Owner.ProviderID != surface.ID {
				return nil, fmt.Errorf("provider owner %s does not match surface provider %s", surface.Owner.ProviderID, surface.ID)
			}
			owners[surface.ID] = *surface.Owner
		}
		for _, model := range surface.Models {
			selection := config.SelectedModel{Provider: surface.ID, Model: model.ID}
			externalID := workspace.modelID(selection)
			catalog[externalID] = selection
			displayName := model.Name
			if displayName == "" {
				displayName = model.ID
			}
			effort := model.DefaultReasoningEffort
			levels := model.ReasoningLevels
			if !model.CanReason {
				effort = "none"
				levels = []string{"none"}
			} else if effort == "" && len(levels) > 0 {
				effort = levels[0]
			} else if effort == "" {
				effort = "medium"
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
				"id": externalID, "model": externalID, "displayName": externalID,
				"description": surface.Name + " - " + displayName,
				"hidden":      false, "isDefault": selected.Provider == surface.ID && selected.Model == model.ID,
				"defaultReasoningEffort": effort, "supportedReasoningEfforts": reasoning,
				"inputModalities": modalities, "supportsPersonality": false,
				"upgrade": nil, "upgradeInfo": nil, "availabilityNux": nil, "modelSpecialty": nil,
				"multiAgentVersion": nil, "additionalSpeedTiers": []string{}, "serviceTiers": []any{}, "defaultServiceTier": nil, "availableAccessPrograms": nil,
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

func paginateCodexModels(models []any, cursor *string, limit *uint32) ([]any, any, error) {
	if len(models) == 0 {
		return []any{}, nil, nil
	}
	start := 0
	if cursor != nil {
		parsed, err := strconv.ParseUint(*cursor, 10, 64)
		if err != nil || parsed > uint64(^uint(0)>>1) {
			return nil, nil, fmt.Errorf("invalid cursor: %s", *cursor)
		}
		start = int(parsed)
	}
	if start > len(models) {
		return nil, nil, fmt.Errorf("cursor %d exceeds total models %d", start, len(models))
	}
	pageSize := len(models)
	if limit != nil {
		pageSize = int(*limit)
		if pageSize == 0 {
			pageSize = 1
		}
		if pageSize > len(models) {
			pageSize = len(models)
		}
	}
	end := start + pageSize
	if end > len(models) {
		end = len(models)
	}
	var nextCursor any
	if end < len(models) {
		nextCursor = strconv.Itoa(end)
	}
	return models[start:end], nextCursor, nil
}

type codexThreadListEntry struct {
	thread    any
	id        string
	createdAt int64
	updatedAt int64
}

type codexThreadListCursor struct {
	Version       int    `json:"version"`
	SortKey       string `json:"sortKey"`
	SortDirection string `json:"sortDirection"`
	Timestamp     int64  `json:"timestamp"`
	ThreadID      string `json:"threadId"`
	IncludeAnchor bool   `json:"includeAnchor"`
}

func codexThreadListTimestamp(entry codexThreadListEntry, sortKey string) int64 {
	if sortKey == "updated_at" {
		return entry.updatedAt
	}
	return entry.createdAt
}

func compareCodexThreadListPosition(leftTimestamp int64, leftID string, rightTimestamp int64, rightID, sortDirection string) int {
	comparison := 0
	switch {
	case leftTimestamp < rightTimestamp:
		comparison = -1
	case leftTimestamp > rightTimestamp:
		comparison = 1
	case leftID < rightID:
		comparison = -1
	case leftID > rightID:
		comparison = 1
	}
	if sortDirection == "desc" {
		return -comparison
	}
	return comparison
}

func encodeCodexThreadListCursor(entry codexThreadListEntry, sortKey, sortDirection string, includeAnchor bool) string {
	encoded, _ := json.Marshal(codexThreadListCursor{
		Version:       1,
		SortKey:       sortKey,
		SortDirection: sortDirection,
		Timestamp:     codexThreadListTimestamp(entry, sortKey),
		ThreadID:      entry.id,
		IncludeAnchor: includeAnchor,
	})
	return string(encoded)
}

func decodeCodexThreadListCursor(value, sortKey, sortDirection string) (codexThreadListCursor, error) {
	var cursor codexThreadListCursor
	if err := json.Unmarshal([]byte(value), &cursor); err != nil || cursor.Version != 1 || cursor.ThreadID == "" {
		return codexThreadListCursor{}, fmt.Errorf("invalid cursor: %s", value)
	}
	if cursor.SortKey != sortKey || cursor.SortDirection != sortDirection {
		return codexThreadListCursor{}, fmt.Errorf("cursor sort options do not match request")
	}
	return cursor, nil
}

func paginateCodexThreads(entries []codexThreadListEntry, cursorValue *string, limit *uint32, sortKey, sortDirection string) ([]any, any, any, error) {
	if sortKey == "" {
		sortKey = "created_at"
	}
	if sortDirection == "" {
		sortDirection = "desc"
	}
	if sortKey != "created_at" && sortKey != "updated_at" {
		return nil, nil, nil, fmt.Errorf("thread/list sortKey %q is not supported", sortKey)
	}
	if sortDirection != "asc" && sortDirection != "desc" {
		return nil, nil, nil, fmt.Errorf("invalid thread/list sortDirection %q", sortDirection)
	}
	sort.Slice(entries, func(i, j int) bool {
		return compareCodexThreadListPosition(
			codexThreadListTimestamp(entries[i], sortKey), entries[i].id,
			codexThreadListTimestamp(entries[j], sortKey), entries[j].id,
			sortDirection,
		) < 0
	})
	start := 0
	if cursorValue != nil {
		cursor, err := decodeCodexThreadListCursor(*cursorValue, sortKey, sortDirection)
		if err != nil {
			return nil, nil, nil, err
		}
		start = len(entries)
		for i, entry := range entries {
			comparison := compareCodexThreadListPosition(
				codexThreadListTimestamp(entry, sortKey), entry.id,
				cursor.Timestamp, cursor.ThreadID,
				sortDirection,
			)
			if comparison > 0 || comparison == 0 && cursor.IncludeAnchor {
				start = i
				break
			}
		}
	}
	pageSize := 25
	if limit != nil {
		pageSize = int(*limit)
		if pageSize < 1 {
			pageSize = 1
		}
		if pageSize > 100 {
			pageSize = 100
		}
	}
	end := start + pageSize
	if end > len(entries) {
		end = len(entries)
	}
	selected := entries[start:end]
	page := make([]any, 0, len(selected))
	for _, entry := range selected {
		page = append(page, entry.thread)
	}
	var nextCursor any
	var backwardsCursor any
	if len(selected) != 0 {
		oppositeDirection := "asc"
		if sortDirection == "asc" {
			oppositeDirection = "desc"
		}
		backwardsCursor = encodeCodexThreadListCursor(selected[0], sortKey, oppositeDirection, true)
		if end < len(entries) {
			nextCursor = encodeCodexThreadListCursor(selected[len(selected)-1], sortKey, sortDirection, false)
		}
	}
	return page, nextCursor, backwardsCursor, nil
}

func (b *codexNativeBridge) applyModelSelection(ctx context.Context, workspace *codexNativeWorkspace, selection config.SelectedModel) error {
	owner, ok := b.modelOwners[workspace][selection.Provider]
	if !ok {
		return fmt.Errorf("provider owner is unavailable for %s", selection.Provider)
	}
	state, err := workspace.client.OverrideModels(ctx, workspace.value.ID, config.AgentModelState{
		Large: &config.OwnedSelectedModel{Model: selection, Owner: owner},
	})
	if err != nil {
		return err
	}
	return workspace.client.UpdateAgent(ctx, workspace.value.ID, state)
}

func (b *codexNativeBridge) restoreModelSelection(ctx context.Context, workspace *codexNativeWorkspace, selection config.SelectedModel) error {
	if selection.Model == "" || selection.Provider == "" {
		return errors.New("previous workspace model is unavailable")
	}
	if err := b.applyModelSelection(ctx, workspace, selection); err != nil {
		return err
	}
	b.selectedModel[workspace] = selection
	return nil
}

func (b *codexNativeBridge) selectModel(ctx context.Context, workspace *codexNativeWorkspace, modelID, providerID string) (config.SelectedModel, error) {
	if b.models[workspace] == nil {
		if _, err := b.listModels(ctx, workspace); err != nil {
			return config.SelectedModel{}, err
		}
	}
	catalog := b.models[workspace]
	selection := b.selectedModel[workspace]
	if modelID == "" {
		if selection.Provider == "" || selection.Model == "" {
			return config.SelectedModel{}, errors.New("configured default model is unavailable")
		}
		if providerID != "" && selection.Provider != providerID {
			return config.SelectedModel{}, fmt.Errorf("configured default model uses provider %q, not requested provider %q", selection.Provider, providerID)
		}
	} else {
		selection = config.SelectedModel{}
		if candidate, ok := catalog[modelID]; ok {
			if providerID != "" && providerID != candidate.Provider {
				return config.SelectedModel{}, fmt.Errorf("model %q uses provider %q, not requested provider %q", modelID, candidate.Provider, providerID)
			}
			selection = candidate
		} else {
			matches := make([]config.SelectedModel, 0, 1)
			for _, candidate := range catalog {
				if candidate.Model == modelID && (providerID == "" || candidate.Provider == providerID) {
					matches = append(matches, candidate)
				}
			}
			if len(matches) == 0 && providerID == "" {
				for _, candidate := range catalog {
					if candidate.Provider+"/"+candidate.Model == modelID {
						matches = append(matches, candidate)
					}
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
	}
	if selection.Model == "" || selection.Provider == "" {
		if providerID != "" {
			return config.SelectedModel{}, fmt.Errorf("model %q is not available from provider %q", modelID, providerID)
		}
		return config.SelectedModel{}, fmt.Errorf("model %q is not available", modelID)
	}
	previous := b.selectedModel[workspace]
	if err := b.applyModelSelection(ctx, workspace, selection); err != nil {
		rollbackCtx, cancelRollback := context.WithTimeout(context.Background(), codexServerReadyTimeout)
		rollbackErr := b.restoreModelSelection(rollbackCtx, workspace, previous)
		cancelRollback()
		return config.SelectedModel{}, errors.Join(err, rollbackErr)
	}
	b.selectedModel[workspace] = selection
	return selection, nil
}

func (b *codexNativeBridge) selectThreadModel(ctx context.Context, binding *codexThreadBinding, modelID, providerID string) (selected config.SelectedModel, returnErr error) {
	if modelID == "" && binding.model.Model != "" {
		if b.models[binding.workspace] == nil {
			if _, err := b.listModels(ctx, binding.workspace); err != nil {
				return config.SelectedModel{}, err
			}
		}
		modelID = binding.workspace.modelID(binding.model)
		candidate, ok := b.models[binding.workspace][modelID]
		if !ok || candidate.Provider != binding.model.Provider || candidate.Model != binding.model.Model {
			return config.SelectedModel{}, fmt.Errorf("thread model %q is not available from provider %q", binding.model.Model, binding.model.Provider)
		}
		if providerID == "" {
			providerID = binding.model.Provider
		}
	}
	previous := b.selectedModel[binding.workspace]
	selection, err := b.selectModel(ctx, binding.workspace, modelID, providerID)
	if err != nil {
		return config.SelectedModel{}, err
	}
	defer func() {
		if returnErr != nil {
			rollbackCtx, cancel := context.WithTimeout(context.Background(), codexServerReadyTimeout)
			defer cancel()
			returnErr = errors.Join(returnErr, b.restoreModelSelection(rollbackCtx, binding.workspace, previous))
		}
	}()
	current, err := binding.workspace.client.GetSession(ctx, binding.workspace.value.ID, binding.nativeID)
	if err != nil {
		return config.SelectedModel{}, err
	}
	var models config.AgentModelState
	if len(current.Models) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(current.Models))
		decoder.UseNumber()
		if err := decoder.Decode(&models); err != nil {
			return config.SelectedModel{}, err
		}
	} else {
		workspace, err := binding.workspace.client.GetWorkspace(ctx, binding.workspace.value.ID)
		if err != nil {
			return config.SelectedModel{}, err
		}
		if workspace.Config == nil {
			return config.SelectedModel{}, errors.New("workspace configuration is missing")
		}
		models = workspace.Config.AgentModelState()
	}
	owner, ok := b.modelOwners[binding.workspace][selection.Provider]
	if !ok {
		return config.SelectedModel{}, errors.New("session model provider owner is unavailable")
	}
	selection.ReasoningEffort = current.AnalysisEffort
	models.Large = &config.OwnedSelectedModel{Owner: owner, Model: selection}
	if _, err := binding.workspace.client.SetSessionModels(ctx, binding.workspace.value.ID, binding.nativeID, current.ModelRevision, models); err != nil {
		return config.SelectedModel{}, err
	}
	binding.model = selection
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
	threadID, turnID, itemID, itemType, text string
	cancelled                                bool
	err                                      error
	outputErr                                error
}

type codexShellResult struct {
	binding                           *codexThreadBinding
	threadID, turnID, itemID, command string
	output                            string
	exitCode                          int
	durationMs                        int64
	standalone                        bool
	timedOut                          bool
	err                               error
}

func codexShellCommandItem(result codexShellResult, status string, completed bool) map[string]any {
	item := map[string]any{
		"id": result.itemID, "type": "commandExecution", "pluginId": nil, "scriptPath": nil,
		"command": result.command, "cwd": result.binding.workspace.value.Path, "processId": nil,
		"source": "userShell", "status": status, "commandActions": []any{},
		"aggregatedOutput": nil, "exitCode": nil, "durationMs": nil,
	}
	if completed {
		item["aggregatedOutput"] = result.output
		item["exitCode"] = result.exitCode
		item["durationMs"] = result.durationMs
	}
	return item
}

func (b *codexNativeBridge) runShellCommand(ctx context.Context, binding *codexThreadBinding, threadID, turnID, itemID, command string, standalone bool) codexShellResult {
	started := time.Now()
	result := codexShellResult{binding: binding, threadID: threadID, turnID: turnID, itemID: itemID, command: command, standalone: standalone}
	response, err := binding.workspace.client.RunShellCommand(ctx, binding.workspace.value.ID, binding.nativeID, command, 80)
	result.durationMs = time.Since(started).Milliseconds()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		result.timedOut = true
		result.exitCode = -1
		result.output = response.Output
		if result.output != "" && !strings.HasSuffix(result.output, "\n") {
			result.output += "\n"
		}
		result.output += "Timeout"
		return result
	}
	if err != nil {
		result.err = err
		result.exitCode = -1
		result.output = err.Error()
		return result
	}
	result.output = response.Output
	result.exitCode = response.ExitCode
	return result
}

type codexTurnInput struct {
	Type         string          `json:"type"`
	Text         string          `json:"text"`
	Name         string          `json:"name"`
	Path         string          `json:"path"`
	Detail       json.RawMessage `json:"detail"`
	TextElements json.RawMessage `json:"textElements"`
}

func validateCodexTurnInputPresence(raw json.RawMessage) error {
	var params map[string]json.RawMessage
	if err := json.Unmarshal(raw, &params); err != nil {
		return errors.New("turn params must be an object")
	}
	rawInput, exists := params["input"]
	if !exists || strings.TrimSpace(string(rawInput)) == "null" {
		return errors.New("input must be an array")
	}
	var inputs []json.RawMessage
	if err := json.Unmarshal(rawInput, &inputs); err != nil {
		return errors.New("input must be an array")
	}
	for index, rawInput := range inputs {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(rawInput, &fields); err != nil || fields == nil {
			return fmt.Errorf("input[%d] must be an object", index)
		}
		var inputType string
		if err := json.Unmarshal(fields["type"], &inputType); err != nil || inputType == "" {
			return fmt.Errorf("input[%d].type must be a non-empty string", index)
		}
		requireField := func(name string) error {
			value, present := fields[name]
			if !present || strings.TrimSpace(string(value)) == "null" {
				return fmt.Errorf("input[%d].%s is required", index, name)
			}
			return nil
		}
		switch inputType {
		case "text":
			if err := requireField("text"); err != nil {
				return err
			}
		case "localImage":
			if err := requireField("path"); err != nil {
				return err
			}
		case "skill":
			if err := requireField("name"); err != nil {
				return err
			}
			if err := requireField("path"); err != nil {
				return err
			}
		}
	}
	return nil
}

func codexNativeTurnInput(inputs []codexTurnInput) ([]proto.AgentInputPart, error) {
	parts := make([]proto.AgentInputPart, 0, len(inputs))
	for _, input := range inputs {
		switch input.Type {
		case "text":
			elements, err := codexTextElements(input.TextElements)
			if err != nil {
				return nil, err
			}
			parts = append(parts, proto.AgentInputPart{Type: "text", Text: input.Text, TextElements: elements})
		case "localImage":
			if input.Path == "" {
				return nil, errors.New("localImage path must not be empty")
			}
			if len(input.Detail) != 0 && string(input.Detail) != "null" {
				return nil, errors.New("localImage.detail is not supported by the Crux compatibility layer")
			}
			parts = append(parts, proto.AgentInputPart{Type: "local_file", Path: input.Path})
		case "skill":
			if input.Name == "" {
				return nil, errors.New("skill name must not be empty")
			}
			if input.Path == "" {
				return nil, errors.New("skill path must not be empty")
			}
			parts = append(parts, proto.AgentInputPart{Type: "skill", Text: input.Name, Path: input.Path})
		default:
			return nil, fmt.Errorf("turn input type %q is not supported by the Crux compatibility layer", input.Type)
		}
	}
	return parts, nil
}

func codexTextElements(raw json.RawMessage) ([]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return []any{}, nil
	}
	var elements []any
	if err := json.Unmarshal(raw, &elements); err != nil {
		return nil, errors.New("text.textElements must be an array")
	}
	return elements, nil
}

func codexUserMessageContent(inputs []codexTurnInput) ([]any, error) {
	content := make([]any, 0, len(inputs))
	for _, input := range inputs {
		switch input.Type {
		case "text":
			elements, err := codexTextElements(input.TextElements)
			if err != nil {
				return nil, err
			}
			content = append(content, map[string]any{"type": "text", "text": input.Text, "textElements": elements})
		case "localImage":
			content = append(content, map[string]any{"type": "localImage", "path": input.Path})
		case "skill":
			content = append(content, map[string]any{"type": "skill", "name": input.Name, "path": input.Path})
		default:
			return nil, fmt.Errorf("turn input type %q is not supported by the Crux compatibility layer", input.Type)
		}
	}
	return content, nil
}

func (b *codexNativeBridge) runTurn(ctx context.Context, binding *codexThreadBinding, turnID, itemID, runID string, inputParts []proto.AgentInputPart, permissionMode proto.AgentPermissionMode, output *protocolOutput, state *codexConnectionState, admitted chan<- error, started <-chan struct{}) codexTurnResult {
	workspace := binding.workspace
	result := codexTurnResult{threadID: binding.codexID, turnID: turnID, itemID: itemID, itemType: "agentMessage"}
	if err := b.ensureAgent(ctx, workspace); err != nil {
		admitted <- err
		result.err = err
		return result
	}
	eventCtx, cancelEvents := context.WithCancel(ctx)
	defer cancelEvents()
	events, err := workspace.client.SubscribeEvents(eventCtx, workspace.value.ID)
	if err != nil {
		admitted <- err
		result.err = err
		return result
	}
	if err := workspace.client.SendAgentMessage(ctx, workspace.value.ID, proto.AgentMessage{
		SessionID: binding.nativeID, RunID: runID, MessageRunID: runID, UserMessageID: turnID, AssistantItemID: itemID, WaitForAdmission: true, InputParts: inputParts, PermissionMode: permissionMode,
	}); err != nil {
		admitted <- err
		result.err = err
		return result
	}
	admitted <- nil
	cancelActiveTurn := func(cause error) codexTurnResult {
		cancelCtx, cancel := context.WithTimeout(context.Background(), codexServerReadyTimeout)
		defer cancel()
		result.err = errors.Join(cause, workspace.client.CancelAgentSession(cancelCtx, workspace.value.ID, binding.nativeID))
		result.cancelled = true
		return result
	}
	select {
	case <-ctx.Done():
		return cancelActiveTurn(nil)
	case <-started:
	}
	read := make(map[string]int)
	for {
		select {
		case <-ctx.Done():
			return cancelActiveTurn(nil)
		case event, ok := <-events:
			if !ok {
				return cancelActiveTurn(errors.New("Crux event stream closed"))
			}
			switch value := event.(type) {
			case pubsub.Event[proto.Message]:
				msg := value.Payload
				if msg.SessionID != binding.nativeID || msg.Role != proto.Assistant || msg.RunID != runID {
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
					if err := state.writeThreadNotification(output, binding, "item/agentMessage/delta", map[string]any{"threadId": binding.codexID, "turnId": turnID, "itemId": itemID, "delta": delta}); err != nil {
						result.outputErr = err
						return cancelActiveTurn(err)
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
						if err := state.writeThreadNotification(output, binding, "item/agentMessage/delta", map[string]any{"threadId": binding.codexID, "turnId": turnID, "itemId": itemID, "delta": delta}); err != nil {
							result.err = err
							result.outputErr = err
							return result
						}
					}
				}
				result.cancelled = complete.Cancelled
				if complete.Error != "" && !complete.Cancelled {
					result.err = errors.New(complete.Error)
				}
				return result
			case pubsub.Event[proto.AgentEvent]:
				if value.Payload.Error != nil && value.Payload.RunID == runID {
					return cancelActiveTurn(value.Payload.Error)
				}
			}
		}
	}
}

func (b *codexNativeBridge) runCompaction(ctx context.Context, binding *codexThreadBinding, turnID, itemID string) codexTurnResult {
	result := codexTurnResult{threadID: binding.codexID, turnID: turnID, itemID: itemID, itemType: "contextCompaction"}
	if err := b.ensureAgent(ctx, binding.workspace); err != nil {
		result.err = err
		return result
	}
	if err := binding.workspace.client.AgentSummarizeSession(ctx, binding.workspace.value.ID, binding.nativeID); err != nil {
		if ctx.Err() != nil {
			result.cancelled = true
		} else {
			result.err = err
		}
	}
	return result
}

func runCodexAppServerNative(ctx context.Context, invocation compatibility.Invocation, request compatibility.Request) (returnErr error) {
	inputCloser, ok := request.Prompt.Stdin.(io.Closer)
	if !ok {
		return errors.New("Codex app-server stdin must be closable")
	}
	serverCtx, cancelServer := context.WithCancel(ctx)
	defer cancelServer()
	bridge := newCodexNativeBridgeState(serverCtx, invocation, request)
	defer func() {
		returnErr = errors.Join(returnErr, bridge.Close())
	}()
	output := &protocolOutput{writer: invocation.Stdout}
	lines := make(chan []byte)
	scanErrors := make(chan error, 1)
	var workers sync.WaitGroup
	workerDeliveryCtx, stopWorkerDelivery := context.WithCancel(context.Background())
	workers.Add(1)
	go func() {
		defer workers.Done()
		defer close(lines)
		scanner := scanProtocolInput(request.Prompt.Stdin)
		defer func() {
			scanErrors <- scanner.Err()
		}()
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			select {
			case lines <- line:
			case <-serverCtx.Done():
				return
			}
		}
	}()
	state := &codexConnectionState{optOutNotificationMethods: make(map[string]struct{})}
	turnDone := make(chan codexTurnResult, 64)
	shellDone := make(chan codexShellResult, 16)
	fuzzyDone := make(chan codexFuzzySearchResult, 64)
	fuzzyTokenGenerations := make(map[string]uint64)
	fuzzySessionGenerations := make(map[string]uint64)
	fuzzyTokenCancels := make(map[string]context.CancelFunc)
	fuzzyOwner := uuid.NewString()
	fuzzyWorkspaces := make(map[*codexNativeWorkspace]struct{})
	fuzzyContext, cancelFuzzy := context.WithCancel(serverCtx)
	defer cancelFuzzy()
	defer func() {
		var cleanupErrors []error
		for workspace := range fuzzyWorkspaces {
			cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), codexServerReadyTimeout)
			if err := workspace.client.CloseWorkspaceFileSearchOwner(cleanupCtx, proto.WorkspaceFileSearchOwnerRequest{Owner: fuzzyOwner}); err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("close fuzzy file search owner for workspace %s: %w", workspace.value.ID, err))
			}
			cancelCleanup()
		}
		returnErr = errors.Join(returnErr, errors.Join(cleanupErrors...))
	}()
	activeFuzzy := 0
	contextDone := serverCtx.Done()
	var shutdownErr error
	var scanErr error
	type activeTurn struct {
		threadID, turnID  string
		runID             string
		assistantItemID   string
		binding           *codexThreadBinding
		permissionMode    proto.AgentPermissionMode
		ctx               context.Context
		cancel            context.CancelFunc
		items             []any
		pendingInterrupts []json.RawMessage
	}
	activeTurns := make(map[string]*activeTurn)
	pendingTurns := make(map[string]codexTurnResult)
	runningShells := make(map[string]context.CancelFunc)
	defer func() {
		cancelServer()
		cancelFuzzy()
		stopWorkerDelivery()
		for _, active := range activeTurns {
			active.cancel()
		}
		for _, cancelShell := range runningShells {
			cancelShell()
		}
		_ = inputCloser.Close()
		workers.Wait()
	}()
	activeForThread := func(threadID string) *activeTurn {
		if active := activeTurns[threadID]; active != nil {
			return active
		}
		for _, active := range activeTurns {
			if active.binding.nativeID == threadID {
				return active
			}
		}
		return nil
	}
	finishTurn := func(result codexTurnResult) error {
		active := activeTurns[result.threadID]
		if active == nil {
			return nil
		}
		pendingInterrupts := active.pendingInterrupts
		active.pendingInterrupts = nil
		for _, requestID := range pendingInterrupts {
			if err := writeCodexResult(output, requestID, map[string]any{}); err != nil {
				return err
			}
		}
		if _, running := runningShells[result.threadID]; running {
			pendingTurns[result.threadID] = result
			return nil
		}
		active.binding.loaded = true
		active.binding.active = false
		active.binding.systemError = result.err != nil && !result.cancelled
		if err := state.writeThreadNotification(output, active.binding, "thread/status/changed", map[string]any{"threadId": active.threadID, "status": codexThreadStatus(active.binding, false)}); err != nil {
			return err
		}
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
		item := map[string]any{"id": result.itemID, "type": result.itemType}
		if result.itemType == "agentMessage" {
			item["status"] = itemStatus
			item["text"] = result.text
		}
		if err := state.writeThreadNotification(output, active.binding, "item/completed", map[string]any{"threadId": result.threadID, "turnId": result.turnID, "item": item}); err != nil {
			return err
		}
		items := append(append([]any(nil), active.items...), item)
		turn := map[string]any{"id": result.turnID, "status": status, "items": items, "error": turnError}
		if err := state.writeThreadNotification(output, active.binding, "turn/completed", map[string]any{"threadId": result.threadID, "turn": turn}); err != nil {
			return err
		}
		bridge.scheduleThreadUnload(active.binding)
		delete(activeTurns, result.threadID)
		return nil
	}
	for lines != nil || len(activeTurns) != 0 || len(runningShells) != 0 || activeFuzzy != 0 {
		select {
		case <-contextDone:
			shutdownErr = serverCtx.Err()
			contextDone = nil
			lines = nil
			cancelFuzzy()
			for _, active := range activeTurns {
				active.cancel()
			}
			for _, cancelShell := range runningShells {
				cancelShell()
			}
			_ = inputCloser.Close()
		case signal := <-bridge.unloadReady:
			binding := bridge.unloadThread(signal)
			if binding == nil {
				continue
			}
			if err := state.writeNotification(output, "thread/status/changed", map[string]any{"threadId": binding.codexID, "status": codexThreadStatus(binding, false)}); err != nil {
				return err
			}
			if err := state.writeNotification(output, "thread/closed", map[string]any{"threadId": binding.codexID}); err != nil {
				return err
			}
		case result := <-fuzzyDone:
			activeFuzzy--
			if result.sessionID != "" {
				if result.sessionGeneration != fuzzySessionGenerations[result.sessionID] || !result.accepted {
					continue
				}
				if result.err != nil {
					result.files = []codexFuzzyFile{}
				}
				if err := state.writeNotification(output, "fuzzyFileSearch/sessionUpdated", map[string]any{"sessionId": result.sessionID, "query": result.query, "files": result.files}); err != nil {
					return err
				}
				if err := state.writeNotification(output, "fuzzyFileSearch/sessionCompleted", map[string]any{"sessionId": result.sessionID}); err != nil {
					return err
				}
				continue
			}
			if result.hasCancellationToken && fuzzyTokenGenerations[result.cancellationToken] == result.tokenGeneration {
				delete(fuzzyTokenGenerations, result.cancellationToken)
				delete(fuzzyTokenCancels, result.cancellationToken)
			}
			if result.err != nil && !errors.Is(result.err, context.Canceled) {
				if err := writeCodexError(output, result.requestID, -32000, result.err.Error()); err != nil {
					return err
				}
				continue
			}
			if result.files == nil {
				result.files = []codexFuzzyFile{}
			}
			if err := writeCodexResult(output, result.requestID, map[string]any{"files": result.files}); err != nil {
				return err
			}
		case result := <-shellDone:
			delete(runningShells, result.threadID)
			itemStatus := "completed"
			if result.timedOut || result.err != nil || result.exitCode != 0 {
				itemStatus = "failed"
			}
			if result.output != "" {
				if err := state.writeThreadNotification(output, result.binding, "item/commandExecution/outputDelta", map[string]any{"threadId": result.threadID, "turnId": result.turnID, "itemId": result.itemID, "delta": result.output}); err != nil {
					return err
				}
			}
			item := codexShellCommandItem(result, itemStatus, true)
			if err := state.writeThreadNotification(output, result.binding, "item/completed", map[string]any{"threadId": result.threadID, "turnId": result.turnID, "item": item}); err != nil {
				return err
			}
			if result.standalone {
				result.binding.active = false
				result.binding.systemError = result.err != nil
				if err := state.writeThreadNotification(output, result.binding, "thread/status/changed", map[string]any{"threadId": result.threadID, "status": codexThreadStatus(result.binding, false)}); err != nil {
					return err
				}
				turnStatus := "completed"
				var turnError any
				if result.err != nil {
					turnStatus = "failed"
					turnError = map[string]any{"message": result.err.Error()}
				}
				turn := map[string]any{"id": result.turnID, "status": turnStatus, "items": []any{item}, "error": turnError}
				if err := state.writeThreadNotification(output, result.binding, "turn/completed", map[string]any{"threadId": result.threadID, "turn": turn}); err != nil {
					return err
				}
				bridge.scheduleThreadUnload(result.binding)
			}
			if pending, ok := pendingTurns[result.threadID]; ok {
				delete(pendingTurns, result.threadID)
				if err := finishTurn(pending); err != nil {
					return err
				}
			}
		case result := <-turnDone:
			if result.outputErr != nil {
				return result.outputErr
			}
			if err := finishTurn(result); err != nil {
				return err
			}
		case line, ok := <-lines:
			if !ok {
				lines = nil
				scanErr = <-scanErrors
				if scanErr != nil {
					shutdownErr = scanErr
					cancelFuzzy()
				}
				for _, active := range activeTurns {
					active.cancel()
				}
				for _, cancelShell := range runningShells {
					cancelShell()
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
			if message.Method != "initialize" && !state.initialized {
				if len(message.ID) != 0 {
					_ = writeCodexError(output, message.ID, -32600, "Not initialized")
				}
				continue
			}
			if !bridge.ready && slices.Contains(codexSupportedMethods(), message.Method) && !slices.Contains([]string{"initialize", "initialized", "getAuthStatus", "account/read", "account/gatewayOAuth/read", "account/login/start", "account/logout"}, message.Method) {
				started, startErr := connectCodexNativeBridge(serverCtx, invocation, request)
				if startErr != nil {
					_ = writeCodexError(output, message.ID, -32000, startErr.Error())
					continue
				}
				started.unloadDelay = bridge.unloadDelay
				bridge = started
			}
			switch message.Method {
			case "initialize":
				if state.initialized {
					_ = writeCodexError(output, message.ID, -32600, "Already initialized")
					continue
				}
				initializedState, clientName, _, initializeErr := parseCodexInitializeParams(message.Params)
				if initializeErr != nil {
					errorMessage := "Invalid request: " + initializeErr.Error()
					if strings.HasPrefix(initializeErr.Error(), "Invalid clientInfo.name:") {
						errorMessage = initializeErr.Error()
					}
					_ = writeCodexError(output, message.ID, -32600, errorMessage)
					continue
				}
				home, homeErr := codexHome(invocation.Env, request.WorkingDir)
				if homeErr != nil {
					_ = writeCodexError(output, message.ID, -32600, "Invalid request: resolve server home")
					continue
				}
				userAgent, userAgentErr := codexInitializeUserAgent(ctx, invocation)
				if userAgentErr != nil {
					if err := writeCodexError(output, message.ID, -32000, userAgentErr.Error()); err != nil {
						return err
					}
					continue
				}
				initializedState.initialized = true
				initializedState.clientName = clientName
				state = initializedState
				platformFamily := "unix"
				if runtime.GOOS == "windows" {
					platformFamily = "windows"
				}
				if err := writeCodexResult(output, message.ID, map[string]any{"userAgent": userAgent, "codexHome": filepath.Clean(home), "platformFamily": platformFamily, "platformOs": runtime.GOOS}); err != nil {
					return err
				}
			case "initialized":
			case "getAuthStatus":
				var params struct {
					IncludeToken *bool `json:"includeToken"`
					RefreshToken *bool `json:"refreshToken"`
				}
				if len(message.Params) != 0 && string(message.Params) != "null" {
					if err := json.Unmarshal(message.Params, &params); err != nil {
						_ = writeCodexError(output, message.ID, -32600, "Invalid request: getAuthStatus flags must be booleans or null")
						continue
					}
				}
				if err := writeCodexResult(output, message.ID, codexLegacyAuthStatusResult()); err != nil {
					return err
				}
			case "account/read":
				if err := writeCodexResult(output, message.ID, codexAccountReadResult()); err != nil {
					return err
				}
			case "account/gatewayOAuth/read":
				if err := writeCodexResult(output, message.ID, codexGatewayOAuthReadResult()); err != nil {
					return err
				}
			case "account/login/start":
				if err := writeCodexResult(output, message.ID, map[string]any{"type": "apiKey"}); err != nil {
					return err
				}
			case "account/logout":
				if err := writeCodexResult(output, message.ID, map[string]any{}); err != nil {
					return err
				}
			case "configRequirements/read":
				workspace, wsErr := bridge.workspace(request.WorkingDir)
				if wsErr != nil {
					_ = writeCodexError(output, message.ID, -32000, wsErr.Error())
					continue
				}
				requirements, requirementsErr := workspace.client.GetConfigRequirements(ctx)
				if requirementsErr != nil {
					_ = writeCodexError(output, message.ID, -32000, requirementsErr.Error())
					continue
				}
				if err := writeCodexResult(output, message.ID, requirements); err != nil {
					return err
				}
			case "skills/list":
				var params struct {
					CWDs        []string `json:"cwds"`
					ForceReload bool     `json:"forceReload"`
				}
				if len(message.Params) != 0 && string(message.Params) != "null" {
					if paramsErr := json.Unmarshal(message.Params, &params); paramsErr != nil {
						_ = writeCodexError(output, message.ID, -32600, "Invalid request: "+paramsErr.Error())
						continue
					}
				}
				if len(params.CWDs) == 0 {
					params.CWDs = []string{request.WorkingDir}
				}
				data := make([]any, 0, len(params.CWDs))
				failed := false
				for _, cwd := range params.CWDs {
					if !filepath.IsAbs(cwd) {
						_ = writeCodexError(output, message.ID, -32600, "Invalid request: cwd must be an absolute path")
						failed = true
						break
					}
					workspace, wsErr := bridge.workspace(cwd)
					if wsErr != nil {
						_ = writeCodexError(output, message.ID, -32000, wsErr.Error())
						failed = true
						break
					}
					catalog, catalogErr := workspace.client.SkillCatalog(ctx, workspace.value.ID, params.ForceReload)
					if catalogErr != nil {
						_ = writeCodexError(output, message.ID, -32000, catalogErr.Error())
						failed = true
						break
					}
					entry, entryErr := codexSkillsListEntry(filepath.Clean(cwd), catalog)
					if entryErr != nil {
						_ = writeCodexError(output, message.ID, -32000, entryErr.Error())
						failed = true
						break
					}
					data = append(data, entry)
				}
				if failed {
					continue
				}
				if err := writeCodexResult(output, message.ID, map[string]any{"data": data}); err != nil {
					return err
				}
			case "config/batchWrite":
				result, configErr := bridge.writeConfig(ctx, message.Params)
				if configErr != nil {
					if err := writeCodexConfigError(output, message.ID, configErr); err != nil {
						return err
					}
					continue
				}
				if err := writeCodexResult(output, message.ID, result); err != nil {
					return err
				}
			case "config/read":
				var params struct {
					CWD string `json:"cwd"`
				}
				if len(message.Params) != 0 && string(message.Params) != "null" {
					if paramsErr := json.Unmarshal(message.Params, &params); paramsErr != nil {
						_ = writeCodexError(output, message.ID, -32600, "Invalid request: "+paramsErr.Error())
						continue
					}
				}
				var view config.ConfigReadView
				var configErr error
				if params.CWD == "" {
					var nativeClient *client.Client
					nativeClient, configErr = codexClientFactory(request.WorkingDir)
					if configErr == nil {
						view, configErr = nativeClient.ReadConfigView(ctx, "")
					}
				} else {
					var workspace *codexNativeWorkspace
					workspace, configErr = bridge.workspace(params.CWD)
					if configErr == nil {
						view, configErr = workspace.client.ReadConfigView(ctx, workspace.value.ID)
					}
				}
				if configErr != nil {
					_ = writeCodexError(output, message.ID, -32000, configErr.Error())
					continue
				}
				result := codexConfigViewResult(view)
				if err := writeCodexResult(output, message.ID, result); err != nil {
					return err
				}
			case "project/list", "project/read", "project/create", "project/import", "project/update", "project/delete":
				result, changed, changeType, projectErr := bridge.handleProject(ctx, message.Method, message.Params)
				if projectErr != nil {
					if err := writeCodexError(output, message.ID, -32602, projectErr.Error()); err != nil {
						return err
					}
					continue
				}
				if changed != nil {
					if err := state.writeNotification(output, "project/changed", map[string]any{"projectId": changed.ID, "changeType": changeType}); err != nil {
						return err
					}
				}
				if err := writeCodexResult(output, message.ID, result); err != nil {
					return err
				}
			case "plugin/list", "thread/goal/set", "thread/goal/get", "thread/goal/clear", "thread/metadata/update":
				if len(message.ID) != 0 {
					if err := writeCodexError(output, message.ID, -32601, "Method not supported by the Crux compatibility layer"); err != nil {
						return err
					}
				}
			case "fuzzyFileSearch":
				var params struct {
					Query             *string   `json:"query"`
					Roots             *[]string `json:"roots"`
					CancellationToken *string   `json:"cancellationToken"`
				}
				if paramsErr := json.Unmarshal(message.Params, &params); paramsErr != nil {
					_ = writeCodexError(output, message.ID, -32600, "Invalid request: "+paramsErr.Error())
					continue
				}
				if params.Query == nil || params.Roots == nil {
					_ = writeCodexError(output, message.ID, -32600, "Invalid request: fuzzyFileSearch requires query and roots")
					continue
				}
				workspace, workspaceErr := bridge.workspace(request.WorkingDir)
				if workspaceErr != nil {
					_ = writeCodexError(output, message.ID, -32000, workspaceErr.Error())
					continue
				}
				searchContext, cancelSearch := context.WithCancel(fuzzyContext)
				var token string
				var generation uint64
				if params.CancellationToken != nil {
					token = *params.CancellationToken
					if previous := fuzzyTokenCancels[token]; previous != nil {
						previous()
					}
					generation = fuzzyTokenGenerations[token] + 1
					fuzzyTokenGenerations[token] = generation
					fuzzyTokenCancels[token] = cancelSearch
				}
				requestID := append(json.RawMessage(nil), message.ID...)
				query := *params.Query
				roots := append([]string(nil), (*params.Roots)...)
				hasCancellationToken := params.CancellationToken != nil
				activeFuzzy++
				workers.Add(1)
				go func() {
					defer workers.Done()
					defer cancelSearch()
					result := codexFuzzySearchResult{requestID: requestID, hasCancellationToken: hasCancellationToken, cancellationToken: token, tokenGeneration: generation}
					_, result.files, result.err = runCodexFuzzySearch(searchContext, workspace, query, roots)
					select {
					case fuzzyDone <- result:
					case <-workerDeliveryCtx.Done():
					}
				}()
			case "fuzzyFileSearch/sessionStart":
				if !state.experimentalAPI {
					_ = writeCodexError(output, message.ID, -32600, "fuzzyFileSearch/sessionStart requires experimentalApi capability")
					continue
				}
				var params struct {
					SessionID *string   `json:"sessionId"`
					Roots     *[]string `json:"roots"`
				}
				if paramsErr := json.Unmarshal(message.Params, &params); paramsErr != nil || params.SessionID == nil || params.Roots == nil {
					_ = writeCodexError(output, message.ID, -32600, "Invalid request: fuzzyFileSearch/sessionStart requires sessionId and roots")
					continue
				}
				if *params.SessionID == "" {
					_ = writeCodexError(output, message.ID, -32600, "sessionId must not be empty")
					continue
				}
				workspace, workspaceErr := bridge.workspace(request.WorkingDir)
				if workspaceErr != nil {
					_ = writeCodexError(output, message.ID, -32000, workspaceErr.Error())
					continue
				}
				if len(*params.Roots) == 0 {
					_ = writeCodexError(output, message.ID, -32000, "failed to start fuzzy file search session: at least one search root is required")
					continue
				}
				startErr := workspace.client.StartWorkspaceFileSearchSession(ctx, workspace.value.ID, proto.WorkspaceFileSearchSessionStartRequest{Owner: fuzzyOwner, SessionID: *params.SessionID, Roots: append([]string(nil), (*params.Roots)...)})
				if startErr != nil {
					_ = writeCodexError(output, message.ID, -32000, "failed to start fuzzy file search session: "+startErr.Error())
					continue
				}
				fuzzyWorkspaces[workspace] = struct{}{}
				fuzzySessionGenerations[*params.SessionID]++
				if err := writeCodexResult(output, message.ID, map[string]any{}); err != nil {
					return err
				}
			case "fuzzyFileSearch/sessionUpdate":
				if !state.experimentalAPI {
					_ = writeCodexError(output, message.ID, -32600, "fuzzyFileSearch/sessionUpdate requires experimentalApi capability")
					continue
				}
				var params struct {
					SessionID *string `json:"sessionId"`
					Query     *string `json:"query"`
				}
				if paramsErr := json.Unmarshal(message.Params, &params); paramsErr != nil || params.SessionID == nil || params.Query == nil {
					_ = writeCodexError(output, message.ID, -32600, "Invalid request: fuzzyFileSearch/sessionUpdate requires sessionId and query")
					continue
				}
				workspace, workspaceErr := bridge.workspace(request.WorkingDir)
				if workspaceErr != nil {
					_ = writeCodexError(output, message.ID, -32000, workspaceErr.Error())
					continue
				}
				begin, beginErr := workspace.client.BeginWorkspaceFileSearchSessionUpdate(ctx, workspace.value.ID, proto.WorkspaceFileSearchSessionBeginRequest{Owner: fuzzyOwner, SessionID: *params.SessionID, Query: *params.Query, Limit: codexFuzzyFileLimit})
				if beginErr != nil {
					messageText := beginErr.Error()
					if strings.Contains(messageText, "fuzzy file search session not found: ") {
						messageText = "fuzzy file search session not found: " + *params.SessionID
					}
					_ = writeCodexError(output, message.ID, -32600, messageText)
					continue
				}
				if err := writeCodexResult(output, message.ID, map[string]any{}); err != nil {
					return err
				}
				sessionID := *params.SessionID
				sessionGeneration := fuzzySessionGenerations[sessionID]
				activeFuzzy++
				workers.Add(1)
				go func() {
					defer workers.Done()
					accepted, query, files, searchErr := completeCodexFuzzySessionUpdate(fuzzyContext, workspace, fuzzyOwner, sessionID, begin.UpdateID)
					select {
					case fuzzyDone <- codexFuzzySearchResult{sessionID: sessionID, sessionGeneration: sessionGeneration, query: query, files: files, accepted: accepted, err: searchErr}:
					case <-workerDeliveryCtx.Done():
					}
				}()
			case "fuzzyFileSearch/sessionStop":
				if !state.experimentalAPI {
					_ = writeCodexError(output, message.ID, -32600, "fuzzyFileSearch/sessionStop requires experimentalApi capability")
					continue
				}
				var params struct {
					SessionID *string `json:"sessionId"`
				}
				if paramsErr := json.Unmarshal(message.Params, &params); paramsErr != nil || params.SessionID == nil {
					_ = writeCodexError(output, message.ID, -32600, "Invalid request: fuzzyFileSearch/sessionStop requires sessionId")
					continue
				}
				workspace, workspaceErr := bridge.workspace(request.WorkingDir)
				if workspaceErr != nil {
					_ = writeCodexError(output, message.ID, -32000, workspaceErr.Error())
					continue
				}
				if stopErr := workspace.client.StopWorkspaceFileSearchSession(ctx, workspace.value.ID, proto.WorkspaceFileSearchSessionRequest{Owner: fuzzyOwner, SessionID: *params.SessionID}); stopErr != nil {
					_ = writeCodexError(output, message.ID, -32000, stopErr.Error())
					continue
				}
				fuzzySessionGenerations[*params.SessionID]++
				if err := writeCodexResult(output, message.ID, map[string]any{}); err != nil {
					return err
				}
			case "model/list":
				var params struct {
					Cursor        *string `json:"cursor"`
					Limit         *uint32 `json:"limit"`
					IncludeHidden *bool   `json:"includeHidden"`
				}
				if len(message.Params) != 0 && string(message.Params) != "null" {
					if paramsErr := json.Unmarshal(message.Params, &params); paramsErr != nil {
						_ = writeCodexError(output, message.ID, -32600, "Invalid request: "+paramsErr.Error())
						continue
					}
				}
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
				page, nextCursor, pageErr := paginateCodexModels(models, params.Cursor, params.Limit)
				if pageErr != nil {
					_ = writeCodexError(output, message.ID, -32600, pageErr.Error())
					continue
				}
				if err := writeCodexResult(output, message.ID, map[string]any{"data": page, "nextCursor": nextCursor}); err != nil {
					return err
				}
			case "thread/list":
				result, listErr := bridge.listProjectThreads(ctx, message.Params)
				if listErr != nil {
					if err := writeCodexError(output, message.ID, -32602, listErr.Error()); err != nil {
						return err
					}
					continue
				}
				if err := writeCodexResult(output, message.ID, result); err != nil {
					return err
				}
			case "thread/settings/update":
				if err := bridge.updateThreadSettings(ctx, message.Params); err != nil {
					if writeErr := writeCodexError(output, message.ID, -32602, err.Error()); writeErr != nil {
						return writeErr
					}
					continue
				}
				if err := writeCodexResult(output, message.ID, map[string]any{}); err != nil {
					return err
				}
			case "thread/loaded/list":
				var params struct {
					Cursor *string `json:"cursor"`
					Limit  *uint32 `json:"limit"`
				}
				if len(message.Params) != 0 && string(message.Params) != "null" {
					if paramsErr := json.Unmarshal(message.Params, &params); paramsErr != nil {
						_ = writeCodexError(output, message.ID, -32600, "Invalid request: "+paramsErr.Error())
						continue
					}
				}
				page, nextCursor, pageErr := paginateCodexLoadedThreads(bridge.threads, params.Cursor, params.Limit)
				if pageErr != nil {
					_ = writeCodexError(output, message.ID, -32600, "Invalid request: "+pageErr.Error())
					continue
				}
				if err := writeCodexResult(output, message.ID, map[string]any{"data": page, "nextCursor": nextCursor}); err != nil {
					return err
				}
			case "thread/backgroundTerminals/clean", "thread/backgroundTerminals/list", "thread/backgroundTerminals/terminate":
				if !state.experimentalAPI {
					_ = writeCodexError(output, message.ID, -32600, message.Method+" requires experimentalApi capability")
					continue
				}
				var params struct {
					ThreadID  string  `json:"threadId"`
					Cursor    *string `json:"cursor"`
					Limit     *uint32 `json:"limit"`
					ProcessID string  `json:"processId"`
				}
				if paramsErr := json.Unmarshal(message.Params, &params); paramsErr != nil {
					_ = writeCodexError(output, message.ID, -32600, "Invalid request: "+paramsErr.Error())
					continue
				}
				if params.ThreadID == "" {
					_ = writeCodexError(output, message.ID, -32600, "Invalid request: threadId is required")
					continue
				}
				var processID int64
				if message.Method == "thread/backgroundTerminals/terminate" {
					var parseErr error
					processID, parseErr = strconv.ParseInt(params.ProcessID, 10, 32)
					if parseErr != nil {
						_ = writeCodexError(output, message.ID, -32600, "invalid background terminal process id: "+parseErr.Error())
						continue
					}
				}
				if _, parseErr := uuid.Parse(params.ThreadID); parseErr != nil {
					_ = writeCodexError(output, message.ID, -32600, "Invalid request: invalid thread id: "+parseErr.Error())
					continue
				}
				binding, _, findErr := bridge.findThread(ctx, params.ThreadID, "")
				if findErr != nil {
					_ = writeCodexError(output, message.ID, -32002, "Thread not found")
					continue
				}
				binding.loaded = true
				terminals, listErr := bridge.backgroundTerminals(ctx, binding)
				if listErr != nil {
					_ = writeCodexError(output, message.ID, -32000, listErr.Error())
					continue
				}
				switch message.Method {
				case "thread/backgroundTerminals/clean":
					cleanFailed := false
					for _, terminal := range terminals {
						if _, stopErr := binding.workspace.client.StopTask(ctx, binding.workspace.value.ID, terminal.taskID); stopErr != nil {
							_ = writeCodexError(output, message.ID, -32000, stopErr.Error())
							cleanFailed = true
							break
						}
					}
					if cleanFailed {
						continue
					}
					if err := writeCodexResult(output, message.ID, map[string]any{}); err != nil {
						return err
					}
				case "thread/backgroundTerminals/list":
					page, nextCursor, pageErr := paginateCodexBackgroundTerminals(terminals, params.Cursor, params.Limit)
					if pageErr != nil {
						_ = writeCodexError(output, message.ID, -32600, pageErr.Error())
						continue
					}
					if err := writeCodexResult(output, message.ID, map[string]any{"data": page, "nextCursor": nextCursor}); err != nil {
						return err
					}
				case "thread/backgroundTerminals/terminate":
					terminated := false
					terminateFailed := false
					for _, terminal := range terminals {
						if int64(terminal.processNumber) != processID || bridge.terminalTaskIDs[terminal.processNumber] != terminal.taskID {
							continue
						}
						if _, stopErr := binding.workspace.client.StopTask(ctx, binding.workspace.value.ID, terminal.taskID); stopErr != nil {
							_ = writeCodexError(output, message.ID, -32000, stopErr.Error())
							terminateFailed = true
							break
						}
						terminated = true
						break
					}
					if terminateFailed {
						continue
					}
					if err := writeCodexResult(output, message.ID, map[string]any{"terminated": terminated}); err != nil {
						return err
					}
				}
			case "thread/items/list":
				if len(message.ID) != 0 {
					if err := writeCodexError(output, message.ID, -32601, "thread/items/list is not supported yet"); err != nil {
						return err
					}
				}
			case "thread/turns/list":
				var params struct {
					ThreadID      string  `json:"threadId"`
					Cursor        *string `json:"cursor"`
					Limit         *uint32 `json:"limit"`
					SortDirection *string `json:"sortDirection"`
					ItemsView     *string `json:"itemsView"`
				}
				if paramsErr := json.Unmarshal(message.Params, &params); paramsErr != nil {
					_ = writeCodexError(output, message.ID, -32600, "Invalid request: "+paramsErr.Error())
					continue
				}
				if params.ThreadID == "" {
					_ = writeCodexError(output, message.ID, -32600, "Invalid request: threadId is required")
					continue
				}
				if _, parseErr := uuid.Parse(params.ThreadID); parseErr != nil {
					_ = writeCodexError(output, message.ID, -32600, "Invalid request: invalid thread id: "+parseErr.Error())
					continue
				}
				sortDirection := "desc"
				if params.SortDirection != nil {
					sortDirection = *params.SortDirection
				}
				if sortDirection != "asc" && sortDirection != "desc" {
					_ = writeCodexError(output, message.ID, -32600, "Invalid request: invalid sortDirection: "+sortDirection)
					continue
				}
				itemsView := "summary"
				if params.ItemsView != nil {
					itemsView = *params.ItemsView
				}
				if itemsView != "notLoaded" && itemsView != "summary" && itemsView != "full" {
					_ = writeCodexError(output, message.ID, -32600, "Invalid request: invalid itemsView: "+itemsView)
					continue
				}
				binding, sess, findErr := bridge.findThread(ctx, params.ThreadID, "")
				if findErr != nil {
					_ = writeCodexError(output, message.ID, -32600, "Invalid request: thread not loaded: "+params.ThreadID)
					continue
				}
				if binding.loaded && binding.ephemeral {
					_ = writeCodexError(output, message.ID, -32600, "Invalid request: ephemeral threads do not support thread/turns/list")
					continue
				}
				messages, messagesErr := binding.workspace.client.ListMessages(ctx, binding.workspace.value.ID, sess.ID)
				if messagesErr != nil {
					_ = writeCodexError(output, message.ID, -32000, messagesErr.Error())
					continue
				}
				if binding.loaded && len(messages) == 0 {
					_ = writeCodexError(output, message.ID, -32600, "Invalid request: thread "+params.ThreadID+" is not materialized yet; thread/turns/list is unavailable before first user message")
					continue
				}
				turns, projectionErr := codexThreadTurns(messages, binding.active || sess.IsBusy)
				if projectionErr != nil {
					_ = writeCodexError(output, message.ID, -32000, projectionErr.Error())
					continue
				}
				if viewErr := applyCodexThreadTurnsItemsView(turns, itemsView); viewErr != nil {
					_ = writeCodexError(output, message.ID, -32000, viewErr.Error())
					continue
				}
				page, nextCursor, backwardsCursor, pageErr := paginateCodexThreadTurns(turns, params.Cursor, params.Limit, sortDirection)
				if pageErr != nil {
					_ = writeCodexError(output, message.ID, -32600, pageErr.Error())
					continue
				}
				if err := writeCodexResult(output, message.ID, map[string]any{"data": page, "nextCursor": nextCursor, "backwardsCursor": backwardsCursor}); err != nil {
					return err
				}
			case "thread/read", "thread/resume":
				var params struct {
					ThreadID      string          `json:"threadId"`
					CWD           string          `json:"cwd"`
					Model         string          `json:"model"`
					ModelProvider string          `json:"modelProvider"`
					IncludeTurns  json.RawMessage `json:"includeTurns"`
					ExcludeTurns  bool            `json:"excludeTurns"`
				}
				paramsErr := json.Unmarshal(message.Params, &params)
				includeTurns := false
				if message.Method == "thread/read" {
					if paramsErr != nil {
						_ = writeCodexError(output, message.ID, -32600, "Invalid request: "+paramsErr.Error())
						continue
					}
					if params.ThreadID == "" {
						_ = writeCodexError(output, message.ID, -32600, "Invalid request: threadId is required")
						continue
					}
					if _, parseErr := uuid.Parse(params.ThreadID); parseErr != nil {
						_ = writeCodexError(output, message.ID, -32600, "Invalid request: invalid thread id: "+parseErr.Error())
						continue
					}
					if len(params.IncludeTurns) != 0 {
						if string(params.IncludeTurns) == "null" || json.Unmarshal(params.IncludeTurns, &includeTurns) != nil {
							_ = writeCodexError(output, message.ID, -32600, "Invalid request: includeTurns must be a boolean")
							continue
						}
					}
				} else if paramsErr != nil || params.ThreadID == "" {
					_ = writeCodexError(output, message.ID, -32602, "Invalid "+message.Method+" params")
					continue
				}
				binding, sess, findErr := bridge.findThread(ctx, params.ThreadID, params.CWD)
				if findErr != nil {
					if message.Method == "thread/read" {
						_ = writeCodexError(output, message.ID, -32600, "Invalid request: thread not loaded: "+params.ThreadID)
					} else {
						_ = writeCodexError(output, message.ID, -32002, "Thread not found")
					}
					continue
				}
				if message.Method == "thread/read" && includeTurns && binding.loaded && binding.ephemeral {
					_ = writeCodexError(output, message.ID, -32600, "Invalid request: ephemeral threads do not support includeTurns")
					continue
				}
				if lineageErr := bridge.hydrateThreadLineage(ctx, binding, sess); lineageErr != nil {
					_ = writeCodexError(output, message.ID, -32000, lineageErr.Error())
					continue
				}
				selection := binding.model
				wasLoaded := binding.loaded
				rollbackResume := func(cause error) error { return cause }
				if message.Method == "thread/resume" {
					previousBindingModel := binding.model
					previousWorkspaceModel := bridge.selectedModel[binding.workspace]
					previousSession := *sess
					persistedSelection := false
					var selectErr error
					selection, selectErr = bridge.selectPublicThreadModel(ctx, binding, params.Model, params.ModelProvider)
					if selectErr != nil {
						_ = writeCodexError(output, message.ID, -32602, selectErr.Error())
						continue
					}
					rollbackResume = func(cause error) error {
						cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), codexServerReadyTimeout)
						defer cancelCleanup()
						var persistErr error
						if persistedSelection {
							_, persistErr = binding.workspace.client.SaveSession(cleanupCtx, binding.workspace.value.ID, previousSession)
						}
						binding.model = previousBindingModel
						binding.loaded = wasLoaded
						return errors.Join(cause, persistErr, bridge.restoreModelSelection(cleanupCtx, binding.workspace, previousWorkspaceModel))
					}
					if params.Model != "" || params.ModelProvider != "" {
						saved, saveErr := bridge.saveSessionModel(ctx, binding.workspace, sess, selection)
						if saveErr != nil {
							_ = writeCodexError(output, message.ID, -32000, rollbackResume(saveErr).Error())
							continue
						}
						sess = saved
						persistedSelection = true
					}
					binding.loaded = true
				} else if selection.Model == "" || selection.Provider == "" {
					cfg, configErr := binding.workspace.client.GetConfig(ctx, binding.workspace.value.ID)
					if configErr != nil {
						_ = writeCodexError(output, message.ID, -32000, configErr.Error())
						continue
					}
					selection = cfg.Models[config.SelectedModelTypeLarge]
					binding.model = selection
				}
				thread := codexThread(binding, sess, selection)
				hydrateTurns := message.Method == "thread/read" && includeTurns || message.Method == "thread/resume" && !params.ExcludeTurns
				if hydrateTurns {
					messages, messagesErr := binding.workspace.client.ListMessages(ctx, binding.workspace.value.ID, sess.ID)
					if messagesErr != nil {
						_ = writeCodexError(output, message.ID, -32000, rollbackResume(messagesErr).Error())
						continue
					}
					if message.Method == "thread/read" && binding.loaded && len(messages) == 0 {
						_ = writeCodexError(output, message.ID, -32600, "Invalid request: includeTurns is unavailable before first user message")
						continue
					}
					turns, projectionErr := codexThreadTurns(messages, sess.IsBusy)
					if projectionErr != nil {
						_ = writeCodexError(output, message.ID, -32000, rollbackResume(projectionErr).Error())
						continue
					}
					thread["turns"] = turns
				}
				result := map[string]any{"thread": thread}
				if message.Method == "thread/resume" {
					result = codexThreadStartResult(thread, selection, binding.policy)
				}
				if err := writeCodexResult(output, message.ID, result); err != nil {
					return rollbackResume(err)
				}
				if message.Method == "thread/resume" {
					bridge.subscribeThread(binding)
				}
				if message.Method == "thread/resume" && !wasLoaded {
					if err := state.writeThreadNotification(output, binding, "thread/status/changed", map[string]any{"threadId": binding.codexID, "status": codexThreadStatus(binding, sess.IsBusy)}); err != nil {
						return err
					}
				}
			case "thread/start":
				var params struct {
					ProjectID         string          `json:"projectId"`
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
				cwd, cwdErr := bridge.resolveProjectCWD(ctx, params.CWD, params.ProjectID)
				if cwdErr != nil {
					_ = writeCodexError(output, message.ID, -32602, cwdErr.Error())
					continue
				}
				workspace, wsErr := bridge.workspace(cwd)
				if wsErr != nil {
					_ = writeCodexError(output, message.ID, -32602, wsErr.Error())
					continue
				}
				projectID, projectErr := bridge.projectForDirectory(ctx, workspace.value.Path)
				if projectErr != nil {
					_ = writeCodexError(output, message.ID, -32000, projectErr.Error())
					continue
				}
				model := params.Model
				if model == "" {
					model = request.Model
				}
				if bridge.models[workspace] == nil {
					if _, listErr := bridge.listModels(ctx, workspace); listErr != nil {
						_ = writeCodexError(output, message.ID, -32602, listErr.Error())
						continue
					}
				}
				previousSelection := bridge.selectedModel[workspace]
				selection, selectErr := bridge.selectPublicModel(ctx, workspace, model, params.ModelProvider)
				if selectErr != nil {
					_ = writeCodexError(output, message.ID, -32602, selectErr.Error())
					continue
				}
				restoreSelection := func(cause error) error {
					cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), codexServerReadyTimeout)
					defer cancelCleanup()
					return errors.Join(cause, bridge.restoreModelSelection(cleanupCtx, workspace, previousSelection))
				}
				sess, createErr := workspace.client.CreateSession(ctx, workspace.value.ID, "Codex")
				if createErr != nil {
					_ = writeCodexError(output, message.ID, -32000, restoreSelection(createErr).Error())
					continue
				}
				saved, saveErr := bridge.saveSessionModel(ctx, workspace, sess, selection)
				if saveErr != nil {
					cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), codexServerReadyTimeout)
					deleteErr := workspace.client.DeleteSession(cleanupCtx, workspace.value.ID, sess.ID)
					cancelCleanup()
					_ = writeCodexError(output, message.ID, -32000, restoreSelection(errors.Join(saveErr, deleteErr)).Error())
					continue
				}
				sess = saved
				binding := bridge.bindThread(workspace, sess)
				binding.projectID = projectID
				binding.loaded = true
				bridge.subscribeThread(binding)
				binding.policy = policy
				ephemeral := !request.Session.Persistent
				if params.Ephemeral != nil {
					ephemeral = ephemeral || *params.Ephemeral
				}
				binding.ephemeral = true
				thread := codexThread(binding, sess, selection)
				thread["ephemeral"] = ephemeral
				if err := writeCodexResult(output, message.ID, codexThreadStartResult(thread, selection, policy)); err != nil {
					cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), codexServerReadyTimeout)
					cleanupErr := bridge.deleteEphemeralThread(cleanupCtx, binding)
					cancelCleanup()
					return restoreSelection(errors.Join(err, cleanupErr))
				}
				binding.ephemeral = ephemeral
				if err := state.writeThreadNotification(output, binding, "thread/started", map[string]any{"thread": thread}); err != nil {
					return err
				}
			case "thread/fork":
				var params struct {
					ThreadID string `json:"threadId"`
					CWD      string `json:"cwd"`
				}
				if paramsErr := json.Unmarshal(message.Params, &params); paramsErr != nil || params.ThreadID == "" {
					_ = writeCodexError(output, message.ID, -32602, "Invalid thread/fork params")
					continue
				}
				binding, _, findErr := bridge.findThread(ctx, params.ThreadID, params.CWD)
				if findErr != nil {
					_ = writeCodexError(output, message.ID, -32002, "Thread not found")
					continue
				}
				forked, forkErr := binding.workspace.client.ForkSessionWithOrigin(ctx, binding.workspace.value.ID, binding.nativeID)
				if forkErr != nil {
					_ = writeCodexError(output, message.ID, -32000, forkErr.Error())
					continue
				}
				forkBinding := bridge.bindThread(binding.workspace, forked)
				forkBinding.ephemeral = true
				forkBinding.model = binding.model
				deleteFork := func(cause error) error {
					cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), codexServerReadyTimeout)
					defer cancelCleanup()
					if cleanupErr := bridge.deleteEphemeralThread(cleanupCtx, forkBinding); cleanupErr != nil {
						return errors.Join(cause, fmt.Errorf("delete failed fork: %w", cleanupErr))
					}
					return cause
				}
				if bridge.models[binding.workspace] == nil {
					if _, listErr := bridge.listModels(ctx, binding.workspace); listErr != nil {
						_ = writeCodexError(output, message.ID, -32000, deleteFork(listErr).Error())
						continue
					}
				}
				previousSelection := bridge.selectedModel[binding.workspace]
				selection, selectErr := bridge.selectThreadModel(ctx, forkBinding, "", "")
				if selectErr != nil {
					_ = writeCodexError(output, message.ID, -32000, deleteFork(selectErr).Error())
					continue
				}
				cleanupFork := func(cause error) error {
					cleanupErr := deleteFork(cause)
					cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), codexServerReadyTimeout)
					defer cancelCleanup()
					return errors.Join(cleanupErr, bridge.restoreModelSelection(cleanupCtx, binding.workspace, previousSelection))
				}
				if forked.Model != selection.Model || forked.Provider != selection.Provider {
					saved, saveErr := bridge.saveSessionModel(ctx, binding.workspace, forked, selection)
					if saveErr != nil {
						_ = writeCodexError(output, message.ID, -32000, cleanupFork(saveErr).Error())
						continue
					}
					forked = saved
				}
				forkBinding.loaded = true
				bridge.subscribeThread(forkBinding)
				if lineageErr := bridge.hydrateThreadLineage(ctx, forkBinding, forked); lineageErr != nil {
					_ = writeCodexError(output, message.ID, -32000, cleanupFork(lineageErr).Error())
					continue
				}
				forkBinding.policy = binding.policy
				thread := codexThread(forkBinding, forked, selection)
				messages, messagesErr := forkBinding.workspace.client.ListMessages(ctx, forkBinding.workspace.value.ID, forked.ID)
				if messagesErr != nil {
					forkBinding.ephemeral = true
					_ = writeCodexError(output, message.ID, -32000, cleanupFork(messagesErr).Error())
					continue
				}
				turns, projectionErr := codexThreadTurns(messages, forked.IsBusy)
				if projectionErr != nil {
					forkBinding.ephemeral = true
					_ = writeCodexError(output, message.ID, -32000, cleanupFork(projectionErr).Error())
					continue
				}
				thread["turns"] = turns
				if err := writeCodexResult(output, message.ID, codexThreadStartResult(thread, selection, forkBinding.policy)); err != nil {
					return cleanupFork(err)
				}
				forkBinding.ephemeral = binding.ephemeral
				notificationThread := make(map[string]any, len(thread))
				for key, value := range thread {
					notificationThread[key] = value
				}
				notificationThread["turns"] = []any{}
				if err := state.writeThreadNotification(output, forkBinding, "thread/started", map[string]any{"thread": notificationThread}); err != nil {
					return err
				}
			case "thread/name/set":
				var params struct {
					ThreadID string `json:"threadId"`
					Name     string `json:"name"`
				}
				if paramsErr := json.Unmarshal(message.Params, &params); paramsErr != nil {
					_ = writeCodexError(output, message.ID, -32600, "Invalid request: "+paramsErr.Error())
					continue
				}
				if params.ThreadID == "" {
					_ = writeCodexError(output, message.ID, -32600, "Invalid request: threadId is required")
					continue
				}
				if _, parseErr := uuid.Parse(params.ThreadID); parseErr != nil {
					_ = writeCodexError(output, message.ID, -32600, "Invalid request: invalid thread id: "+parseErr.Error())
					continue
				}
				name := strings.TrimSpace(params.Name)
				if name == "" {
					_ = writeCodexError(output, message.ID, -32600, "thread name must not be empty")
					continue
				}
				binding, _, findErr := bridge.findThread(ctx, params.ThreadID, "")
				if findErr != nil {
					_ = writeCodexError(output, message.ID, -32002, "Thread not found")
					continue
				}
				if _, renameErr := binding.workspace.client.RenameSession(ctx, binding.workspace.value.ID, binding.nativeID, name); renameErr != nil {
					_ = writeCodexError(output, message.ID, -32000, renameErr.Error())
					continue
				}
				if err := writeCodexResult(output, message.ID, map[string]any{}); err != nil {
					return err
				}
				if err := state.writeNotification(output, "thread/name/updated", map[string]any{"threadId": binding.codexID, "threadName": name}); err != nil {
					return err
				}
			case "thread/archive":
				var params struct {
					ThreadID string `json:"threadId"`
				}
				if paramsErr := json.Unmarshal(message.Params, &params); paramsErr != nil {
					_ = writeCodexError(output, message.ID, -32600, "Invalid request: "+paramsErr.Error())
					continue
				}
				if params.ThreadID == "" {
					_ = writeCodexError(output, message.ID, -32600, "Invalid request: threadId is required")
					continue
				}
				if _, parseErr := uuid.Parse(params.ThreadID); parseErr != nil {
					_ = writeCodexError(output, message.ID, -32600, "Invalid request: invalid session id: "+parseErr.Error())
					continue
				}
				binding, sess, findErr := bridge.findThread(ctx, params.ThreadID, "")
				if findErr != nil {
					_ = writeCodexError(output, message.ID, -32002, "Thread not found")
					continue
				}
				if binding.ephemeral {
					_ = writeCodexError(output, message.ID, -32600, "Invalid request: ephemeral threads do not support thread/archive")
					continue
				}
				messages, messagesErr := binding.workspace.client.ListMessages(ctx, binding.workspace.value.ID, sess.ID)
				if messagesErr != nil {
					_ = writeCodexError(output, message.ID, -32000, messagesErr.Error())
					continue
				}
				if len(messages) == 0 {
					_ = writeCodexError(output, message.ID, -32600, "Invalid request: thread "+params.ThreadID+" is not materialized yet")
					continue
				}
				archived, archiveErr := binding.workspace.client.ArchiveSession(ctx, binding.workspace.value.ID, binding.nativeID)
				if archiveErr != nil {
					errorMessage := archiveErr.Error()
					const activeWriterSuffix = " already has an active writer"
					if start := strings.Index(errorMessage, "thread "); start >= 0 {
						activeWriterMessage := strings.TrimSpace(errorMessage[start:])
						if strings.HasSuffix(activeWriterMessage, activeWriterSuffix) {
							nativeThreadID := strings.TrimSuffix(strings.TrimPrefix(activeWriterMessage, "thread "), activeWriterSuffix)
							codexThreadID := params.ThreadID
							if nativeThreadID != binding.nativeID {
								activeSession, getErr := binding.workspace.client.GetSession(ctx, binding.workspace.value.ID, nativeThreadID)
								if getErr != nil {
									_ = writeCodexError(output, message.ID, -32000, getErr.Error())
									continue
								}
								codexThreadID = codexUUIDv7(binding.workspace.value.Path, activeSession)
							}
							_ = writeCodexError(output, message.ID, -32600, "thread "+codexThreadID+activeWriterSuffix)
							continue
						}
					}
					_ = writeCodexError(output, message.ID, -32000, errorMessage)
					continue
				}
				archivedIDs := make([]string, 0, len(archived))
				for index := range archived {
					threadID := codexUUIDv7(binding.workspace.value.Path, &archived[index])
					if archivedBinding := bridge.threads[threadID]; archivedBinding != nil {
						bridge.cancelThreadUnload(archivedBinding)
						delete(bridge.threads, threadID)
					}
					archivedIDs = append(archivedIDs, threadID)
				}
				if err := writeCodexResult(output, message.ID, map[string]any{}); err != nil {
					return err
				}
				for _, threadID := range archivedIDs {
					if err := state.writeNotification(output, "thread/archived", map[string]any{"threadId": threadID}); err != nil {
						return err
					}
				}
			case "thread/unarchive":
				var params struct {
					ThreadID string `json:"threadId"`
				}
				if paramsErr := json.Unmarshal(message.Params, &params); paramsErr != nil {
					_ = writeCodexError(output, message.ID, -32600, "Invalid request: "+paramsErr.Error())
					continue
				}
				if params.ThreadID == "" {
					_ = writeCodexError(output, message.ID, -32600, "Invalid request: threadId is required")
					continue
				}
				if _, parseErr := uuid.Parse(params.ThreadID); parseErr != nil {
					_ = writeCodexError(output, message.ID, -32600, "Invalid request: invalid session id: "+parseErr.Error())
					continue
				}
				binding, sess, findErr := bridge.findStoredThread(ctx, params.ThreadID)
				if findErr != nil || !sess.Archived {
					_ = writeCodexError(output, message.ID, -32600, "no archived rollout found for thread id "+params.ThreadID)
					continue
				}
				if lineageErr := bridge.hydrateThreadLineage(ctx, binding, sess); lineageErr != nil {
					_ = writeCodexError(output, message.ID, -32603, "failed to unarchive session: "+lineageErr.Error())
					continue
				}
				unarchived, unarchiveErr := binding.workspace.client.UnarchiveSession(ctx, binding.workspace.value.ID, binding.nativeID)
				if unarchiveErr != nil {
					errorMessage := unarchiveErr.Error()
					const activeWriterSuffix = " already has an active writer"
					if strings.Contains(errorMessage, activeWriterSuffix) {
						_ = writeCodexError(output, message.ID, -32600, "thread "+params.ThreadID+activeWriterSuffix)
						continue
					}
					if strings.Contains(errorMessage, "session is not archived") {
						_ = writeCodexError(output, message.ID, -32600, "no archived rollout found for thread id "+params.ThreadID)
						continue
					}
					_ = writeCodexError(output, message.ID, -32603, errorMessage)
					continue
				}
				thread := codexThread(binding, unarchived, binding.model)
				if err := writeCodexResult(output, message.ID, map[string]any{"thread": thread}); err != nil {
					return err
				}
				if err := state.writeNotification(output, "thread/unarchived", map[string]any{"threadId": binding.codexID}); err != nil {
					return err
				}
			case "thread/unsubscribe":
				var params struct {
					ThreadID string `json:"threadId"`
				}
				if paramsErr := json.Unmarshal(message.Params, &params); paramsErr != nil {
					_ = writeCodexError(output, message.ID, -32600, "Invalid request: "+paramsErr.Error())
					continue
				}
				if params.ThreadID == "" {
					_ = writeCodexError(output, message.ID, -32600, "Invalid request: threadId is required")
					continue
				}
				if _, parseErr := uuid.Parse(params.ThreadID); parseErr != nil {
					_ = writeCodexError(output, message.ID, -32600, "invalid thread id: "+parseErr.Error())
					continue
				}
				binding := bridge.threads[params.ThreadID]
				status := "notLoaded"
				if binding != nil && binding.loaded {
					status = "notSubscribed"
					if binding.subscribed.Swap(false) {
						status = "unsubscribed"
						bridge.scheduleThreadUnload(binding)
					}
				}
				if err := writeCodexResult(output, message.ID, map[string]any{"status": status}); err != nil {
					return err
				}
			case "thread/delete":
				var params struct {
					ThreadID string `json:"threadId"`
				}
				if paramsErr := json.Unmarshal(message.Params, &params); paramsErr != nil {
					_ = writeCodexError(output, message.ID, -32600, "Invalid request: "+paramsErr.Error())
					continue
				}
				if params.ThreadID == "" {
					_ = writeCodexError(output, message.ID, -32600, "Invalid request: threadId is required")
					continue
				}
				if _, parseErr := uuid.Parse(params.ThreadID); parseErr != nil {
					_ = writeCodexError(output, message.ID, -32600, "Invalid request: invalid thread id: "+parseErr.Error())
					continue
				}
				binding, _, findErr := bridge.findStoredThread(ctx, params.ThreadID)
				if findErr != nil {
					_ = writeCodexError(output, message.ID, -32600, "thread not found: "+params.ThreadID)
					continue
				}
				if binding.ephemeral {
					_ = writeCodexError(output, message.ID, -32600, "thread is not persisted and cannot be deleted: "+params.ThreadID)
					continue
				}
				deleted, deleteErr := binding.workspace.client.DeleteSessionSubtree(ctx, binding.workspace.value.ID, binding.nativeID)
				if deleteErr != nil {
					errorMessage := deleteErr.Error()
					const activeWriterSuffix = " already has an active writer"
					if start := strings.Index(errorMessage, "thread "); start >= 0 {
						activeWriterMessage := strings.TrimSpace(errorMessage[start:])
						if strings.HasSuffix(activeWriterMessage, activeWriterSuffix) {
							nativeThreadID := strings.TrimSuffix(strings.TrimPrefix(activeWriterMessage, "thread "), activeWriterSuffix)
							codexThreadID := params.ThreadID
							if nativeThreadID != binding.nativeID {
								activeSession, getErr := binding.workspace.client.GetSession(ctx, binding.workspace.value.ID, nativeThreadID)
								if getErr != nil {
									_ = writeCodexError(output, message.ID, -32000, getErr.Error())
									continue
								}
								codexThreadID = codexUUIDv7(binding.workspace.value.Path, activeSession)
							}
							_ = writeCodexError(output, message.ID, -32600, "thread "+codexThreadID+activeWriterSuffix)
							continue
						}
					}
					_ = writeCodexError(output, message.ID, -32000, errorMessage)
					continue
				}
				deletedIDs := make([]string, 0, len(deleted))
				for index := range deleted {
					threadID := codexUUIDv7(binding.workspace.value.Path, &deleted[index])
					if deletedBinding := bridge.threads[threadID]; deletedBinding != nil {
						bridge.cancelThreadUnload(deletedBinding)
						delete(bridge.threads, threadID)
					}
					deletedIDs = append(deletedIDs, threadID)
				}
				if err := writeCodexResult(output, message.ID, map[string]any{}); err != nil {
					return err
				}
				for _, threadID := range deletedIDs {
					if err := state.writeNotification(output, "thread/deleted", map[string]any{"threadId": threadID}); err != nil {
						return err
					}
				}
			case "thread/rollback":
				if state.clientName != "codex-tui" {
					if err := output.write(map[string]any{"method": "deprecationNotice", "params": map[string]any{"summary": "thread/rollback is deprecated and will be removed soon"}}); err != nil {
						return err
					}
				}
				var params struct {
					ThreadID string  `json:"threadId"`
					NumTurns *uint32 `json:"numTurns"`
				}
				if paramsErr := json.Unmarshal(message.Params, &params); paramsErr != nil {
					_ = writeCodexError(output, message.ID, -32600, "Invalid request: "+paramsErr.Error())
					continue
				}
				if params.ThreadID == "" {
					_ = writeCodexError(output, message.ID, -32600, "Invalid request: threadId is required")
					continue
				}
				if params.NumTurns == nil {
					_ = writeCodexError(output, message.ID, -32600, "Invalid request: numTurns is required")
					continue
				}
				if *params.NumTurns == 0 {
					_ = writeCodexError(output, message.ID, -32600, "numTurns must be >= 1")
					continue
				}
				if _, parseErr := uuid.Parse(params.ThreadID); parseErr != nil {
					_ = writeCodexError(output, message.ID, -32600, "Invalid request: invalid thread id: "+parseErr.Error())
					continue
				}
				binding, sess, findErr := bridge.findThread(ctx, params.ThreadID, "")
				if findErr != nil {
					_ = writeCodexError(output, message.ID, -32002, "Thread not found")
					continue
				}
				if binding.ephemeral {
					_ = writeCodexError(output, message.ID, -32600, "thread rollback requires persisted thread history")
					continue
				}
				if sess.IsBusy || activeTurns[binding.codexID] != nil {
					_ = writeCodexError(output, message.ID, -32600, "Cannot rollback while a turn is in progress.")
					continue
				}
				if lineageErr := bridge.hydrateThreadLineage(ctx, binding, sess); lineageErr != nil {
					_ = writeCodexError(output, message.ID, -32000, lineageErr.Error())
					continue
				}
				selection := binding.model
				if selection.Model == "" || selection.Provider == "" {
					cfg, configErr := binding.workspace.client.GetConfig(ctx, binding.workspace.value.ID)
					if configErr != nil {
						_ = writeCodexError(output, message.ID, -32000, configErr.Error())
						continue
					}
					selection = cfg.Models[config.SelectedModelTypeLarge]
					binding.model = selection
				}
				messages, messagesErr := binding.workspace.client.ListMessages(ctx, binding.workspace.value.ID, binding.nativeID)
				if messagesErr != nil {
					_ = writeCodexError(output, message.ID, -32000, messagesErr.Error())
					continue
				}
				if len(messages) == 0 {
					_ = writeCodexError(output, message.ID, -32600, "thread rollback requires persisted thread history")
					continue
				}
				userMessageIDs := make([]string, 0)
				for _, storedMessage := range messages {
					if codexEffectiveUserMessage(storedMessage) {
						userMessageIDs = append(userMessageIDs, storedMessage.ID)
					}
				}
				committedSession := sess
				committedMessages := messages
				if len(userMessageIDs) != 0 {
					cutoff := 0
					if uint64(*params.NumTurns) < uint64(len(userMessageIDs)) {
						cutoff = len(userMessageIDs) - int(*params.NumTurns)
					}
					rollbackResult, rollbackErr := binding.workspace.client.SessionRollback(ctx, binding.workspace.value.ID, binding.nativeID, userMessageIDs[cutoff])
					if rollbackErr != nil {
						errorMessage := rollbackErr.Error()
						switch {
						case strings.Contains(errorMessage, agent.ErrSessionMutationInProgress.Error()):
							_ = writeCodexError(output, message.ID, -32600, "rollback already in progress for this thread")
						case strings.Contains(errorMessage, agent.ErrSessionBusy.Error()):
							_ = writeCodexError(output, message.ID, -32600, "Cannot rollback while a turn is in progress.")
						default:
							_ = writeCodexError(output, message.ID, -32000, errorMessage)
						}
						continue
					}
					committedSession = &rollbackResult.Session
					committedMessages = rollbackResult.Messages
				}
				turns, projectionErr := codexThreadTurns(committedMessages, false)
				if projectionErr != nil {
					_ = writeCodexError(output, message.ID, -32000, projectionErr.Error())
					continue
				}
				thread := codexThread(binding, committedSession, selection)
				thread["turns"] = turns
				if err := writeCodexResult(output, message.ID, map[string]any{"thread": thread}); err != nil {
					return err
				}
			case "thread/compact/start":
				var params struct {
					ThreadID string `json:"threadId"`
				}
				if json.Unmarshal(message.Params, &params) != nil || params.ThreadID == "" {
					_ = writeCodexError(output, message.ID, -32600, "Invalid thread/compact/start params")
					continue
				}
				if _, parseErr := uuid.Parse(params.ThreadID); parseErr != nil {
					_ = writeCodexError(output, message.ID, -32600, "invalid session id: "+parseErr.Error())
					continue
				}
				binding := bridge.threads[params.ThreadID]
				if binding == nil || !binding.loaded {
					_ = writeCodexError(output, message.ID, -32002, "Thread not found")
					continue
				}
				if activeTurns[binding.codexID] != nil {
					_ = writeCodexError(output, message.ID, -32600, "A turn is already running")
					continue
				}
				if _, running := runningShells[binding.codexID]; running {
					_ = writeCodexError(output, message.ID, -32600, "A turn is already running")
					continue
				}
				turnID, itemID := uuid.Must(uuid.NewV7()).String(), uuid.Must(uuid.NewV7()).String()
				turn := map[string]any{"id": turnID, "status": "inProgress", "items": []any{}, "error": nil}
				turnCtx, cancel := context.WithCancel(serverCtx)
				binding.active = true
				bridge.cancelThreadUnload(binding)
				binding.systemError = false
				activeTurns[binding.codexID] = &activeTurn{threadID: binding.codexID, turnID: turnID, binding: binding, permissionMode: binding.policy.permissionMode, ctx: turnCtx, cancel: cancel}
				if err := writeCodexResult(output, message.ID, map[string]any{}); err != nil {
					return err
				}
				if err := state.writeThreadNotification(output, binding, "turn/started", map[string]any{"threadId": binding.codexID, "turn": turn}); err != nil {
					return err
				}
				item := map[string]any{"id": itemID, "type": "contextCompaction"}
				if err := state.writeThreadNotification(output, binding, "item/started", map[string]any{"threadId": binding.codexID, "turnId": turnID, "item": item}); err != nil {
					return err
				}
				if err := state.writeThreadNotification(output, binding, "thread/status/changed", map[string]any{"threadId": binding.codexID, "status": codexThreadStatus(binding, false)}); err != nil {
					return err
				}
				workers.Add(1)
				go func() {
					defer workers.Done()
					result := bridge.runCompaction(turnCtx, binding, turnID, itemID)
					select {
					case turnDone <- result:
					case <-workerDeliveryCtx.Done():
					}
				}()
			case "turn/start":
				var params struct {
					ThreadID            string           `json:"threadId"`
					ClientUserMessageID *string          `json:"clientUserMessageId"`
					Model               string           `json:"model"`
					ApprovalPolicy      string           `json:"approvalPolicy"`
					ApprovalsReviewer   string           `json:"approvalsReviewer"`
					SandboxPolicy       json.RawMessage  `json:"sandboxPolicy"`
					Input               []codexTurnInput `json:"input"`
				}
				if json.Unmarshal(message.Params, &params) != nil {
					_ = writeCodexError(output, message.ID, -32602, "Invalid turn/start params")
					continue
				}
				if inputErr := validateCodexTurnInputPresence(message.Params); inputErr != nil {
					_ = writeCodexError(output, message.ID, -32602, inputErr.Error())
					continue
				}
				effortPresent, effort, effortErr := decodeOptionalNullableString(message.Params, "effort")
				if effortErr != nil {
					_ = writeCodexError(output, message.ID, -32602, effortErr.Error())
					continue
				}
				if effortPresent && effort != "" {
					if validationErr := session.ValidateAnalysisEffort(effort); validationErr != nil {
						_ = writeCodexError(output, message.ID, -32602, validationErr.Error())
						continue
					}
				}
				binding, nativeSession, findErr := bridge.findThread(ctx, params.ThreadID, "")
				if findErr != nil {
					_ = writeCodexError(output, message.ID, -32002, "Thread not found")
					continue
				}
				if activeTurns[binding.codexID] != nil {
					_ = writeCodexError(output, message.ID, -32600, "A turn is already running")
					continue
				}
				if _, running := runningShells[binding.codexID]; running {
					_ = writeCodexError(output, message.ID, -32600, "A turn is already running")
					continue
				}
				turnPolicy, policyErr := parseCodexTurnSandboxPolicy(params.SandboxPolicy, binding.policy)
				if policyErr == nil {
					turnPolicy, policyErr = parseCodexExecutionPolicy(params.ApprovalPolicy, params.ApprovalsReviewer, nil, turnPolicy)
				}
				if policyErr != nil {
					_ = writeCodexError(output, message.ID, -32602, policyErr.Error())
					continue
				}
				inputParts, inputErr := codexNativeTurnInput(params.Input)
				if inputErr != nil {
					_ = writeCodexError(output, message.ID, -32602, inputErr.Error())
					continue
				}
				userContent, contentErr := codexUserMessageContent(params.Input)
				if contentErr != nil {
					_ = writeCodexError(output, message.ID, -32602, contentErr.Error())
					continue
				}
				previousThreadModel := binding.model
				previousWorkspaceModel := bridge.selectedModel[binding.workspace]
				if _, selectErr := bridge.selectThreadModel(ctx, binding, params.Model, ""); selectErr != nil {
					_ = writeCodexError(output, message.ID, -32602, selectErr.Error())
					continue
				}
				previousEffort := nativeSession.AnalysisEffort
				if effortPresent {
					if _, setErr := binding.workspace.client.SetSessionAnalysisEffort(ctx, binding.workspace.value.ID, binding.nativeID, effort); setErr != nil {
						if bridge.selectedModel[binding.workspace].Provider != previousWorkspaceModel.Provider || bridge.selectedModel[binding.workspace].Model != previousWorkspaceModel.Model {
							if _, restoreErr := bridge.selectModel(ctx, binding.workspace, previousWorkspaceModel.Model, previousWorkspaceModel.Provider); restoreErr != nil {
								setErr = fmt.Errorf("%w; restoring rejected turn model: %v", setErr, restoreErr)
							}
						}
						binding.model = previousThreadModel
						_ = writeCodexError(output, message.ID, -32603, setErr.Error())
						continue
					}
				}
				turnID, itemID, runID := uuid.Must(uuid.NewV7()).String(), uuid.Must(uuid.NewV7()).String(), uuid.NewString()
				turn := map[string]any{"id": turnID, "status": "inProgress", "items": []any{}, "error": nil}
				var clientID any
				if params.ClientUserMessageID != nil {
					clientID = *params.ClientUserMessageID
				}
				userItem := map[string]any{"id": turnID, "type": "userMessage", "clientId": clientID, "content": userContent}
				turnCtx, cancel := context.WithCancel(serverCtx)
				if params.ClientUserMessageID != nil {
					turnCtx = agent.WithSubmissionID(turnCtx, *params.ClientUserMessageID)
				}
				if effort != "" {
					turnCtx = agent.WithAnalysisEffort(turnCtx, effort)
				}
				admitted := make(chan error, 1)
				started := make(chan struct{})
				workers.Add(1)
				go func() {
					defer workers.Done()
					result := bridge.runTurn(turnCtx, binding, turnID, itemID, runID, inputParts, turnPolicy.permissionMode, output, state, admitted, started)
					select {
					case turnDone <- result:
					case <-workerDeliveryCtx.Done():
					}
				}()
				if admissionErr := <-admitted; admissionErr != nil {
					cancel()
					var restoreErrs []error
					if effortPresent {
						if _, restoreErr := binding.workspace.client.SetSessionAnalysisEffort(ctx, binding.workspace.value.ID, binding.nativeID, previousEffort); restoreErr != nil {
							restoreErrs = append(restoreErrs, fmt.Errorf("restore analysis effort: %w", restoreErr))
						}
					}
					if bridge.selectedModel[binding.workspace].Provider != previousWorkspaceModel.Provider || bridge.selectedModel[binding.workspace].Model != previousWorkspaceModel.Model {
						if _, restoreErr := bridge.selectModel(ctx, binding.workspace, previousWorkspaceModel.Model, previousWorkspaceModel.Provider); restoreErr != nil {
							restoreErrs = append(restoreErrs, fmt.Errorf("restore model selection: %w", restoreErr))
						}
					}
					binding.model = previousThreadModel
					if len(restoreErrs) > 0 {
						admissionErr = fmt.Errorf("%w; restoring rejected turn overrides: %v", admissionErr, errors.Join(restoreErrs...))
					}
					_ = writeCodexError(output, message.ID, -32000, admissionErr.Error())
					continue
				}
				binding.policy = turnPolicy
				binding.loaded = true
				binding.active = true
				bridge.cancelThreadUnload(binding)
				binding.systemError = false
				activeTurns[binding.codexID] = &activeTurn{threadID: binding.codexID, turnID: turnID, runID: runID, assistantItemID: itemID, binding: binding, permissionMode: turnPolicy.permissionMode, ctx: turnCtx, cancel: cancel, items: []any{userItem}}
				if err := writeCodexResult(output, message.ID, map[string]any{"turn": turn}); err != nil {
					cancel()
					return err
				}
				if err := state.writeThreadNotification(output, binding, "turn/started", map[string]any{"threadId": binding.codexID, "turn": turn}); err != nil {
					cancel()
					return err
				}
				if err := state.writeThreadNotification(output, binding, "item/started", map[string]any{"threadId": binding.codexID, "turnId": turnID, "item": userItem}); err != nil {
					cancel()
					return err
				}
				item := map[string]any{"id": itemID, "type": "agentMessage", "status": "inProgress", "text": ""}
				if err := state.writeThreadNotification(output, binding, "item/started", map[string]any{"threadId": binding.codexID, "turnId": turnID, "item": item}); err != nil {
					cancel()
					return err
				}
				if err := state.writeThreadNotification(output, binding, "thread/status/changed", map[string]any{"threadId": binding.codexID, "status": codexThreadStatus(binding, false)}); err != nil {
					cancel()
					return err
				}
				close(started)
			case "thread/shellCommand":
				var params struct {
					ThreadID  string `json:"threadId"`
					Command   string `json:"command"`
					TimeoutMS *int64 `json:"timeoutMs"`
				}
				if json.Unmarshal(message.Params, &params) != nil || params.ThreadID == "" {
					_ = writeCodexError(output, message.ID, -32602, "Invalid thread/shellCommand params")
					continue
				}
				command := strings.TrimSpace(params.Command)
				if command == "" {
					_ = writeCodexError(output, message.ID, -32602, "command must not be empty")
					continue
				}
				if params.TimeoutMS != nil && *params.TimeoutMS < 0 {
					_ = writeCodexError(output, message.ID, -32602, fmt.Sprintf("thread/shellCommand timeoutMs must be non-negative, got %d", *params.TimeoutMS))
					continue
				}
				if _, parseErr := uuid.Parse(params.ThreadID); parseErr != nil {
					_ = writeCodexError(output, message.ID, -32602, "invalid thread id: "+parseErr.Error())
					continue
				}
				binding, _, findErr := bridge.findThread(ctx, params.ThreadID, "")
				if findErr != nil {
					_ = writeCodexError(output, message.ID, -32002, "Thread not found")
					continue
				}
				if _, running := runningShells[binding.codexID]; running {
					_ = writeCodexError(output, message.ID, -32600, "A shell command is already running for thread")
					continue
				}
				active := activeTurns[binding.codexID]
				standalone := active == nil
				turnID := uuid.Must(uuid.NewV7()).String()
				parentCtx := serverCtx
				if active != nil {
					turnID = active.turnID
					parentCtx = active.ctx
				}
				itemID := uuid.Must(uuid.NewV7()).String()
				timeout := time.Hour
				if params.TimeoutMS != nil {
					timeout = time.Duration(*params.TimeoutMS) * time.Millisecond
				}
				shellCtx, cancelShell := context.WithTimeout(parentCtx, timeout)
				binding.loaded = true
				bridge.subscribeThread(binding)
				binding.systemError = false
				if standalone {
					binding.active = true
				}
				runningShells[binding.codexID] = cancelShell
				if err := writeCodexResult(output, message.ID, map[string]any{}); err != nil {
					cancelShell()
					return err
				}
				if standalone {
					turn := map[string]any{"id": turnID, "status": "inProgress", "items": []any{}, "error": nil}
					if err := state.writeThreadNotification(output, binding, "turn/started", map[string]any{"threadId": binding.codexID, "turn": turn}); err != nil {
						cancelShell()
						return err
					}
				}
				startedItem := codexShellCommandItem(codexShellResult{binding: binding, threadID: binding.codexID, turnID: turnID, itemID: itemID, command: command}, "inProgress", false)
				if err := state.writeThreadNotification(output, binding, "item/started", map[string]any{"threadId": binding.codexID, "turnId": turnID, "item": startedItem}); err != nil {
					cancelShell()
					return err
				}
				if standalone {
					if err := state.writeThreadNotification(output, binding, "thread/status/changed", map[string]any{"threadId": binding.codexID, "status": codexThreadStatus(binding, false)}); err != nil {
						cancelShell()
						return err
					}
				}
				workers.Add(1)
				go func() {
					defer workers.Done()
					defer cancelShell()
					result := bridge.runShellCommand(shellCtx, binding, binding.codexID, turnID, itemID, command, standalone)
					select {
					case shellDone <- result:
					case <-workerDeliveryCtx.Done():
					}
				}()
			case "turn/steer":
				var params struct {
					ThreadID                   string           `json:"threadId"`
					ExpectedTurnID             string           `json:"expectedTurnId"`
					ClientUserMessageID        *string          `json:"clientUserMessageId"`
					ResponsesAPIClientMetadata json.RawMessage  `json:"responsesapiClientMetadata"`
					AdditionalContext          json.RawMessage  `json:"additionalContext"`
					Input                      []codexTurnInput `json:"input"`
				}
				if json.Unmarshal(message.Params, &params) != nil || params.ThreadID == "" || params.ExpectedTurnID == "" {
					_ = writeCodexError(output, message.ID, -32600, "Invalid turn/steer params")
					continue
				}
				if inputErr := validateCodexTurnInputPresence(message.Params); inputErr != nil {
					_ = writeCodexError(output, message.ID, -32600, inputErr.Error())
					continue
				}
				if len(params.ResponsesAPIClientMetadata) != 0 && string(params.ResponsesAPIClientMetadata) != "null" || len(params.AdditionalContext) != 0 && string(params.AdditionalContext) != "null" {
					_ = writeCodexError(output, message.ID, -32600, "Unsupported turn/steer metadata in the Crux compatibility layer")
					continue
				}
				active := activeForThread(params.ThreadID)
				if active == nil {
					_ = writeCodexError(output, message.ID, -32600, "No active turn for thread")
					continue
				}
				if params.ExpectedTurnID != active.turnID {
					_ = writeCodexError(output, message.ID, -32600, "expectedTurnId does not match the active turn")
					continue
				}
				inputParts, inputErr := codexNativeTurnInput(params.Input)
				if inputErr != nil {
					_ = writeCodexError(output, message.ID, -32600, inputErr.Error())
					continue
				}
				userContent, contentErr := codexUserMessageContent(params.Input)
				if contentErr != nil {
					_ = writeCodexError(output, message.ID, -32600, contentErr.Error())
					continue
				}
				steerUserID := uuid.Must(uuid.NewV7()).String()
				steerCtx := agent.WithDeliveryMode(ctx, agent.DeliverySteer)
				if params.ClientUserMessageID != nil {
					steerCtx = agent.WithSubmissionID(steerCtx, *params.ClientUserMessageID)
				}
				if steerErr := active.binding.workspace.client.SendAgentMessage(steerCtx, active.binding.workspace.value.ID, proto.AgentMessage{
					SessionID: active.binding.nativeID, MessageRunID: active.runID, UserMessageID: steerUserID, AssistantItemID: active.assistantItemID, RequireActiveRun: true, WaitForAdmission: true, InputParts: inputParts, PermissionMode: active.permissionMode,
				}); steerErr != nil {
					_ = writeCodexError(output, message.ID, -32000, steerErr.Error())
					continue
				}
				if err := writeCodexResult(output, message.ID, map[string]any{"turnId": active.turnID}); err != nil {
					return err
				}
				var clientID any
				if params.ClientUserMessageID != nil {
					clientID = *params.ClientUserMessageID
				}
				userItem := map[string]any{"id": steerUserID, "type": "userMessage", "clientId": clientID, "content": userContent}
				active.items = append(active.items, userItem)
				if err := state.writeThreadNotification(output, active.binding, "item/started", map[string]any{"threadId": active.threadID, "turnId": active.turnID, "item": userItem}); err != nil {
					return err
				}
			case "turn/interrupt":
				var params struct {
					ThreadID string `json:"threadId"`
					TurnID   string `json:"turnId"`
				}
				if paramsErr := json.Unmarshal(message.Params, &params); paramsErr != nil || params.ThreadID == "" || params.TurnID == "" {
					_ = writeCodexError(output, message.ID, -32602, "Invalid turn/interrupt params")
					continue
				}
				active := activeForThread(params.ThreadID)
				if active == nil || active.turnID != params.TurnID {
					_ = writeCodexError(output, message.ID, -32600, "No matching active turn")
					continue
				}
				if cancelErr := active.binding.workspace.client.CancelAgentSession(ctx, active.binding.workspace.value.ID, active.binding.nativeID); cancelErr != nil {
					_ = writeCodexError(output, message.ID, -32000, cancelErr.Error())
					continue
				}
				if len(message.ID) != 0 {
					active.pendingInterrupts = append(active.pendingInterrupts, append(json.RawMessage(nil), message.ID...))
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
	return shutdownErr
}
