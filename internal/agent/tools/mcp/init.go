// Package mcp provides functionality for managing Model Context Protocol (MCP)
// clients within the Crux application.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/home"
	"github.com/example-git/crux/internal/oauth"
	mcpoauth "github.com/example-git/crux/internal/oauth/mcp"
	"github.com/example-git/crux/internal/permission"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/example-git/crux/internal/version"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"
)

// parseLevel converts an MCP logging level string to a slog.Level. The
// entire MCP logging feature is deprecated per SEP-2577 but remains
// functional; servers may still send log notifications during the
// deprecation window.
func parseLevel(level string) slog.Level {
	switch level {
	case "info":
		return slog.LevelInfo
	case "notice":
		return slog.LevelInfo
	case "warning":
		return slog.LevelWarn
	default:
		return slog.LevelDebug
	}
}

// ClientSession wraps an mcp.ClientSession with a context cancel function so
// that the context created during session establishment is properly cleaned up
// on close.
type ClientSession struct {
	*mcp.ClientSession
	cancel       context.CancelFunc
	oauthHandler *mcpoauth.Handler
	closeOnce    sync.Once
	closeErr     error
}

// Close cancels the session context and then closes the underlying session.
func (s *ClientSession) Close() error {
	s.closeOnce.Do(func() {
		s.cancel()
		if s.oauthHandler != nil {
			s.oauthHandler.Close()
		}
		s.closeErr = s.ClientSession.Close()
	})
	return s.closeErr
}

// suppressBrowserKey marks a context as requesting the OAuth handler not
// open a local browser; the caller surfaces the authorization URL itself.
type suppressBrowserKey struct{}

// ArmInit marks that MCP initialization is expected so WaitForInit blocks
// until it completes. Call this synchronously before launching Initialize in a
// goroutine; otherwise WaitForInit could observe the not-yet-started state and
// return early, letting the tool list be read before MCP tools register.
func (runtime *Manager) ArmInit() {
	runtime.initMu.Lock()
	runtime.initStarted = true
	runtime.initMu.Unlock()
}

// finishInit records the terminal startup result before releasing waiters.
func (runtime *Manager) finishInit(err error) {
	runtime.initOnce.Do(func() {
		runtime.initMu.Lock()
		runtime.initErr = err
		runtime.initMu.Unlock()
		close(runtime.initDone)
	})
}

// DisarmInit undoes ArmInit so WaitForInit stops blocking and returns
// immediately. It exists for tests in other packages that arm the gate
// without ever running Initialize and must not leak a permanently-blocking
// gate into the rest of the test binary. Production code never needs it.
func (runtime *Manager) DisarmInit() {
	runtime.initMu.Lock()
	runtime.initStarted = false
	runtime.initMu.Unlock()
}

// renewLock returns the per-server mutex used to serialize session renewals,
// creating it on first use.
func (runtime *Manager) renewLock(name string) *sync.Mutex {
	runtime.renewMusMu.Lock()
	defer runtime.renewMusMu.Unlock()
	mu, ok := runtime.renewMus[name]
	if !ok {
		mu = &sync.Mutex{}
		runtime.renewMus[name] = mu
	}
	return mu
}

// State represents the current state of an MCP client
type State int

const (
	StateDisabled State = iota
	StateStarting
	StateConnected
	StateError
	StateNeedsAuth
)

func (s State) String() string {
	switch s {
	case StateDisabled:
		return "disabled"
	case StateStarting:
		return "starting"
	case StateConnected:
		return "connected"
	case StateError:
		return "error"
	case StateNeedsAuth:
		return "needs auth"
	default:
		return "unknown"
	}
}

// EventType represents the type of MCP event
type EventType uint

const (
	EventStateChanged EventType = iota
	EventToolsListChanged
	EventPromptsListChanged
	EventResourcesListChanged
	// EventChannelMessage is published when a channel server pushes a
	// notifications/claude/channel event. ChannelMessage carries the rendered,
	// escaped <channel> element ready for injection into the session.
	EventChannelMessage
)

// Event represents an event in the MCP system
type Event struct {
	Type   EventType
	Name   string
	State  State
	Error  error
	Counts Counts
	// ChannelMessage is set only for EventChannelMessage: the fully rendered
	// and escaped <channel>...</channel> element to inject into the session.
	ChannelMessage string
}

// Counts number of available tools, prompts, etc.
type Counts struct {
	Tools     int
	Prompts   int
	Resources int
}

// ClientInfo holds information about an MCP client's state.
type ClientInfo struct {
	Name        string
	State       State
	Error       error
	Client      *ClientSession
	Counts      Counts
	ConnectedAt time.Time

	// Config is the configuration the server last successfully connected
	// with. Reconcile compares it against the live config to decide whether
	// a connected server needs a restart. It is recorded by updateState on
	// StateConnected and cleared on StateDisabled, so it never has to be
	// synced by hand.
	Config config.MCPConfig

	// PendingConfig is the configuration an in-flight initialization is
	// connecting with. It is set on StateStarting so a config change that
	// arrives mid-connect is compared against the attempt actually in
	// progress rather than the last successful one, which would leave the
	// server skipped as "starting" and never restarted for the new config.
	PendingConfig *config.MCPConfig
}

