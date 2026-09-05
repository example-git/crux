package compatibility

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

const BypassEnvironment = "CRUX_COMPATIBILITY_BYPASS"

type ExecutionStyle string

const (
	ExecutionInteractive ExecutionStyle = "interactive"
	ExecutionHeadless    ExecutionStyle = "headless"
)

type PromptSource string

const (
	PromptArguments  PromptSource = "arguments"
	PromptStdin      PromptSource = "stdin"
	PromptCombined   PromptSource = "combined"
	PromptStreamJSON PromptSource = "stream-json"
)

type SessionMode string

const (
	SessionNew      SessionMode = "new"
	SessionLatest   SessionMode = "latest"
	SessionExplicit SessionMode = "explicit"
	SessionFork     SessionMode = "fork"
)

type RealtimeProtocol string

const (
	ProtocolCodexAppServer RealtimeProtocol = "codex-app-server"
	ProtocolCodexSchema    RealtimeProtocol = "codex-schema"
	ProtocolClaudeSDK      RealtimeProtocol = "claude-sdk"
	ProtocolCopilotACP     RealtimeProtocol = "copilot-acp"
	ProtocolCopilotSDK     RealtimeProtocol = "copilot-sdk"
)

type OutputMode string

const (
	OutputText       OutputMode = "text"
	OutputJSON       OutputMode = "json"
	OutputJSONLines  OutputMode = "json-lines"
	OutputStreamJSON OutputMode = "stream-json"
)

type PermissionMode string

type SandboxMode string

type Invocation struct {
	Executable string
	Name       string
	Args       []string
	WorkingDir string
	Env        []string
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
}

type Prompt struct {
	Source PromptSource
	Text   string
	Stdin  io.Reader
}

type Session struct {
	Mode       SessionMode
	ID         string
	Persistent bool
}

type PermissionPolicy struct {
	Mode            PermissionMode
	Sandbox         SandboxMode
	Bypass          bool
	AllowedTools    []string
	DeniedTools     []string
	AllowedPaths    []string
	DeniedPaths     []string
	AllowedURLs     []string
	DeniedURLs      []string
	AdditionalRules []string
}

type Output struct {
	Mode            OutputMode
	Schema          []byte
	LastMessagePath string
	Quiet           bool
	Verbose         bool
}

type Limits struct {
	Timeout   time.Duration
	MaxTurns  int
	BudgetUSD float64
}

type Request struct {
	Source                string
	Protocol              RealtimeProtocol
	Style                 ExecutionStyle
	Prompt                Prompt
	WorkingDir            string
	AdditionalDirectories []string
	Model                 string
	SmallModel            string
	Agent                 string
	Effort                string
	Session               Session
	Permissions           PermissionPolicy
	Output                Output
	Limits                Limits
	Attachments           []string
	SystemPrompt          string
	AppendSystemPrompt    string
	Metadata              map[string]string
}

type Adapter interface {
	Translate(context.Context, Invocation) (Request, error)
}

type Runtime interface {
	Execute(context.Context, Invocation, Request) error
}

type Registration struct {
	Name    string
	Adapter Adapter
	Runtime Runtime
}

type ExitError struct {
	Code   int
	Stdout string
	Stderr string
}

func (e *ExitError) Error() string {
	if e.Stderr != "" {
		return strings.TrimSpace(e.Stderr)
	}
	return strings.TrimSpace(e.Stdout)
}

type Registry struct {
	mu            sync.RWMutex
	registrations map[string]Registration
}

func NewRegistry() *Registry {
	return &Registry{registrations: make(map[string]Registration)}
}

