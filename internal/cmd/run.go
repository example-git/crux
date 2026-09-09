package cmd

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/format"
	"github.com/example-git/crux/internal/herdr"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/example-git/crux/internal/session"
	"github.com/example-git/crux/internal/ui/anim"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/example-git/crux/internal/workspace"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

var runCmd = &cobra.Command{
	Aliases: []string{"r"},
	Use:     "run [prompt...]",
	Short:   "Run a single non-interactive prompt",
	Long: `Run a single prompt in non-interactive mode and exit.
The prompt can be provided as arguments or piped from stdin.`,
	Example: `
# Run a simple prompt
crux run "Guess my 5 favorite Pokémon"

# Pipe input from stdin
curl https://example.com | crux run "Summarize this website"

# Read from a file
crux run "What is this code doing?" <<< prrr.go

# Redirect output to a file
crux run "Generate a hot README for this project" > MY_HOT_README.md

# Run in quiet mode (hide the spinner)
crux run --quiet "Generate a README for this project"

# Run in verbose mode (show logs)
crux run --verbose "Generate a README for this project"

# Continue a previous session
crux run --session {session-id} "Follow up on your last response"

# Continue the most recent session
crux run --continue "Follow up on your last response"

  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		var (
			quiet, _          = cmd.Flags().GetBool("quiet")
			verbose, _        = cmd.Flags().GetBool("verbose")
			largeModel, _     = cmd.Flags().GetString("model")
			smallModel, _     = cmd.Flags().GetString("small-model")
			sessionID, _      = cmd.Flags().GetString("session")
			useLast, _        = cmd.Flags().GetBool("continue")
			permissionMode, _ = cmd.Flags().GetString("compatibility-permission-mode")
		)

		// Cancel on SIGINT or SIGTERM.
		parentContext := cmd.Context()
		ctx, cancel := signal.NotifyContext(parentContext, os.Interrupt, syscall.SIGTERM)
		defer cancel()
		cmd.SetContext(ctx)
		defer cmd.SetContext(parentContext)
		if err := validateRunModelFlags(cmd); err != nil {
			return err
		}

		if permissionMode != string(proto.AgentPermissionDeny) && permissionMode != string(proto.AgentPermissionBypass) {
			return fmt.Errorf("invalid compatibility permission mode %q", permissionMode)
		}

		prompt := strings.Join(args, " ")

		prompt, err := MaybePrependStdin(prompt)
		if err != nil {
			slog.Error("Failed to read from stdin", "error", err)
			return err
		}

		if prompt == "" {
			return fmt.Errorf("no prompt provided")
		}

		if useClientServer() {
			c, ws, _, err := connectToServer(cmd)
			if err != nil {
				return err
			}
			clientWs := workspace.NewClientWorkspace(c, *ws)
			defer clientWs.Shutdown()

			if sessionID != "" {
				sess, err := resolveSessionByID(ctx, c, ws.ID, sessionID)
				if err != nil {
					return err
				}
				sessionID = sess.ID
			}

			if verbose {
				slog.SetDefault(slog.New(log.New(os.Stderr)))
			}

			// Authenticated connection setup already resolved and admitted the
			// complete flag selection before collecting its provider dependencies.
			// Re-resolving implicit defaults against that changed main model can
			// choose a different auxiliary model.
			modelsPrepared := c.LocalRuntimeStore() != nil && (largeModel != "" || smallModel != "")
			return runNonInteractiveWithWorkspace(ctx, c, ws, clientWs, prompt, largeModel, smallModel, quiet || verbose, sessionID, useLast, proto.AgentPermissionMode(permissionMode), modelsPrepared)
		}

		ws, cleanup, err := setupLocalWorkspace(cmd)
		if err != nil {
			return err
		}
		defer cleanup()

		if verbose {
			slog.SetDefault(slog.New(log.New(os.Stderr)))
		}

		appWs := ws.(*workspace.AppWorkspace)

		if sessionID != "" {
			sess, err := resolveSessionID(ctx, appWs.App().Sessions, sessionID)
			if err != nil {
				return err
			}
			sessionID = sess.ID
		}

		return appWs.App().RunNonInteractive(ctx, os.Stdout, prompt, largeModel, smallModel, quiet || verbose, sessionID, useLast, permissionMode == string(proto.AgentPermissionBypass))
	},
}

func init() {
	runCmd.Flags().BoolP("quiet", "q", false, "Hide spinner")
	runCmd.Flags().BoolP("verbose", "v", false, "Show logs")
	runCmd.Flags().StringP("model", "m", "", "Model to use. Accepts 'model' or 'provider/model' to disambiguate models with the same name across providers")
	runCmd.Flags().String("small-model", "", "Small model to use. If not provided, uses the default small model for the provider")
	runCmd.Flags().StringP("session", "s", "", "Continue a previous session by ID")
	runCmd.Flags().BoolP("continue", "C", false, "Continue the most recent session")
	runCmd.Flags().String("compatibility-permission-mode", string(proto.AgentPermissionBypass), "Set non-interactive permission behavior")
	_ = runCmd.Flags().MarkHidden("compatibility-permission-mode")
	runCmd.MarkFlagsMutuallyExclusive("session", "continue")
}

// runNonInteractive executes the agent via the server and streams output
// to stdout.
func runNonInteractive(
	ctx context.Context,
	c *client.Client,
	ws *proto.Workspace,
	prompt, largeModel, smallModel string,
	hideSpinner bool,
	continueSessionID string,
	useLast bool,
	permissionMode proto.AgentPermissionMode,
) error {
	retained := workspace.NewClientWorkspace(c, *ws)
	defer retained.Shutdown()
	return runNonInteractiveWithWorkspace(ctx, c, ws, retained, prompt, largeModel, smallModel, hideSpinner, continueSessionID, useLast, permissionMode, false)
}

func runNonInteractiveWithWorkspace(
	ctx context.Context,
	c *client.Client,
	ws *proto.Workspace,
	retained *workspace.ClientWorkspace,
	prompt, largeModel, smallModel string,
	hideSpinner bool,
	continueSessionID string,
	useLast bool,
	permissionMode proto.AgentPermissionMode,
	modelsPrepared bool,
) error {
	slog.Info("Running in non-interactive mode")

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if !modelsPrepared && (largeModel != "" || smallModel != "") {
		if err := overrideModels(ctx, retained, largeModel, smallModel); err != nil {
			return fmt.Errorf("failed to override models: %w", err)
		}
	}

	var sess *proto.Session
	if continueSessionID != "" || useLast {
		var err error
		sess, err = resolveSession(ctx, c, ws.ID, continueSessionID, useLast)
		if err != nil {
			return fmt.Errorf("failed to resolve session: %w", err)
		}
		if largeModel == "" && smallModel == "" {
			if _, err := restoreModelFromSession(ctx, c, ws.ID, retained, sess.ID); err != nil {
				return fmt.Errorf("failed to restore model from session: %w", err)
			}
		}
		slog.Info("Continuing session for non-interactive run", "session_id", sess.ID)
	}

	if retained.Config() == nil {
		return fmt.Errorf("workspace configuration is missing")
	}
	if err := retained.InitCoderAgentNonInteractive(ctx); err != nil {
		return fmt.Errorf("failed to initialize agent: %w", err)
	}

	var (
		spinner   *format.Spinner
		stderrTTY bool
		progress  bool
	)

	stderrTTY = term.IsTerminal(os.Stderr.Fd())
	cfg := retained.Config()
	progress = cfg.Options == nil || cfg.Options.Progress == nil || *cfg.Options.Progress

	if !hideSpinner && stderrTTY {
		t := styles.ThemeForProvider(cfg.Models[config.SelectedModelTypeLarge].Provider)

		spinner = format.NewSpinner(ctx, cancel, anim.Settings{
			Size:        10,
			Label:       "Generating",
			GradColorA:  t.WorkingGradFromColor,
			GradColorB:  t.WorkingGradToColor,
			CycleColors: true,
		})
		spinner.Start()
	}

	stopSpinner := func() {
		if !hideSpinner && spinner != nil {
			spinner.Stop()
			spinner = nil
		}
	}

	// Wait for the agent to become ready (MCP init, etc).
	if err := waitForAgent(ctx, c, ws.ID); err != nil {
		stopSpinner()
		return fmt.Errorf("agent not ready: %w", err)
	}

	// Force-update agent models so MCP tools are loaded.
	if err := retained.UpdateAgentModel(ctx, retained.Config().AgentModelState()); err != nil {
		stopSpinner()
		return fmt.Errorf("failed to update agent: %w", err)
	}

	defer stopSpinner()

	if sess == nil {
		var err error
		sess, err = resolveSession(ctx, c, ws.ID, "", false)
		if err != nil {
			return fmt.Errorf("failed to resolve session: %w", err)
		}
		slog.Info("Created session for non-interactive run", "session_id", sess.ID)
	}

	events, err := c.SubscribeEvents(ctx, ws.ID)
	if err != nil {
		return fmt.Errorf("failed to subscribe to events: %w", err)
	}

	// Mint a per-call RunID so we can correlate the terminal
	// RunComplete with *this* SendMessage even if the session was
	// busy and another turn finished first. Without it the stream
	// loop would exit on whichever RunComplete arrived first for
	// the same session and drop the queued prompt's output.
	runID := uuid.New().String()
	if err := c.SendMessageWithPermissionMode(ctx, ws.ID, sess.ID, runID, prompt, permissionMode); err != nil {
		return fmt.Errorf("failed to send message: %w", err)
	}

	stream := &runStream{
		sessionID: sess.ID,
		runID:     runID,
		out:       os.Stdout,
		read:      make(map[string]int),
	}

	// Start herdr integration when running inside a herdr pane.
	hc := herdr.Init()
	hc.SetSessionID(sess.ID)
	defer hc.Close()

	defer func() {
		if progress && stderrTTY {
			_, _ = fmt.Fprintf(os.Stderr, ansi.ResetProgressBar)
		}
		_, _ = fmt.Fprintln(os.Stdout)
	}()

	for {
		if progress && stderrTTY {
			_, _ = fmt.Fprintf(os.Stderr, ansi.SetIndeterminateProgressBar)
		}

		select {
		case ev, ok := <-events:
			if !ok {
				stopSpinner()
				return fmt.Errorf("event stream closed before the run completed")
			}
			if retained.HandleClientRefreshEvent(ctx, ev) {
				continue
			}

			// Forward events to herdr if running inside a herdr pane.
			if hev := herdr.Translate(ev); hev != nil {
				hc.HandleEvent(hev)
			}

			done, err := stream.handle(ev, stopSpinner)
			if err != nil {
				return err
			}
			if done {
				return nil
			}

		case <-ctx.Done():
			stopSpinner()
			return ctx.Err()
		}
	}
}

// runStream tracks the per-message stdout cursor and the
// reconciliation state used by [runNonInteractive] to translate
// streaming SSE events into a final, complete stdout for `crux run`.
// It is split out so the state machine can be exercised in unit tests
// without spinning up the full server/client harness.
//
// runID, when non-empty, is the authoritative correlator for the
// terminal RunComplete event: the stream suppresses live message
// events and only exits on a RunComplete whose RunID matches, so a
// turn that finishes first on the same session (e.g. when our prompt
// was queued behind a busy session) cannot contaminate stdout or
// terminate us prematurely. When empty (older servers, tests that
// don't supply one) the stream falls back to SessionID-only matching
// and live message streaming, which is still correct for the
// single-turn case.
type runStream struct {
	sessionID string
	runID     string
	out       io.Writer
	read      map[string]int
	printed   bool
}

// handle processes one SSE event. Returns done=true when the run
// loop should exit (RunComplete observed); returns an error only
// when the agent run failed (not on context cancel — that path is
// handled by the caller's select). stopSpinner is called on the
// first observable assistant output and on completion; passing nil
// is safe for tests.
func (s *runStream) handle(ev any, stopSpinner func()) (done bool, err error) {
	stop := func() {
		if stopSpinner != nil {
			stopSpinner()
		}
	}
	switch e := ev.(type) {
	case pubsub.Event[proto.Message]:
		msg := e.Payload
		if msg.SessionID != s.sessionID || msg.Role != proto.Assistant || len(msg.Parts) == 0 {
			return false, nil
		}
		if s.runID != "" {
			return false, nil
		}
		stop()

		content := msg.Content().String()
		readBytes := s.read[msg.ID]
		if len(content) < readBytes {
			slog.Error("Non-interactive: message content shorter than read bytes",
				"message_length", len(content), "read_bytes", readBytes)
			return false, fmt.Errorf("message content is shorter than read bytes: %d < %d", len(content), readBytes)
		}

		part := content[readBytes:]
		if readBytes == 0 {
			part = strings.TrimLeft(part, " \t")
		}
		if s.printed || strings.TrimSpace(part) != "" {
			s.printed = true
			fmt.Fprint(s.out, part)
		}
		s.read[msg.ID] = len(content)
		return false, nil

	case pubsub.Event[proto.RunComplete]:
		// RunComplete is the authoritative end-of-run signal. We
		// exit on it instead of guessing from message finish parts,
		// which fire on every tool-call step too and were the
		// source of the regression where `crux run` exited
		// mid-turn on finish.reason == tool_use.
		//
		// Correlation:
		//   - if we minted a RunID for this SendMessage, only the
		//     event whose RunID matches is ours; any other turn
		//     finishing first on the same session (busy-session
		//     queue path) must be ignored.
		//   - if we have no RunID (older server, tests), fall back
		//     to SessionID matching.
		if s.runID != "" {
			if e.Payload.RunID != s.runID {
				return false, nil
			}
		} else if e.Payload.SessionID != s.sessionID {
			return false, nil
		}
		stop()
		if e.Payload.Error != "" && !e.Payload.Cancelled {
			return true, fmt.Errorf("agent run failed: %s", e.Payload.Error)
		}
		// Reconcile stdout against the authoritative final
		// assistant text carried in the event. The pubsub fan-in
		// does not serialize publishes across upstream brokers, so
		// the final message event may not have reached this loop
		// yet; the embedded Text field is the backstop that
		// guarantees the full final text always appears on stdout.
		if e.Payload.MessageID != "" {
			full := e.Payload.Text
			readBytes := s.read[e.Payload.MessageID]
			if readBytes < len(full) {
				tail := full[readBytes:]
				if readBytes == 0 {
					tail = strings.TrimLeft(tail, " \t")
				}
				if s.printed || strings.TrimSpace(tail) != "" {
					s.printed = true
					fmt.Fprint(s.out, tail)
				}
			}
		}
		return true, nil

	case pubsub.Event[proto.AgentEvent]:
		if e.Payload.Error == nil {
			return false, nil
		}
		// Attribute the error to our run before treating it as
		// fatal. Async errors from an unrelated workspace run share
		// this channel, so a foreign failure must not abort us:
		//   - if the event carries a RunID, it is the authoritative
		//     correlator: it must match our run exactly, otherwise it
		//     belongs to a different request and we ignore it.
		//   - if the event carries no RunID (older server), fall back
		//     to SessionID: it must be present and match our session,
		//     otherwise we ignore it.
		if e.Payload.RunID != "" {
			if e.Payload.RunID != s.runID {
				return false, nil
			}
		} else if e.Payload.SessionID == "" || e.Payload.SessionID != s.sessionID {
			return false, nil
		}
		stop()
		return true, fmt.Errorf("agent error: %w", e.Payload.Error)
	}
	return false, nil
}

// waitForAgent polls GetAgentInfo until the agent is ready, with a
// timeout.
func waitForAgent(ctx context.Context, c *client.Client, wsID string) error {
	timeout := time.After(30 * time.Second)
	for {
		info, err := c.GetAgentInfo(ctx, wsID)
		if err == nil && info.IsReady {
			return nil
		}
		select {
		case <-timeout:
			if err != nil {
				return fmt.Errorf("timeout waiting for agent: %w", err)
			}
			return fmt.Errorf("timeout waiting for agent readiness")
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func modelOwnerFromSurfaces(surfaces []providerregistry.Surface, model config.SelectedModel) (providerregistry.RegistrationOwner, error) {
	surface, ok := providerregistry.LookupSurface(surfaces, model.Provider)
	if !ok || !surface.Available || surface.Owner == nil {
		return providerregistry.RegistrationOwner{}, fmt.Errorf("provider owner is unavailable for %s", model.Provider)
	}
	if surface.Owner.ProviderID != model.Provider {
		return providerregistry.RegistrationOwner{}, fmt.Errorf("provider owner %s does not match model provider %s", surface.Owner.ProviderID, model.Provider)
	}
	return *surface.Owner, nil
}

// overrideModels resolves against the owning workspace and applies one
// transient selection. The caller rebuilds from the acknowledged config.
func overrideModels(ctx context.Context, retained *workspace.ClientWorkspace, largeModel, smallModel string) error {
	cfg := retained.Config()
	if cfg == nil {
		return fmt.Errorf("failed to get config: workspace configuration is missing")
	}
	requested, err := resolveModelOverrides(cfg, retained.ProviderSurfaces(), largeModel, smallModel, func(providerID string) (config.SelectedModel, error) {
		return retained.GetDefaultSmallModelContext(ctx, providerID)
	})
	if err != nil {
		return err
	}
	_, err = retained.OverrideModels(ctx, requested)
	return err
}

func resolveModelOverrides(cfg *config.Config, surfaces []providerregistry.Surface, largeModel, smallModel string, defaultSmall func(string) (config.SelectedModel, error)) (config.AgentModelState, error) {
	largeMatches, smallMatches := findModelMatches(cfg, largeModel, smallModel)
	requested := config.AgentModelState{}
	owned := func(selected config.SelectedModel) (*config.OwnedSelectedModel, error) {
		owner, err := modelOwnerFromSurfaces(surfaces, selected)
		if err != nil {
			return nil, err
		}
		return &config.OwnedSelectedModel{Model: selected, Owner: owner}, nil
	}
	if largeModel != "" {
		match, err := validateModelMatches(largeMatches, largeModel, "large")
		if err != nil {
			return config.AgentModelState{}, err
		}
		requested.Large, err = owned(config.SelectedModel{Provider: match.provider, Model: match.modelID})
		if err != nil {
			return config.AgentModelState{}, err
		}
	}
	if smallModel != "" {
		match, err := validateModelMatches(smallMatches, smallModel, "small")
		if err != nil {
			return config.AgentModelState{}, err
		}
		requested.Small, err = owned(config.SelectedModel{Provider: match.provider, Model: match.modelID})
		if err != nil {
			return config.AgentModelState{}, err
		}
	} else if requested.Large != nil {
		selected, err := defaultSmall(requested.Large.Model.Provider)
		if err != nil {
			return config.AgentModelState{}, fmt.Errorf("resolve default small model: %w", err)
		}
		requested.Small, err = owned(selected)
		if err != nil {
			return config.AgentModelState{}, err
		}
	}
	return requested, nil
}

// restoreModelFromSession uses the last assistant model only when no explicit
// CLI model choice was supplied. Unavailable recorded choices fail visibly;
// they cannot silently fall back to the currently selected provider.
func restoreModelFromSession(ctx context.Context, c *client.Client, workspaceID string, retained *workspace.ClientWorkspace, sessionID string) (bool, error) {
	msgs, err := c.ListMessages(ctx, workspaceID, sessionID)
	if err != nil {
		return false, fmt.Errorf("failed to list messages: %w", err)
	}
	var lastAssistant *proto.Message
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == proto.Assistant && !msgs[i].IsSummaryMessage {
			lastAssistant = &msgs[i]
			break
		}
	}
	if lastAssistant == nil || lastAssistant.Provider == "" || lastAssistant.Model == "" {
		return false, nil
	}
	cfg := retained.Config()
	if cfg == nil {
		return false, fmt.Errorf("workspace configuration is missing")
	}
	current := cfg.Models[config.SelectedModelTypeLarge]
	if current.Provider == lastAssistant.Provider && current.Model == lastAssistant.Model {
		return false, nil
	}
	if !cfg.IsModelAvailable(lastAssistant.Provider, lastAssistant.Model) {
		return false, fmt.Errorf("session model %s/%s is unavailable; choose an explicit --model", lastAssistant.Provider, lastAssistant.Model)
	}
	selected := config.SelectedModel{Provider: lastAssistant.Provider, Model: lastAssistant.Model}
	surfaces := retained.ProviderSurfaces()
	owner, err := modelOwnerFromSurfaces(surfaces, selected)
	if err != nil {
		return false, err
	}
	requested := config.AgentModelState{Large: &config.OwnedSelectedModel{Model: selected, Owner: owner}}
	if _, ok := cfg.Models[config.SelectedModelTypeSmall]; !ok {
		small, err := retained.GetDefaultSmallModelContext(ctx, selected.Provider)
		if err != nil {
			return false, fmt.Errorf("resolve restored default small model: %w", err)
		}
		owner, err := modelOwnerFromSurfaces(surfaces, small)
		if err != nil {
			return false, err
		}
		requested.Small = &config.OwnedSelectedModel{Model: small, Owner: owner}
	}
	if _, err := retained.OverrideModels(ctx, requested); err != nil {
		return false, err
	}
	return true, nil
}

type modelMatch struct {
	provider string
	modelID  string
}

// findModelMatches searches providers for matching large/small model
// strings.
func findModelMatches(cfg *config.Config, largeModel, smallModel string) ([]modelMatch, []modelMatch) {
	largeFilter, largeID := parseModelString(largeModel)
	smallFilter, smallID := parseModelString(smallModel)

	var largeMatches, smallMatches []modelMatch
	for name, provider := range cfg.Providers.Seq2() {
		if !cfg.IsProviderAvailable(name) {
			continue
		}
		for _, m := range provider.Models {
			if matchesModel(largeID, largeFilter, m.ID, name) {
				largeMatches = append(largeMatches, modelMatch{provider: name, modelID: m.ID})
			}
			if matchesModel(smallID, smallFilter, m.ID, name) {
				smallMatches = append(smallMatches, modelMatch{provider: name, modelID: m.ID})
			}
		}
	}
	return largeMatches, smallMatches
}

// parseModelString splits "provider/model" into (provider, model) or
// ("", model).
func parseModelString(s string) (string, string) {
	if s == "" {
		return "", ""
	}
	if idx := strings.Index(s, "/"); idx >= 0 {
		return s[:idx], s[idx+1:]
	}
	return "", s
}

// matchesModel returns true if the model ID matches the filter
// criteria.
func matchesModel(wantID, wantProvider, modelID, providerName string) bool {
	if wantID == "" {
		return false
	}
	if wantProvider != "" && wantProvider != providerName {
		return false
	}
	return strings.EqualFold(modelID, wantID)
}

// validateModelMatches ensures exactly one match exists.
func validateModelMatches(matches []modelMatch, modelID, label string) (modelMatch, error) {
	switch {
	case len(matches) == 0:
		return modelMatch{}, fmt.Errorf("%s model %q not found", label, modelID)
	case len(matches) > 1:
		names := make([]string, len(matches))
		for i, m := range matches {
			names[i] = m.provider
		}
		return modelMatch{}, fmt.Errorf(
			"%s model: model %q found in multiple providers: %s. Please specify provider using 'provider/model' format",
			label, modelID, strings.Join(names, ", "),
		)
	}
	return matches[0], nil
}

// resolveSession returns the session to use for a non-interactive run.
// If continueSessionID is set it fetches that session; if useLast is set it
// returns the most recently updated top-level session; otherwise it creates a
// new one.
func resolveSession(ctx context.Context, c *client.Client, wsID, continueSessionID string, useLast bool) (*proto.Session, error) {
	switch {
	case continueSessionID != "":
		sess, err := c.GetSession(ctx, wsID, continueSessionID)
		if err != nil {
			return nil, fmt.Errorf("session not found: %s", continueSessionID)
		}
		if sess.ParentSessionID != "" {
			return nil, fmt.Errorf("cannot continue a child session: %s", continueSessionID)
		}
		return sess, nil

	case useLast:
		sessions, err := c.ListSessions(ctx, wsID)
		if err != nil || len(sessions) == 0 {
			return nil, fmt.Errorf("no sessions found to continue")
		}
		last := sessions[0]
		for _, s := range sessions[1:] {
			if s.UpdatedAt > last.UpdatedAt && s.ParentSessionID == "" {
				last = s
			}
		}
		return &last, nil

	default:
		return c.CreateSession(ctx, wsID, "non-interactive")
	}
}

// resolveSessionByID resolves a session ID that may be a full UUID or a hash
// prefix returned by crux session list.
func resolveSessionByID(ctx context.Context, c *client.Client, wsID, id string) (*proto.Session, error) {
	if sess, err := c.GetSession(ctx, wsID, id); err == nil {
		return sess, nil
	}

	sessions, err := c.ListSessions(ctx, wsID)
	if err != nil {
		return nil, err
	}

	var matches []proto.Session
	for _, s := range sessions {
		hash := session.HashID(s.ID)
		if hash == id || strings.HasPrefix(hash, id) {
			matches = append(matches, s)
		}
	}

	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("session %q not found", id)
	case 1:
		return &matches[0], nil
	default:
		return nil, fmt.Errorf("session ID %q is ambiguous (%d matches)", id, len(matches))
	}
}