// SubscribeEvents returns a channel for MCP events.
//
// Channel message events (EventChannelMessage) are excluded. The broker belongs
// to this workspace, but these messages still lack a destination session. Keep
// them out of the workspace event fan-out until session routing is explicit.
func (runtime *Manager) SubscribeEvents(ctx context.Context) <-chan pubsub.Event[Event] {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(runtime.ctx, cancel)
	if runtime.ctx.Err() != nil {
		cancel()
	}
	raw := runtime.broker.Subscribe(ctx)
	filtered := make(chan pubsub.Event[Event], 64)
	go func() {
		defer func() { stop(); cancel(); close(filtered) }()
		for ev := range raw {
			if ev.Payload.Type == EventChannelMessage {
				continue
			}
			select {
			case filtered <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return filtered
}

// GetStates returns the current state of all MCP clients
func (runtime *Manager) GetStates() map[string]ClientInfo {
	if runtime.ctx.Err() != nil {
		return nil
	}
	return runtime.states.Copy()
}

// GetState returns the state of a specific MCP client
func (runtime *Manager) GetState(name string) (ClientInfo, bool) {
	if runtime.ctx.Err() != nil {
		return ClientInfo{}, false
	}
	return runtime.states.Get(name)
}

// Initialize initializes MCP clients based on the provided configuration.
func (runtime *Manager) Initialize(ctx context.Context, permissions permission.Service, cfg *config.ConfigStore) {
	if err := runtime.requireStore(cfg); err != nil {
		return
	}
	runtime.ArmInit()
	ctx, done, err := runtime.admit(ctx)
	if err != nil {
		runtime.finishInit(err)
		return
	}
	defer done()
	slog.Info("Initializing MCP clients")
	start := time.Now()

	var wg sync.WaitGroup
	// Initialize runtime.states for all configured MCPs
	for name, m := range cfg.Config().MCP {
		if m.Disabled {
			runtime.updateState(name, StateDisabled, nil, nil, Counts{})
			slog.Debug("Skipping disabled MCP", "name", name)
			continue
		}

		// Set initial starting state
		wg.Add(1)
		runtime.goInitClient(ctx, cfg, name, m, &wg)
	}
	wg.Wait()
	runtime.finishInit(ctx.Err())
	// Non-interactive runs wait for this to finish before sending a prompt, so
	// the total is the floor on their startup latency. Interactive runs do not
	// wait, but the total still explains when late-arriving tools show up.
	slog.Debug("Finished initializing MCP clients", "duration", time.Since(start).Truncate(time.Millisecond).String())
}

// WaitForInit blocks until MCP initialization is complete, i.e. until
// Initialize has finished and closed runtime.initDone. If initialization was never
// armed (ArmInit was not called, e.g. a coordinator built outside app
// startup), there is nothing to wait for and this returns nil immediately
// rather than blocking until ctx is cancelled.
func (runtime *Manager) WaitForInit(ctx context.Context) error {
	if runtime.ctx.Err() != nil {
		return ErrClosed
	}
	runtime.initMu.Lock()
	started := runtime.initStarted
	runtime.initMu.Unlock()
	if !started {
		return nil
	}
	select {
	case <-runtime.initDone:
		if runtime.ctx.Err() != nil {
			return ErrClosed
		}
		runtime.initMu.Lock()
		err := runtime.initErr
		runtime.initMu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// InitializeSingle initializes a single MCP client by name.
func (runtime *Manager) InitializeSingle(ctx context.Context, name string, cfg *config.ConfigStore) error {
	if err := runtime.requireStore(cfg); err != nil {
		return err
	}
	m, exists := cfg.Config().MCP[name]
	if !exists {
		return fmt.Errorf("mcp '%s' not found in configuration", name)
	}

	if m.Disabled {
		return runtime.DisableSingle(cfg, name)
	}

	return runtime.initClient(ctx, cfg, name, m, runtime.currentGen(name), cfg.Resolver())
}

// AuthenticateMCP initiates the OAuth flow for an MCP server that is in
// StateNeedsAuth. It creates the OAuth handler (which starts a local
// callback server), connects to the server (which triggers the browser
// auth flow on 401), and transitions to StateConnected on success.
func (runtime *Manager) AuthenticateMCP(ctx context.Context, cfg *config.ConfigStore, name string) error {
	if err := runtime.requireStore(cfg); err != nil {
		return err
	}
	ctx, done, admissionErr := runtime.serverOperation(ctx, name)
	if admissionErr != nil {
		return admissionErr
	}
	defer done()
	m, exists := cfg.Config().MCP[name]
	if !exists {
		return fmt.Errorf("mcp '%s' not found in configuration", name)
	}

	if !m.OAuth || m.Type != config.MCPHttp {
		return fmt.Errorf("mcp '%s' does not use OAuth authentication", name)
	}

	runtime.updateState(name, StateStarting, nil, nil, Counts{}, withPending(m))

	// This is the user-initiated flow, so permit the interactive browser
	// authorization the handler otherwise withholds during startup.
	ctx = mcpoauth.WithInteractive(ctx)

	// The OAuth handler persists the token automatically as it is
	// exchanged, so a successful connection has already saved it.
	_, err := runtime.connectAndRegister(ctx, cfg, name, m, runtime.currentGen(name), cfg.Resolver(), channelEnabled(cfg.Overrides().EnabledChannels, name))
	if err != nil {
		return err
	}
	return nil
}

// PendingAuthServer describes an MCP server awaiting OAuth.
type PendingAuthServer struct {
	Name string
	URL  string
}

// MCPAuthURL returns the current OAuth authorization URL for the named
// MCP, or empty if none is in progress.
func (runtime *Manager) MCPAuthURL(name string) string {
	if runtime.ctx.Err() != nil {
		return ""
	}
	h, ok := runtime.authURLs.Get(name)
	if !ok || h == nil {
		return ""
	}
	return h.AuthURL()
}

// PendingAuthMCPs returns MCP servers in StateNeedsAuth with their URLs.
func (runtime *Manager) PendingAuthMCPs(cfg *config.ConfigStore) []PendingAuthServer {
	if err := runtime.requireStore(cfg); err != nil {
		return nil
	}
	var pending []PendingAuthServer
	for name, info := range runtime.states.Seq2() {
		if info.State == StateNeedsAuth {
			url := ""
			if m, ok := cfg.Config().MCP[name]; ok {
				url = m.URL
			}
			pending = append(pending, PendingAuthServer{Name: name, URL: url})
		}
	}
	slices.SortFunc(pending, func(a, b PendingAuthServer) int {
		return strings.Compare(a.Name, b.Name)
	})
	return pending
}

// BeginAuth starts the OAuth flow for a server in StateNeedsAuth but
// suppresses opening a local browser; the caller is responsible for
// surfacing the authorization URL (via [MCPAuthURL]) to the user. It returns
// a finish function that must be called exactly once with the request
// context: finish blocks until the flow completes and returns the result.
//
// Only one browser-suppressed flow per server may be in progress. The
// returned cancel function aborts the flow without waiting; use it when the
// caller's context is cancelled.
func (runtime *Manager) BeginAuth(cfg *config.ConfigStore, name string) (finish func(ctx context.Context) error, cancel context.CancelFunc, err error) {
	if err := runtime.requireStore(cfg); err != nil {
		return nil, nil, err
	}
	if runtime.ctx.Err() != nil {
		return nil, nil, ErrClosed
	}
	m, exists := cfg.Config().MCP[name]
	if !exists {
		return nil, nil, fmt.Errorf("mcp '%s' not found in configuration", name)
	}
	if !m.OAuth || m.Type != config.MCPHttp {
		return nil, nil, fmt.Errorf("mcp '%s' does not use OAuth authentication", name)
	}

	lock := runtime.suppressLock(name)
	if !lock.TryLock() {
		return nil, nil, fmt.Errorf("mcp '%s' already has an authentication in progress", name)
	}

	flowCtx, flowCancel := context.WithCancel(runtime.ctx)
	flowCtx = mcpoauth.WithInteractive(flowCtx)
	flowCtx = context.WithValue(flowCtx, suppressBrowserKey{}, true)

	finish = func(ctx context.Context) error {
		defer lock.Unlock()
		defer flowCancel()

		done := make(chan error, 1)
		go func() {
			done <- runtime.runAuthFlow(flowCtx, cfg, name, m)
		}()

		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			flowCancel()
			<-done
			return ctx.Err()
		}
	}
	return finish, flowCancel, nil
}

// runAuthFlow executes the OAuth connect for BeginAuth with browser
// suppression enabled on the freshly created handler.
func (runtime *Manager) runAuthFlow(ctx context.Context, cfg *config.ConfigStore, name string, m config.MCPConfig) error {
	ctx, done, admissionErr := runtime.serverOperation(ctx, name)
	if admissionErr != nil {
		return admissionErr
	}
	defer done()
	runtime.updateState(name, StateStarting, nil, nil, Counts{}, withPending(m))
	_, err := runtime.connectAndRegister(ctx, cfg, name, m, runtime.currentGen(name), cfg.Resolver(), channelEnabled(cfg.Overrides().EnabledChannels, name))
	return err
}

// suppressLock returns the per-server mutex used to serialize
// browser-suppressed OAuth flows, creating it on first use.
func (runtime *Manager) suppressLock(name string) *sync.Mutex {
	runtime.renewMusMu.Lock()
	defer runtime.renewMusMu.Unlock()
	mu, ok := runtime.suppressMus.Get(name)
	if !ok {
		mu = &sync.Mutex{}
		runtime.suppressMus.Set(name, mu)
	}
	return mu
}

// initClient initializes a single MCP client with the given configuration.
// gen is the server generation captured when the attempt was launched; the
// resulting session is only committed if the generation is still current, so
// a config change that restarts the server mid-connect discards this attempt.
func (runtime *Manager) initClient(ctx context.Context, cfg *config.ConfigStore, name string, m config.MCPConfig, gen uint64, resolver config.VariableResolver) error {
	ctx, done, admissionErr := runtime.serverOperation(ctx, name)
	if admissionErr != nil {
		return admissionErr
	}
	defer done()
	if ctx.Err() != nil || runtime.currentGen(name) != gen {
		return context.Canceled
	}
	// OAuth MCPs without a usable cached token require user interaction
	// (browser auth). If a cached token exists with an access token
	// (even if expired), try connecting first so the SDK can attempt a
	// silent refresh. Only defer to the UI if no token is available at
	// all or the token is structurally invalid (empty access token).
	if m.OAuth && m.Type == config.MCPHttp && !hasUsableToken(m.OAuthToken) {
		if m.OAuthToken != nil {
			clearOAuthToken(cfg, name)
		}
		runtime.updateState(name, StateNeedsAuth, nil, nil, Counts{})
		runtime.clearMCPData(name)
		slog.Info("MCP server requires OAuth authentication", "name", name)
		return nil
	}

	runtime.updateState(name, StateStarting, nil, nil, Counts{}, withPending(m))
	_, err := runtime.connectAndRegister(ctx, cfg, name, m, gen, resolver, channelEnabled(cfg.Overrides().EnabledChannels, name))
	if err != nil {
		// If an OAuth MCP fails because the saved token is no longer
		// valid (e.g. refresh token expired or revoked) or no token
		// could be obtained, clear the stale token and prompt the user
		// to re-authenticate instead of leaving the server stuck in
		// StateError.
		if m.OAuth && m.Type == config.MCPHttp && isOAuthInitErr(err) {
			if m.OAuthToken != nil {
				clearOAuthToken(cfg, name)
			}
			runtime.updateState(name, StateNeedsAuth, nil, nil, Counts{})
			slog.Info("MCP OAuth token is no longer valid, re-authentication required", "name", name, "error", err)
			return nil
		}
		return err
	}
	return nil
}

// connectAndRegister creates a session, lists tools and prompts,
// registers them in workspace state, and transitions to StateConnected.
// Returns the session so callers can perform post-processing (e.g.
// token persistence).
//
// gen is the generation captured when this attempt was launched. If the
// server was torn down since (generation bumped), the freshly built session
// is closed and discarded instead of being registered over whatever the
// newer attempt is doing. This is what makes a config change that lands
// mid-connect converge on the latest config rather than a stale one.
func (runtime *Manager) connectAndRegister(ctx context.Context, cfg *config.ConfigStore, name string, m config.MCPConfig, gen uint64, resolver config.VariableResolver, channelOptIn bool) (*ClientSession, error) {
	session, err := runtime.createSession(ctx, cfg, name, m, resolver, channelOptIn)
	if err != nil {
		return nil, err
	}

	// A teardown ran while we were connecting: a newer attempt owns this
	// server now. Bail before writing to any shared registry so we don't
	// clobber the newer attempt's registrations; just drop our own session.
	if runtime.currentGen(name) != gen || ctx.Err() != nil {
		slog.Debug("Discarding stale MCP session after config change", "name", name)
		closeSession(name, session)
		return nil, context.Canceled
	}

	toolCount, err := runtime.registerSessionTools(ctx, cfg, name, session)
	if err != nil {
		slog.Error("Error listing tools", "error", err)
		runtime.updateState(name, StateError, err, nil, Counts{})
		closeSession(name, session)
		return nil, err
	}

	prompts, err := getPrompts(ctx, session)
	if err != nil {
		slog.Error("Error listing prompts", "error", err)
		runtime.updateState(name, StateError, err, nil, Counts{})
		closeSession(name, session)
		return nil, err
	}

	// Re-check before publishing: if a teardown landed during registration a
	// newer attempt owns the registries now, so leave them and our session
	// alone rather than overwriting its state.
	if runtime.currentGen(name) != gen || ctx.Err() != nil {
		slog.Debug("Discarding stale MCP session after config change", "name", name)
		closeSession(name, session)
		return nil, context.Canceled
	}

	runtime.updatePrompts(name, prompts)
	runtime.sessions.Set(name, session)

	runtime.updateState(name, StateConnected, nil, session, Counts{
		Tools:   toolCount,
		Prompts: len(prompts),
	}, withConfig(m))

	return session, nil
}

// persistOAuthToken saves the OAuth token from a session to the global
// config so it survives restarts.

// DisableSingle disables and closes a single MCP client by name.
func (runtime *Manager) DisableSingle(cfg *config.ConfigStore, name string) error {
	if err := runtime.requireStore(cfg); err != nil {
		return err
	}
	_, done, err := runtime.serverOperation(context.Background(), name)
	if err != nil {
		return err
	}
	defer done()
	// teardown bumps the generation, invalidating any in-flight connect, and
	// the StateDisabled transition clears the recorded config so a later
	// re-enable (even with an unchanged config) is seen as new and restarts.
	runtime.teardown(name)
	runtime.updateState(name, StateDisabled, nil, nil, Counts{})
	slog.Info("Disabled mcp client", "name", name)
	return nil
}

// goInitClient launches initClient in a goroutine with panic recovery.
// Shared by Initialize and Reinitialize so the panic-to-state policy
// lives in one place. wg, if non-nil, is Done when the attempt finishes
// (success or failure); Initialize uses it to await startup. The goroutine
// captures the server's generation at launch so a concurrent teardown
// invalidates its result rather than letting it register a stale session.
func (runtime *Manager) goInitClient(ctx context.Context, cfg *config.ConfigStore, name string, m config.MCPConfig, wg *sync.WaitGroup) {
	gen := runtime.currentGen(name)
	go func() {
		if wg != nil {
			defer wg.Done()
		}
		defer func() {
			if r := recover(); r != nil {
				var err error
				switch v := r.(type) {
				case error:
					err = v
				case string:
					err = fmt.Errorf("panic: %s", v)
				default:
					err = fmt.Errorf("panic: %v", v)
				}
				runtime.updateState(name, StateError, err, nil, Counts{})
				slog.Error("Panic in MCP client initialization", "error", err, "name", name)
			}
		}()
		start := time.Now()
		err := runtime.initClient(ctx, cfg, name, m, gen, cfg.Resolver())
		slog.Debug(
			"MCP client initialization finished",
			"name", name,
			"duration", time.Since(start).Truncate(time.Millisecond).String(),
			"error", err,
		)
	}()
}

// currentGen returns a server's current generation without bumping it.
func (runtime *Manager) currentGen(name string) uint64 {
	g, _ := runtime.gens.Get(name)
	return g
}

// teardown closes a server's session and clears its tools, prompts,
// resources, and auth state, then bumps the server's generation so any
// in-flight initialization for it is discarded on commit. It leaves the
// runtime.states entry intact; callers decide whether to delete or update it.
// Shared by DisableSingle, removeServer, and the restart path in
// Reinitialize.
func (runtime *Manager) teardown(name string) {
	g, _ := runtime.gens.Get(name)
	runtime.gens.Set(name, g+1)
	if session, ok := runtime.sessions.Take(name); ok {
		closeSession(name, session)
	}
	runtime.clearMCPData(name)
}

func (runtime *Manager) getOrRenewClient(ctx context.Context, cfg *config.ConfigStore, name string) (*ClientSession, error) {
	m := cfg.Config().MCP[name]
	timeout := mcpTimeout(m)

	ctx, done, err := runtime.serverOperation(ctx, name)
	if err != nil {
		return nil, err
	}
	defer done()
	if m.Disabled {
		return nil, fmt.Errorf("mcp '%s' is disabled", name)
	}
	// Under the lock the map is stable: any in-flight renewal has finished and
	// either re-registered its session or failed and left none. A renewal
	// removes the session transiently (StateError takes it before rebuilding),
	// so this check must happen here rather than before the lock — otherwise a
	// caller arriving mid-renewal sees no session and wrongly reports the
	// server unavailable.
	sess, ok := runtime.sessions.Get(name)
	if !ok {
		return nil, fmt.Errorf("mcp '%s' not available", name)
	}

	// A concurrent goroutine may have already renewed the session while we
	// waited for the lock. Reuse it if it is now healthy.
	pingErr := pingSession(ctx, sess, timeout)
	if pingErr == nil {
		return sess, nil
	}

	state, _ := runtime.states.Get(name)
	// StateError closes the dead session and clears its tools, prompts, and
	// resources from the registry.
	runtime.updateState(name, StateError, maybeTimeoutErr(pingErr, timeout), nil, state.Counts)

	// Capture the generation so a reconcile teardown that lands mid-renewal
	// invalidates this rebuild instead of letting it clobber the newer one.
	gen := runtime.currentGen(name)
	newSess, err := runtime.newSession(ctx, cfg, name, m, cfg.Resolver(), channelEnabled(cfg.Overrides().EnabledChannels, name))
	if err != nil {
		runtime.clearMCPData(name)
		// If an OAuth MCP fails to reconnect because the token is no
		// longer valid, clear the stale token and prompt the user to
		// re-authenticate instead of leaving it in an error state.
		if m.OAuth && m.Type == config.MCPHttp {
			if m.OAuthToken != nil && isOAuthInitErr(err) {
				clearOAuthToken(cfg, name)
			}
			runtime.updateState(name, StateNeedsAuth, nil, nil, Counts{})
			slog.Info("MCP OAuth session expired, re-authentication required", "name", name, "error", err)
		}
		return nil, err
	}

	// A reconcile teardown ran while we were rebuilding: a newer attempt owns
	// this server now. Bail before writing to any shared registry so we don't
	// clobber the newer attempt's registrations; just drop our own session.
	if runtime.currentGen(name) != gen || ctx.Err() != nil {
		closeSession(name, newSess)
		return nil, context.Canceled
	}

	// StateError cleared this server's tools, prompts, and resources from the
	// registry. Re-list and re-register them all on the fresh session and
	// recompute the counts from what actually registered; otherwise the agent
	// reconnects but the registries stay empty (the next tool call fails with
	// "tool not found") while the reported counts still advertise capabilities
	// that are no longer there.
	var counts Counts
	counts.Tools, err = runtime.registerSessionTools(ctx, cfg, name, newSess)
	if err != nil {
		runtime.updateState(name, StateError, err, nil, Counts{})
		closeSession(name, newSess)
		return nil, err
	}

	prompts, err := getPrompts(ctx, newSess)
	if err != nil {
		runtime.updateState(name, StateError, err, nil, Counts{})
		closeSession(name, newSess)
		return nil, err
	}
	runtime.updatePrompts(name, prompts)
	counts.Prompts = len(prompts)

	resources, err := getResources(ctx, newSess)
	if err != nil {
		runtime.updateState(name, StateError, err, nil, Counts{})
		closeSession(name, newSess)
		return nil, err
	}
	counts.Resources = runtime.updateResources(name, resources)

	// Re-check before publishing: if a teardown landed during registration a
	// newer attempt owns the registries now, so leave them and our session
	// alone rather than overwriting its state.
	if runtime.currentGen(name) != gen || ctx.Err() != nil {
		closeSession(name, newSess)
		return nil, context.Canceled
	}

	runtime.sessions.Set(name, newSess)
	runtime.updateState(name, StateConnected, nil, newSess, counts, withConfig(m))
	return newSess, nil
}

// pingSession pings a session with the server's configured timeout.
func pingSession(ctx context.Context, s *ClientSession, timeout time.Duration) error {
	pingCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return s.Ping(pingCtx, nil)
}

// closeSession closes an MCP session, logging only unexpected errors. EOF,
// context cancellation, and a killed child are the ordinary result of tearing
// a session down and are not worth surfacing.
func closeSession(name string, s *ClientSession) {
	if err := s.Close(); err != nil &&
		!errors.Is(err, io.EOF) &&
		!errors.Is(err, context.Canceled) &&
		err.Error() != "signal: killed" {
		slog.Warn("Error closing MCP session", "name", name, "error", err)
	}
}

// stateOpt mutates the ClientInfo a transition is about to publish. Config
// recording is opt-in: only the sites that actually own a config (the connect
// and starting paths) pass one, so the many error and count-refresh call sites
// can't accidentally clobber the recorded config by passing a zero value.
type stateOpt func(*ClientInfo)

// withConfig records the config now in effect. Used on StateConnected.
func withConfig(m config.MCPConfig) stateOpt {
	return func(i *ClientInfo) {
		i.Config = m
		i.PendingConfig = nil
	}
}

// withPending records the config an in-flight attempt is connecting with.
// Used on StateStarting.
func withPending(m config.MCPConfig) stateOpt {
	return func(i *ClientInfo) {
		mc := m
		i.PendingConfig = &mc
	}
}

// updateState updates the state of an MCP client and publishes an event.
//
// Config bookkeeping is split between the caller and the state machine:
//   - Callers that own a config opt in via withConfig (StateConnected) or
//     withPending (StateStarting). Everyone else leaves the recorded config
//     untouched, so an error or count refresh can't wipe it.
//   - The state machine owns the transitions with fixed semantics:
//     StateDisabled clears both so a later re-enable with an unchanged config
//     is seen as new rather than skipped as already initialized.
func (runtime *Manager) updateState(name string, state State, err error, client *ClientSession, counts Counts, opts ...stateOpt) {
	prev, _ := runtime.states.Get(name)
	info := prev
	info.Name = name
	info.State = state
	info.Error = err
	info.Client = client
	info.Counts = counts
	for _, opt := range opts {
		opt(&info)
	}
	switch state {
	case StateConnected:
		info.ConnectedAt = time.Now()
	case StateDisabled:
		info.Config = config.MCPConfig{}
		info.PendingConfig = nil
	case StateError:
		// A session that has errored is dead to us. Atomically remove it and
		// close it so the child process and its stdio pipes are released — the
		// bare map delete this used to do leaked both. Clearing the tool
		// registry keeps the agent from advertising tools it can no longer
		// call: without it, crux_info / the `/mcp` menu and the tool list
		// handed to the LLM diverge, so a server still reads "connected, N
		// tools" while every call fails with "tool not found".
		if old, ok := runtime.sessions.Take(name); ok {
			closeSession(name, old)
		}
		// Drop every registry entry for the dead server. Leaving prompts or
		// resources behind lets a disconnected server keep advertising
		// capabilities the agent can no longer fulfil, the same divergence the
		// tool clear prevents.
		runtime.allTools.Del(name)
		runtime.allPrompts.Del(name)
		runtime.allResources.Del(name)
	}
	runtime.states.Set(name, info)

	// Publish state change event
	runtime.broker.Publish(pubsub.UpdatedEvent, Event{
		Type:   EventStateChanged,
		Name:   name,
		State:  state,
		Error:  err,
		Counts: counts,
	})
}

func (runtime *Manager) createSession(ctx context.Context, cfg *config.ConfigStore, name string, m config.MCPConfig, resolver config.VariableResolver, channelOptIn bool) (*ClientSession, error) {
	timeout := mcpTimeout(m)
	mcpCtx, sessionCancel := context.WithCancel(context.WithoutCancel(ctx))
	stopLifetime := context.AfterFunc(runtime.ctx, sessionCancel)
	cancel := func() { stopLifetime(); sessionCancel() }
	stopCaller := context.AfterFunc(ctx, cancel)
	defer stopCaller()
	if ctx.Err() != nil || runtime.ctx.Err() != nil {
		cancel()
		return nil, context.Canceled
	}
	cancelTimer := time.AfterFunc(timeout, cancel)

	transport, oauthHandler, err := runtime.createTransport(mcpCtx, cfg, name, m, resolver)
	if err != nil {
		runtime.updateState(name, StateError, err, nil, Counts{})
		slog.Error("Error creating MCP client", "error", err, "name", name)
		cancel()
		cancelTimer.Stop()
		return nil, err
	}

	// If the caller requested a browser-suppressed flow (server-driven
	// remote auth), suppress the handler's local browser open; the caller
	// surfaces runtime.MCPAuthURL(name) to the user on their own machine.
	if oauthHandler != nil {
		if suppress, _ := ctx.Value(suppressBrowserKey{}).(bool); suppress {
			oauthHandler.SetBrowserSuppress(true)
		}
	}

	// Wrap the transport so channel notifications can be intercepted. The
	// gate starts undecided: notifications that arrive during capability
	// negotiation are buffered. After Connect resolves, the gate is opened
	// (and the buffer drained) only when the server declares the channel
	// capability AND was opted in via --channels; otherwise it is closed
	// (buffer discarded). This prevents early notifications from being lost.
	channelGate := newChannelGate()
	transport = &channelTransport{runtime: runtime, inner: transport, name: name, gate: channelGate}

	client := mcp.NewClient(
		&mcp.Implementation{
			Name:    "crux",
			Version: version.Version,
			Title:   "Crux",
		},
		&mcp.ClientOptions{
			ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
				runtime.broker.Publish(pubsub.UpdatedEvent, Event{
					Type: EventToolsListChanged,
					Name: name,
				})
			},
			PromptListChangedHandler: func(context.Context, *mcp.PromptListChangedRequest) {
				runtime.broker.Publish(pubsub.UpdatedEvent, Event{
					Type: EventPromptsListChanged,
					Name: name,
				})
			},
			ResourceListChangedHandler: func(context.Context, *mcp.ResourceListChangedRequest) {
				runtime.broker.Publish(pubsub.UpdatedEvent, Event{
					Type: EventResourcesListChanged,
					Name: name,
				})
			},
			// Retain protocol logging during the SDK's documented deprecation window.
			LoggingMessageHandler: func(ctx context.Context, req *mcp.LoggingMessageRequest) { //nolint:staticcheck
				level := parseLevel(string(req.Params.Level))
				slog.Log(ctx, level, "MCP log", "name", name, "logger", req.Params.Logger, "data", req.Params.Data)
			},
		},
	)

	session, err := client.Connect(mcpCtx, transport, nil)
	if err != nil {
		err = resourceTransportError(transport, err)
		if oauthHandler != nil {
			oauthHandler.Close()
		}
		if ctx.Err() == nil && runtime.ctx.Err() == nil {
			err = maybeStdioErr(ctx, err, transport)
		}
		runtime.updateState(name, StateError, maybeTimeoutErr(err, timeout), nil, Counts{})
		slog.Error("MCP client failed to initialize", "error", err, "name", name)
		cancel()
		cancelTimer.Stop()
		return nil, err
	}

	cancelTimer.Stop()
	slog.Debug("MCP client initialized", "name", name)

	// Resolve the channel gate: open only for a server that both declares
	// the claude/channel capability and was opted in via --channels.
	// Otherwise close it (fail closed). Resolving drains buffered messages
	// that arrived during negotiation so a fast server does not lose early
	// events.
	if channelOptIn && hasChannelCapability(session.InitializeResult()) {
		buffered := channelGate.resolve(true)
		for _, raw := range buffered {
			runtime.publishChannelMessage(mcpCtx, name, raw)
		}
		slog.Info("MCP channel enabled", "name", name, "buffered", len(buffered))
	} else {
		channelGate.resolve(false)
	}

	return &ClientSession{
		ClientSession: session,
		cancel:        cancel,
		oauthHandler:  oauthHandler,
	}, nil
}

// Keep a captured HTTP refusal visible even when SDK protocol negotiation
// reports only the subsequent connection-close error.
func resourceTransportError(transport mcp.Transport, err error) error {
	switch transport := transport.(type) {
	case *channelTransport:
		return resourceTransportError(transport.inner, err)
	case *mcp.SSEClientTransport:
		return mcpoauth.ResourceHTTPError(transport.HTTPClient, err)
	case *mcp.StreamableClientTransport:
		return mcpoauth.ResourceHTTPError(transport.HTTPClient, err)
	default:
		return err
	}
}

// maybeStdioErr if a stdio mcp prints an error in non-json format, it'll fail
// to parse, and the cli will then close it, causing the EOF error.
// so, if we got an EOF err, and the transport is STDIO, we try to exec it
// again with a timeout and collect the output so we can add details to the
// error.
// this happens particularly when starting things with npx, e.g. if node can't
// be found or some other error like that.
func maybeStdioErr(ctx context.Context, err error, transport mcp.Transport) error {
	if !errors.Is(err, io.EOF) {
		return err
	}
	ct, ok := transport.(*mcp.CommandTransport)
	if !ok {
		return err
	}
	if err2 := stdioCheck(ctx, ct.Command); err2 != nil {
		err = errors.Join(err, err2)
	}
	return err
}

func maybeTimeoutErr(err error, timeout time.Duration) error {
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("timed out after %s", timeout)
	}
	return err
}

func (runtime *Manager) createTransport(ctx context.Context, cfg *config.ConfigStore, name string, m config.MCPConfig, resolver config.VariableResolver) (mcp.Transport, *mcpoauth.Handler, error) {
	switch m.Type {
	case config.MCPStdio:
		command, err := resolver.ResolveValue(m.Command)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid mcp command: %w", err)
		}
		if strings.TrimSpace(command) == "" {
			return nil, nil, fmt.Errorf("mcp stdio config requires a non-empty 'command' field")
		}
		args, err := m.ResolvedArgs(resolver)
		if err != nil {
			return nil, nil, err
		}
		envs, err := m.ResolvedEnv(resolver)
		if err != nil {
			return nil, nil, err
		}
		cmd := exec.CommandContext(ctx, home.Long(command), args...)
		environment := os.Environ()
		if cfg != nil {
			environment = cfg.Environment()
		}
		cmd.Env = append(environment, envs...)
		// Run the child in its own process group and kill the whole group when
		// the session context is cancelled. A stdio server often spawns its own
		// children (signal-mcp launches signal-cli); os/exec's default
		// cancellation kills only the direct child, orphaning the rest with
		// PPID 1 — production accumulated 15+ such zombies over two days.
		configureStdioProcess(cmd)
		return &mcp.CommandTransport{
			Command: cmd,
		}, nil, nil
	case config.MCPHttp:
		url, err := m.ResolvedURL(resolver)
		if err != nil {
			return nil, nil, err
		}
		if strings.TrimSpace(url) == "" {
			return nil, nil, fmt.Errorf("mcp http config requires a non-empty 'url' field")
		}

		// OAuth-enabled HTTP transport. The handler persists the token
		// (and the client registration/endpoints needed to refresh it)
		// on every exchange and refresh via this saver.
		if m.OAuth {
			tokenSaver := func(tok *oauth.Token) {
				if err := cfg.SetConfigField(config.ScopeGlobal, fmt.Sprintf("mcp.%s.oauth_token", name), tok); err != nil {
					slog.Warn("Failed to persist MCP OAuth token", "name", name, "error", err)
				} else {
					slog.Info("Persisted MCP OAuth token", "name", name)
				}
			}

			// A pre-registered client is required for servers that do not
			// support dynamic client registration (e.g. GitHub, Slack).
			// Resolve the credentials through the shell like other config
			// values so $VAR and $(cmd) work.
			var preregistered *oauth.OAuthClient
			if strings.TrimSpace(m.OAuthClientID) != "" {
				clientID, err := resolver.ResolveValue(m.OAuthClientID)
				if err != nil {
					return nil, nil, fmt.Errorf("oauth_client_id: %w", err)
				}
				clientSecret, err := resolver.ResolveValue(m.OAuthClientSecret)
				if err != nil {
					return nil, nil, fmt.Errorf("oauth_client_secret: %w", err)
				}
				preregistered = &oauth.OAuthClient{
					ClientID:     strings.TrimSpace(clientID),
					ClientSecret: strings.TrimSpace(clientSecret),
				}
			}

			// Normalize trailing slash for PRM discovery compatibility.
			normalizedURL := strings.TrimSuffix(url, "/")
			oauthHandler, oauthErr := mcpoauth.NewHandlerWithContext(ctx, name, normalizedURL, m.OAuthToken, preregistered, tokenSaver, mcpoauth.IsInteractive(ctx), m.OAuthCallbackPort)
			if oauthErr != nil {
				return nil, nil, fmt.Errorf("failed to create OAuth handler for mcp %q: %w", name, oauthErr)
			}
			runtime.authURLs.Set(name, oauthHandler)
			return &mcp.StreamableClientTransport{
				Endpoint:     url,
				OAuthHandler: oauthHandler,
				HTTPClient:   mcpoauth.ResourceHTTPClient(mcpoauth.NewSessionHTTPClient(ctx), url),
			}, oauthHandler, nil
		}

		headers, err := m.ResolvedHeaders(resolver)
		if err != nil {
			return nil, nil, err
		}
		client := mcpoauth.NewSessionHTTPClient(ctx)
		client.Transport = &headerRoundTripper{headers: headers, base: client.Transport}
		client = mcpoauth.ResourceHTTPClient(client, url)
		return &mcp.StreamableClientTransport{
			Endpoint:   url,
			HTTPClient: client,
		}, nil, nil
	case config.MCPSSE:
		url, err := m.ResolvedURL(resolver)
		if err != nil {
			return nil, nil, err
		}
		if strings.TrimSpace(url) == "" {
			return nil, nil, fmt.Errorf("mcp sse config requires a non-empty 'url' field")
		}
		headers, err := m.ResolvedHeaders(resolver)
		if err != nil {
			return nil, nil, err
		}

		sessionClient := mcpoauth.NewSessionHTTPClient(ctx)
		var transport http.RoundTripper = &headerRoundTripper{headers: headers, base: sessionClient.Transport}
		var oauthHandler *mcpoauth.Handler

		// SSE transports don't support the SDK's OAuthHandler natively,
		// so we wrap the HTTP transport with our own round-tripper that
		// injects bearer tokens and handles 401-triggered authorization.
		// Based on Bruno Krugel's oauthRoundTripper from PR #3396.
		if m.OAuth {
			tokenSaver := func(tok *oauth.Token) {
				if err := cfg.SetConfigField(config.ScopeGlobal, fmt.Sprintf("mcp.%s.oauth_token", name), tok); err != nil {
					slog.Warn("Failed to persist MCP OAuth token", "name", name, "error", err)
				} else {
					slog.Info("Persisted MCP OAuth token", "name", name)
				}
			}

			var preregistered *oauth.OAuthClient
			if strings.TrimSpace(m.OAuthClientID) != "" {
				clientID, err := resolver.ResolveValue(m.OAuthClientID)
				if err != nil {
					return nil, nil, fmt.Errorf("oauth_client_id: %w", err)
				}
				clientSecret, err := resolver.ResolveValue(m.OAuthClientSecret)
				if err != nil {
					return nil, nil, fmt.Errorf("oauth_client_secret: %w", err)
				}
				preregistered = &oauth.OAuthClient{
					ClientID:     strings.TrimSpace(clientID),
					ClientSecret: strings.TrimSpace(clientSecret),
				}
			}

			// Normalize trailing slash for PRM discovery compatibility.
			normalizedURL := strings.TrimSuffix(url, "/")
			handler, oauthErr := mcpoauth.NewHandlerWithContext(ctx, name, normalizedURL, m.OAuthToken, preregistered, tokenSaver, mcpoauth.IsInteractive(ctx), m.OAuthCallbackPort)
			if oauthErr != nil {
				return nil, nil, fmt.Errorf("failed to create OAuth handler for mcp %q: %w", name, oauthErr)
			}
			oauthHandler = handler
			runtime.authURLs.Set(name, handler)
			transport = newOAuthRoundTripper(handler, transport)
		}

		sessionClient.Transport = transport
		client := mcpoauth.ResourceHTTPClient(sessionClient, url)
		return &mcp.SSEClientTransport{
			Endpoint:   url,
			HTTPClient: client,
		}, oauthHandler, nil
	default:
		return nil, nil, fmt.Errorf("unsupported mcp type: %s", m.Type)
	}
}

type headerRoundTripper struct {
	base    http.RoundTripper
	headers map[string]string
}

func (rt headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	for k, v := range rt.headers {
		req.Header.Set(k, v)
	}
	base := rt.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// oauthRoundTripper wraps an HTTP transport with OAuth bearer token
// injection and 401-triggered authorization. Used for SSE transports
// that don't support the SDK's OAuthHandler natively. Based on Bruno
// Krugel's implementation from PR #3396.
type oauthRoundTripper struct {
	base    http.RoundTripper
	handler auth.OAuthHandler
}

func newOAuthRoundTripper(handler auth.OAuthHandler, base http.RoundTripper) *oauthRoundTripper {
	return &oauthRoundTripper{base: base, handler: handler}
}

func (rt *oauthRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := rt.doRequestWithToken(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		if authErr := rt.handler.Authorize(req.Context(), req, resp); authErr != nil {
			if nonRetryableMCPHTTPError(authErr) {
				resp.Body.Close()
				return nil, authErr
			}
			return resp, nil
		}
		resp.Body.Close()
		return rt.doRequestWithToken(req.Clone(req.Context()))
	}

	return resp, nil
}

func (rt *oauthRoundTripper) doRequestWithToken(req *http.Request) (*http.Response, error) {
	ts, err := rt.handler.TokenSource(req.Context())
	if err != nil {
		return nil, fmt.Errorf("oauth token source: %w", err)
	}
	if ts != nil {
		token, err := ts.Token()
		if nonRetryableMCPHTTPError(err) {
			return nil, err
		}
		if err == nil && token != nil {
			req.Header.Set("Authorization", "Bearer "+token.AccessToken)
		}
	}
	return rt.base.RoundTrip(req)
}

func nonRetryableMCPHTTPError(err error) bool {
	var refusal interface{ NonRetryable() bool }
	return errors.As(err, &refusal) && refusal.NonRetryable()
}

func mcpTimeout(m config.MCPConfig) time.Duration {
	if m.Timeout > 0 {
		return time.Duration(m.Timeout) * time.Second
	}
	// OAuth flows require user interaction in a browser, so use a
	// generous default to avoid timing out mid-auth.
	if m.OAuth {
		return 30 * time.Second
	}
	return 10 * time.Second
}

// hasUsableToken returns true if the saved OAuth token has an access
// token that can be used or refreshed. A token with an empty access
// token is structurally invalid and should be treated as missing.
func hasUsableToken(tok *oauth.Token) bool {
	return tok != nil && tok.AccessToken != ""
}

// isOAuthInitErr returns true if the error indicates the OAuth token
// is missing, no longer valid, or cannot be refreshed. This covers:
//   - invalid_grant: expired or revoked refresh tokens
//   - invalid_client: deleted or deactivated client registrations
//   - "no token available": the handler had no cached token to use
//   - interactive authorization was required but withheld during startup
func isOAuthInitErr(err error) bool {
	if errors.Is(err, mcpoauth.ErrInteractiveAuthRequired) {
		return true
	}
	var rErr *oauth2.RetrieveError
	if errors.As(err, &rErr) {
		return rErr.ErrorCode == "invalid_grant" || rErr.ErrorCode == "invalid_client"
	}
	msg := err.Error()
	return strings.Contains(msg, "invalid_grant") ||
		strings.Contains(msg, "invalid_client") ||
		strings.Contains(msg, "no token available")
}

// clearOAuthToken removes the persisted OAuth token for a named MCP
// server from the global config so subsequent startups don't retry
// with a known-bad refresh token.
func clearOAuthToken(cfg *config.ConfigStore, name string) {
	key := fmt.Sprintf("mcp.%s.oauth_token", name)
	if err := cfg.RemoveConfigField(config.ScopeGlobal, key); err != nil {
		slog.Warn("Failed to clear stale MCP OAuth token", "name", name, "error", err)
	}
}

// clearMCPData removes a stale MCP server's tools, prompts,
// resources, and auth handlers from workspace state so they are not
// served to the agent.
func (runtime *Manager) clearMCPData(name string) {
	runtime.allTools.Del(name)
	runtime.allPrompts.Del(name)
	runtime.allResources.Del(name)
	if h, ok := runtime.authURLs.Get(name); ok {
		h.Close()
		runtime.authURLs.Del(name)
	}
}

func stdioCheck(ctx context.Context, old *exec.Cmd) error {
	ctx, cancel := context.WithTimeout(ctx, time.Second*5)
	defer cancel()
	cmd := exec.CommandContext(ctx, old.Path, old.Args...)
	cmd.Env = old.Env
	out, err := cmd.CombinedOutput()
	if err == nil || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil
	}
	return fmt.Errorf("%w: %s", err, string(out))
}