func (r *Registry) Register(registration Registration) error {
	name := normalizeName(registration.Name)
	if name == "" {
		return errors.New("compatibility registration name is required")
	}
	if registration.Adapter == nil {
		return fmt.Errorf("compatibility adapter for %q is required", name)
	}
	if registration.Runtime == nil {
		return fmt.Errorf("compatibility runtime for %q is required", name)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.registrations[name]; exists {
		return fmt.Errorf("compatibility adapter %q is already registered", name)
	}
	registration.Name = name
	r.registrations[name] = registration
	return nil
}

func (r *Registry) Dispatch(ctx context.Context, invocation Invocation) (int, bool) {
	if environmentValue(invocation.Env, BypassEnvironment) != "" {
		return 0, false
	}

	name := executableName(invocation.Executable)

	r.mu.RLock()
	registration, ok := r.registrations[name]
	r.mu.RUnlock()
	if !ok {
		return 0, false
	}

	invocation.Name = name
	if invocation.Stdin == nil {
		invocation.Stdin = os.Stdin
	}
	if invocation.Stdout == nil {
		invocation.Stdout = os.Stdout
	}
	if invocation.Stderr == nil {
		invocation.Stderr = os.Stderr
	}
	if invocation.WorkingDir == "" {
		workingDir, err := os.Getwd()
		if err != nil {
			return writeError(invocation, err), true
		}
		invocation.WorkingDir = workingDir
	}
	invocation.Env = setEnvironment(invocation.Env, BypassEnvironment, "1")

	request, err := registration.Adapter.Translate(ctx, invocation)
	if err != nil {
		return writeError(invocation, err), true
	}
	if request.Source == "" {
		request.Source = name
	}
	if request.WorkingDir == "" {
		request.WorkingDir = invocation.WorkingDir
	}
	if err := executeWithBypass(ctx, registration.Runtime, invocation, request); err != nil {
		return writeError(invocation, err), true
	}
	return 0, true
}

var processEnvironmentMu sync.Mutex

func executeWithBypass(ctx context.Context, runtime Runtime, invocation Invocation, request Request) error {
	processEnvironmentMu.Lock()
	defer processEnvironmentMu.Unlock()

	previous, existed := os.LookupEnv(BypassEnvironment)
	if err := os.Setenv(BypassEnvironment, "1"); err != nil {
		return fmt.Errorf("set compatibility child bypass: %w", err)
	}
	defer func() {
		if existed {
			_ = os.Setenv(BypassEnvironment, previous)
		} else {
			_ = os.Unsetenv(BypassEnvironment)
		}
	}()
	return runtime.Execute(ctx, invocation, request)
}

func executableName(executable string) string {
	name := filepath.Base(executable)
	if runtime.GOOS == "windows" && strings.EqualFold(filepath.Ext(name), ".exe") {
		name = strings.TrimSuffix(name, filepath.Ext(name))
	}
	return normalizeName(name)
}

func normalizeName(name string) string {
	return strings.TrimSpace(name)
}

func writeError(invocation Invocation, err error) int {
	if exitError, ok := errors.AsType[*ExitError](err); ok {
		if exitError.Stdout != "" {
			_, _ = io.WriteString(invocation.Stdout, exitError.Stdout)
		}
		if exitError.Stderr != "" {
			_, _ = io.WriteString(invocation.Stderr, exitError.Stderr)
		}
		return exitError.Code
	}
	_, _ = fmt.Fprintln(invocation.Stderr, err)
	return 1
}

func environmentValue(environment []string, key string) string {
	prefix := key + "="
	for _, entry := range slices.Backward(environment) {
		if value, ok := strings.CutPrefix(entry, prefix); ok {
			return value
		}
	}
	return ""
}

func setEnvironment(environment []string, key, value string) []string {
	prefix := key + "="
	updated := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			updated = append(updated, entry)
		}
	}
	return append(updated, prefix+value)
}

var defaultRegistry = NewRegistry()

func Register(registration Registration) error {
	return defaultRegistry.Register(registration)
}

func Dispatch(ctx context.Context, invocation Invocation) (int, bool) {
	return defaultRegistry.Dispatch(ctx, invocation)
}
