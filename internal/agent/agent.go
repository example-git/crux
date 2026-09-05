// Package agent is the core orchestration layer for Crux AI agents.
//
// It provides session-based AI agent functionality for managing
// conversations, tool execution, and message handling. It coordinates
// interactions between language models, messages, sessions, and tools while
// handling features like automatic summarization, queuing, and token
// management.
package agent

import (
	"cmp"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/charmtone"
	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/agent/notify"
	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/agent/tools/mcp"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/imageattachment"
	"github.com/example-git/crux/internal/message"
	codexresponses "github.com/example-git/crux/internal/oauth/codex/responses"
	"github.com/example-git/crux/internal/permission"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/example-git/crux/internal/session"
	"github.com/example-git/crux/internal/stringext"
	"github.com/example-git/crux/internal/version"
)

const (
	DefaultSessionName = "Untitled Session"

	// Constants for auto-summarization thresholds
	largeContextWindowThreshold = 200_000
	largeContextWindowBuffer    = 20_000
	smallContextWindowRatio     = 0.2
	retainedImageHistoryTurns   = 3
	omittedImageHistoryMessage  = "[Earlier image omitted from model history]"
)

var userAgent = fmt.Sprintf("Crux/%s (https://github.com/example-git/crux)", version.Version)

//go:embed templates/title.md
var titlePrompt []byte

//go:embed templates/summary.md
var summaryPrompt []byte

//go:embed templates/suggest_prompt.md
var suggestPromptTemplate []byte

// Used to remove <think> tags from generated titles.
var (
	thinkTagRegex        = regexp.MustCompile(`(?s)<think>.*?</think>`)
	orphanThinkTagRegex  = regexp.MustCompile(`</?think>`)
	summaryAnalysisRegex = regexp.MustCompile(`(?s)<analysis>.*?</analysis>`)
	summaryBlockRegex    = regexp.MustCompile(`(?s)<summary>(.*?)</summary>`)
)

type QueuedPrompt struct {
	SubmissionID string
	Prompt       string
}

type SessionAgentCall struct {
	SessionID    string
	SubmissionID string
	// RunID, when non-empty, is the caller-supplied correlator that
	// gets echoed back on the notify.RunComplete event emitted for
	// this turn. It is preserved when the call is enqueued behind a
	// busy session so the queued turn's terminal event is still
	// recognisable to the original caller. Callers that need a
	// reliable completion contract (e.g. `crux run` against a
	// session that may be busy) MUST set it; SessionID alone is
	// ambiguous when concurrent turns share the same session.
	RunID                string
	Prompt               string
	CodebaseInstructions string
	MemoryInstructions   string
	TurnInstructions     string
	ProviderOptions      fantasy.ProviderOptions
	Attachments          []message.Attachment
	MaxOutputTokens      int64
	Temperature          *float64
	TopP                 *float64
	TopK                 *int64
	FrequencyPenalty     *float64
	PresencePenalty      *float64
	NonInteractive       bool
	// OnComplete, when non-nil, replaces the default RunComplete
	// publish path: the inner Run hands the terminal payload to this
	// callback instead of emitting it on the RunComplete broker. The
	// coordinator uses this hook to coalesce the unauthorized →
	// re-auth → retry chain into a single user-visible terminal
	// event, so non-interactive clients (e.g. `crux run`) don't
	// exit on a stale failed-attempt RunComplete before the
	// successful retry. It is intentionally stripped when queueing
	// a busy-session call (see Run): the originating
	// coordinator.Run has long returned by the time the queued
	// recursion drains, so falling back to the default broker
	// publish keeps the event visible to subscribers.
	OnComplete         func(notify.RunComplete)
	OnMessagePersisted func()
	OnDiscarded        func()
	OnProviderWarning  func(fantasy.CallWarning)
	// Accepted, when non-nil, is the accept reservation taken by
	// BeginAccepted before the call was dispatched onto a goroutine
	// (the client/server fire-and-forget path). Run consumes it under
	// dispatchMu[SessionID] once the accepted -> (cancel-on-entry |
	// queued | active) transition has been chosen. When nil
	// (in-process / local callers like AppWorkspace), behavior is
	// unchanged and no accept tracking applies.
	Accepted *AcceptedRun
	// acceptSeq carries the accept sequence of the handle that produced
	// this call after it has been enqueued and its Accepted handle
	// stripped. The queue-drain paths compare it against a session's
	// cancel mark so a follow-up queued before a cancel is dropped while
	// one queued after the cancel survives. 0 means untracked (an
	// in-process enqueue with no accept reservation), which the drain
	// paths treat as covered by any present mark, preserving the
	// pre-sequence behavior.
	acceptSeq uint64
	runtime   *InstalledRuntime
	// OnAuthRefresh, when non-nil, is called by fantasy when a stream
	// fails with an authentication error (HTTP 401). The callback should
	// refresh credentials and return nil on success, in which case
	// fantasy retries the stream transparently. Returning an error
	// surfaces the original auth error without retry.
	OnAuthRefresh func(ctx context.Context, err *fantasy.ProviderError) error
}

type SessionAgent interface {
	Run(context.Context, SessionAgentCall) (*fantasy.AgentResult, error)
	BeginAccepted(sessionID string) *AcceptedRun
	SetModels(large Model, small Model)
	SetTools(tools, planModeTools []fantasy.AgentTool)
	SetInstructions(instructions fantasy.Instructions)
	SetSystemPrompt(systemPrompt string)
	SetRuntime(runtime InstalledRuntime)
	Runtime() InstalledRuntime
	Cancel(sessionID string)
	CancelAll()
	IsSessionBusy(sessionID string) bool
	IsBusy() bool
	QueuedPrompts(sessionID string) int
	QueuedPromptsList(sessionID string) []QueuedPrompt
	ClearQueue(sessionID string)
	Summarize(context.Context, string, fantasy.ProviderOptions, func(context.Context, *fantasy.ProviderError) error) error
	SummarizeWithRuntime(context.Context, string, fantasy.ProviderOptions, func(context.Context, *fantasy.ProviderError) error, InstalledRuntime) error
	Model() Model
	GenerateTitle(ctx context.Context, sessionID, userPrompt string)
	SuggestPrompt(ctx context.Context, sessionID string) (string, error)
	GenerateMemory(ctx context.Context, purpose, prompt string, maxOutputTokens int64) (string, error)
}

type Model struct {
	Model              fantasy.LanguageModel
	CatalogModel       catalog.Model
	ModelCfg           config.SelectedModel
	FlatRate           bool
	ProviderOptions    fantasy.ProviderOptions
	SystemPromptPrefix string
	InstructionPolicy  fantasy.InstructionPolicy
	ImagePolicy        *imageattachment.Policy
	Compaction         *manifest.CompactionPolicy
	Compactor          RemoteCompactor
	CompactionRetry    *manifest.RetryPolicy
	Retry              *manifest.RetryPolicy
	Metadata           []manifest.MetadataContract
	OnAuthRefresh      func(context.Context, *fantasy.ProviderError) error
}

// activeCancel wraps a context.CancelFunc with a unique pointer identity.
// The pointer is used for compare-and-delete in the dispatch completion path:
// when a finishing run's deferred cleanup fires, it must only remove its own
// entry — not a newer run's entry that was installed in the window between
// the explicit Del and the function return.
type activeCancel struct {
	cancel context.CancelFunc
}

type SystemPromptBuilder func(context.Context, session.Session, Model) (fantasy.Instructions, error)

type InstalledRuntime struct {
	LargeModel           Model
	SmallModel           Model
	Instructions         fantasy.Instructions
	Tools                []fantasy.AgentTool
	PlanModeTools        []fantasy.AgentTool
	DisableAutoSummarize bool
	SystemPromptBuilder  SystemPromptBuilder
	Snapshot             config.RuntimeSnapshot
}

type sessionAgent struct {
	largeModel          *csync.Value[Model]
	smallModel          *csync.Value[Model]
	systemPromptPrefix  *csync.Value[string]
	systemInstructions  *csync.Value[fantasy.Instructions]
	systemPromptBuilder SystemPromptBuilder
	runtimeSnapshot     config.RuntimeSnapshot
	tools               *csync.Slice[fantasy.AgentTool]
	planModeTools       *csync.Slice[fantasy.AgentTool]
	runtimeMu           sync.RWMutex

	isSubAgent           bool
	sessions             session.Service
	messages             message.Service
	disableAutoSummarize bool
	isYolo               bool
	notify               pubsub.Publisher[notify.Notification]
	runComplete          pubsub.Publisher[notify.RunComplete]

	messageQueue       *csync.Map[string, []SessionAgentCall]
	activeRequests     *csync.Map[string, *activeCancel]
	todoReminderMu     sync.Mutex
	todoReminderActive map[string]bool

	// dispatchMu holds a per-session mutex that serializes the
	// accepted -> (cancel-on-entry | queued | active) transition in
	// Run against a concurrent Cancel. The lock is held only during
	// the brief handoff (no DB or LLM I/O under the lock).
	dispatchMu *csync.Map[string, *sync.Mutex]
	// acceptedRuns counts dispatched-but-not-yet-active runs per
	// session. A counter > 0 means a dispatched prompt is in flight
	// and has not yet completed the dispatch handoff in Run. Only
	// BeginAccepted increments it; only AcceptedRun.Close decrements
	// it.
	acceptedRuns *csync.Map[string, int]
	// cancelMark records, per session, a high-water accept sequence: an
	// accepted handle is canceled by it iff the handle's sequence is at
	// or below the mark. Cancel raises the mark to the latest sequence
	// assigned at cancel time, so a single Cancel covers every prompt
	// accepted-but-not-yet-active then, while a prompt accepted later
	// (higher sequence) is never poisoned. Absent or 0 means no pending
	// cancel. It is only raised by Cancel when acceptedRuns > 0, so an
	// idle Escape never records a mark.
	cancelMark *csync.Map[string, uint64]
	// dispatchMuCreate guards lazy creation of per-session entries in
	// dispatchMu so two goroutines can't race to lock different mutex
	// instances for the same session.
	dispatchMuCreate sync.Mutex
	// acceptedMu serializes increments/decrements of acceptedRuns and
	// the assignment of accept sequence numbers from acceptSeqGen. It
	// is separate from dispatchMu so AcceptedRun.Close (which may run
	// while Run holds dispatchMu for the same session) does not
	// deadlock by re-entering the dispatch lock.
	acceptedMu sync.Mutex
	// acceptSeqGen is the monotonic source of accept sequence numbers.
	// Each BeginAccepted increments it under acceptedMu and stamps the
	// returned handle, so sequences strictly increase in accept order
	// across the agent. Cancel uses its current value as the per-session
	// high-water mark.
	acceptSeqGen uint64
}

type SessionAgentOptions struct {
	LargeModel           Model
	SmallModel           Model
	SystemPromptPrefix   string
	Instructions         fantasy.Instructions
	SystemPrompt         string
	SystemPromptBuilder  SystemPromptBuilder
	RuntimeSnapshot      config.RuntimeSnapshot
	IsSubAgent           bool
	DisableAutoSummarize bool
	IsYolo               bool
	Sessions             session.Service
	Messages             message.Service
	Tools                []fantasy.AgentTool
	PlanModeTools        []fantasy.AgentTool
	Notify               pubsub.Publisher[notify.Notification]
	RunComplete          pubsub.Publisher[notify.RunComplete]
}

func NewSessionAgent(
	opts SessionAgentOptions,
) SessionAgent {
	instructions := opts.Instructions
	if instructions.Empty() && opts.SystemPrompt != "" {
		instructions = fantasy.NewInstructions(
			fantasy.StaticInstruction(fantasy.InstructionKindTooling, opts.SystemPrompt),
		)
	}
	return &sessionAgent{
		largeModel:           csync.NewValue(opts.LargeModel),
		smallModel:           csync.NewValue(opts.SmallModel),
		systemPromptPrefix:   csync.NewValue(opts.SystemPromptPrefix),
		systemInstructions:   csync.NewValue(instructions),
		systemPromptBuilder:  opts.SystemPromptBuilder,
		runtimeSnapshot:      opts.RuntimeSnapshot,
		isSubAgent:           opts.IsSubAgent,
		sessions:             opts.Sessions,
		messages:             opts.Messages,
		disableAutoSummarize: opts.DisableAutoSummarize,
		tools:                csync.NewSliceFrom(opts.Tools),
		planModeTools:        csync.NewSliceFrom(opts.PlanModeTools),
		isYolo:               opts.IsYolo,
		notify:               opts.Notify,
		runComplete:          opts.RunComplete,
		messageQueue:         csync.NewMap[string, []SessionAgentCall](),
		activeRequests:       csync.NewMap[string, *activeCancel](),
		todoReminderActive:   make(map[string]bool),
		dispatchMu:           csync.NewMap[string, *sync.Mutex](),
		acceptedRuns:         csync.NewMap[string, int](),
		cancelMark:           csync.NewMap[string, uint64](),
	}
}

// AcceptedRun owns exactly one accept reservation taken by
// BeginAccepted. It is the only carrier of accept-state across the
// backend.runAgent / Coordinator.Run / sessionAgent.Run layers: a
// counter > 0 means a dispatched prompt is in flight and has not yet
// completed the dispatch handoff in Run. Close is the only way to
// release the reservation and is idempotent.
type AcceptedRun struct {
	agent     *sessionAgent
	sessionID string
	// seq is the monotonic accept sequence stamped by BeginAccepted. A
	// cancel covers this handle iff seq is at or below the session's
	// cancel mark, so a handle accepted after a cancel (higher seq) is
	// never poisoned by it.
	seq  uint64
	done atomic.Bool
}

// Close decrements the accept counter for this reservation. It is safe
// to call multiple times; only the first call has effect.
func (r *AcceptedRun) Close() {
	if r == nil {
		return
	}
	if !r.done.CompareAndSwap(false, true) {
		return
	}
	r.agent.endAccepted(r.sessionID)
}

// SessionID exposes the session this reservation is for so the run path
// can use it without an extra parameter.
func (r *AcceptedRun) SessionID() string {
	if r == nil {
		return ""
	}
	return r.sessionID
}

// BeginAccepted increments the accept counter for sessionID and returns
// a handle whose Close is the only way to decrement it. It is the only
// entry point that mutates acceptedRuns.
func (a *sessionAgent) BeginAccepted(sessionID string) *AcceptedRun {
	a.acceptedMu.Lock()
	defer a.acceptedMu.Unlock()
	count, _ := a.acceptedRuns.Get(sessionID)
	a.acceptedRuns.Set(sessionID, count+1)
	a.acceptSeqGen++
	return &AcceptedRun{agent: a, sessionID: sessionID, seq: a.acceptSeqGen}
}

// endAccepted decrements the accept counter for sessionID. It is only
// called via AcceptedRun.Close. It uses a dedicated lock (not the
// per-session dispatch mutex) so it can run while Run holds dispatchMu
// for the same session without deadlocking.
//
// When the count reaches zero the session's cancel mark is dropped: no
// accepted handle remains for it to cover, and any handle accepted later
// gets a strictly higher sequence that the mark would not match anyway.
// Handles canceled on entry never reach RunComplete, so this is the only
// place that clears the mark for an all-canceled batch. Sibling handles
// covered by the same mark are serialized on the per-session dispatch
// mutex and read the mark before they Close, so this never clears it out
// from under a covered handle still waiting to enter Run.
func (a *sessionAgent) endAccepted(sessionID string) {
	a.acceptedMu.Lock()
	defer a.acceptedMu.Unlock()
	count, ok := a.acceptedRuns.Get(sessionID)
	if !ok || count <= 1 {
		a.acceptedRuns.Del(sessionID)
		a.cancelMark.Del(sessionID)
		return
	}
	a.acceptedRuns.Set(sessionID, count-1)
}

// sessionMu returns the per-session dispatch mutex, creating it on first
// use. Creation is guarded so concurrent callers always observe the same
// mutex instance for a given session.
func (a *sessionAgent) sessionMu(sessionID string) *sync.Mutex {
	if mu, ok := a.dispatchMu.Get(sessionID); ok {
		return mu
	}
	a.dispatchMuCreate.Lock()
	defer a.dispatchMuCreate.Unlock()
	if mu, ok := a.dispatchMu.Get(sessionID); ok {
		return mu
	}
	mu := &sync.Mutex{}
	a.dispatchMu.Set(sessionID, mu)
	return mu
}

// enqueueCall appends call to the session's message queue. The
// OnComplete hook is stripped: the caller that supplied it (typically
// coordinator.Run) has its own retry/coalesce scope that ends when it
// returns, so by the time the queue drains nobody is left to consume the
// buffered terminal event. The recursive Run falls back to the default
// broker publish, which is what existing subscribers expect for queued
// turns.
func (a *sessionAgent) enqueueCall(call SessionAgentCall) {
	existing, ok := a.messageQueue.Get(call.SessionID)
	if !ok {
		existing = []SessionAgentCall{}
	}
	queued := call
	if call.Accepted != nil {
		// Preserve the accept sequence after the handle is stripped so
		// the queue-drain paths can tell a follow-up queued before a
		// cancel (covered by the mark) from one queued after it.
		queued.acceptSeq = call.Accepted.seq
	}
	queued.OnComplete = nil
	queued.Accepted = nil
	existing = append(existing, queued)
	a.messageQueue.Set(call.SessionID, existing)
}

// drainQueueForStep partitions the session's queued calls for the current
// streaming step under the per-session dispatch mutex so the filtering is
// atomic against a concurrent Cancel: canceledBySeq requires the caller to
// hold that mutex, and evaluating it here (rather than after unlocking)
// prevents a cancel recorded between the drain and the check from being
// observed inconsistently.
//
// Calls covered by a pending cancel are dropped; the dropped ones that
// carry a RunID are returned in canceledWithRunID so the caller can
// publish their terminal cancelled RunComplete (a caller waiting on that
// RunID, e.g. `crux run`, would otherwise hang). Uncanceled calls without
// a RunID are returned in fold to be folded into the active turn,
// preserving the existing follow-up behavior. Uncanceled calls that carry
// a RunID are left in the queue so each runs as its own turn via the
// recursive run path and publishes its own RunComplete, giving every
// RunID-bearing prompt an explicit lifecycle instead of being silently
// absorbed into another turn. fold is processed by the caller without the
// lock held.
func (a *sessionAgent) drainQueueForStep(sessionID string) (fold, canceledWithRunID []SessionAgentCall) {
	dispatchLock := a.sessionMu(sessionID)
	dispatchLock.Lock()
	defer dispatchLock.Unlock()
	queuedCalls, _ := a.messageQueue.Get(sessionID)
	var keep []SessionAgentCall
	for _, queued := range queuedCalls {
		if a.canceledBySeq(sessionID, queued.acceptSeq) {
			canceledWithRunID = append(canceledWithRunID, queued)
			continue
		}
		if queued.RunID != "" {
			keep = append(keep, queued)
			continue
		}
		fold = append(fold, queued)
	}
	if len(keep) == 0 {
		a.messageQueue.Del(sessionID)
	} else {
		a.messageQueue.Set(sessionID, keep)
	}
	return fold, canceledWithRunID
}

// publishCanceledQueueDrops emits a terminal cancelled RunComplete for
// every dropped queued call that carries a RunID. A queued prompt removed
// from the queue without ever running — covered by a pending cancel, or
// cleared by Cancel/ClearQueue — would otherwise leave a caller blocked on
// that RunID: `crux run` ignores live message events and exits only on a
// RunComplete whose RunID matches. Calls without a RunID had no such waiter
// and are dropped silently as before. A detached, bounded context keeps the
// must-deliver publish alive even when the run context that triggered the
// drop is already canceled.
func (a *sessionAgent) publishCanceledQueueDrops(drops []SessionAgentCall) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, d := range drops {
		if d.OnDiscarded != nil {
			d.OnDiscarded()
		}
		if d.RunID != "" {
			a.publishRunComplete(ctx, d, notify.RunComplete{
				SessionID: d.SessionID,
				RunID:     d.RunID,
				Cancelled: true,
			})
		}
	}
}

// clearQueueAndNotify removes all queued prompts for the session and
// publishes a terminal cancelled RunComplete for any that carried a RunID,
// so callers waiting on those RunIDs (e.g. `crux run`) are not left
// hanging when their queued prompt is discarded without running.
func (a *sessionAgent) clearQueueAndNotify(sessionID string) {
	queued, ok := a.messageQueue.Get(sessionID)
	a.messageQueue.Del(sessionID)
	if !ok {
		return
	}
	a.publishCanceledQueueDrops(queued)
}

// clearPendingCancel removes any pending-cancel mark for sessionID. It
// takes the per-session dispatch lock so it is ordered against Cancel
// and the dispatch handoff.
func (a *sessionAgent) clearPendingCancel(sessionID string) {
	mu := a.sessionMu(sessionID)
	mu.Lock()
	defer mu.Unlock()
	a.cancelMark.Del(sessionID)
}

// canceledBySeq reports whether an accepted handle or queued call with
// the given accept sequence is covered by a pending cancel for the
// session. Callers must hold the session's dispatch mutex. A tracked
// sequence (seq > 0) is covered only when it is at or below the cancel
// high-water mark, so a prompt accepted after the cancel (higher seq) is
// never poisoned. An untracked sequence (seq == 0, an in-process enqueue
// with no accept reservation) is covered whenever any mark is present,
// preserving the pre-sequence behavior. The mark is not consumed: it
// stays so every sibling handle it covers observes the same cancel, and
// a later handle (higher seq) ignores it regardless.
func (a *sessionAgent) canceledBySeq(sessionID string, seq uint64) bool {
	mark, ok := a.cancelMark.Get(sessionID)
	if !ok || mark == 0 {
		return false
	}
	return seq == 0 || seq <= mark
}

// persistCanceledTurn writes the user/assistant records for a turn that
// was canceled before (or just as) streaming would have produced them.
// It creates the user message only when it was not already created by an
// earlier createUserMessage call (userMsgCreated), then writes an
// assistant message with FinishReasonCanceled. Both writes use
// context.WithoutCancel(ctx) so workspace shutdown (which cancels the run
// context) can't drop them.
func (a *sessionAgent) persistCanceledTurn(ctx context.Context, call SessionAgentCall, userMsgCreated bool) error {
	return a.persistCanceledTurnWithModel(ctx, call, userMsgCreated, a.Runtime().LargeModel)
}

func (a *sessionAgent) persistCanceledTurnWithModel(ctx context.Context, call SessionAgentCall, userMsgCreated bool, largeModel Model) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if !userMsgCreated {
		if _, err := a.createUserMessage(writeCtx, call); err != nil {
			return err
		}
	}
	assistant, err := a.messages.Create(writeCtx, call.SessionID, message.CreateMessageParams{
		Role:     message.Assistant,
		Parts:    []message.ContentPart{},
		Model:    largeModel.ModelCfg.Model,
		Provider: largeModel.ModelCfg.Provider,
	})
	if err != nil {
		return err
	}
	assistant.AddFinish(message.FinishReasonCanceled, "User canceled request", "")
	return a.messages.Update(writeCtx, assistant)
}

// publishRunComplete emits the authoritative terminal event for a turn.
// It honors the per-call OnComplete hook when set (so the coordinator can
// coalesce retries) and otherwise falls back to the RunComplete broker.
// ctx is used only for the bounded-blocking must-deliver publish; the
// terminal payload is supplied by the caller. This is the single emit path
// shared by the streaming defer and the cancel-on-entry early return so a
// caller waiting on RunComplete (e.g. `crux run` with a RunID) always
// observes exactly one terminal event regardless of which Run branch ends
// the turn.
func (a *sessionAgent) publishRunComplete(ctx context.Context, call SessionAgentCall, complete notify.RunComplete) {
	if call.OnComplete != nil {
		call.OnComplete(complete)
		return
	}
	if a.runComplete == nil {
		return
	}
	a.runComplete.PublishMustDeliver(ctx, pubsub.UpdatedEvent, complete)
}

// ValidateCall performs the cheap structural validation that
// sessionAgent.Run requires before a call can be dispatched: a call must
// carry either a non-empty prompt or a text attachment, and it must name a
// session. It is exported so callers that accept a run before dispatching it
// (e.g. backend.SendMessage) can apply the same checks and keep the error
// contract consistent.
func ValidateCall(call SessionAgentCall) error {
	if call.Prompt == "" && !message.ContainsTextAttachment(call.Attachments) {
		return ErrEmptyPrompt
	}
	if call.SessionID == "" {
		return ErrSessionMissing
	}
	return nil
}

func replaceSystemInstructions(messages []fantasy.Message, instructions fantasy.Instructions, policy fantasy.InstructionPolicy) []fantasy.Message {
	message := instructions.Message(policy)
	result := make([]fantasy.Message, 0, len(messages)+1)
	if len(message.Content) > 0 {
		result = append(result, message)
	}
	for _, item := range messages {
		if item.Role != fantasy.MessageRoleSystem {
			result = append(result, item)
		}
	}
	return result
}

func appendRuntimeInstructions(base fantasy.Instructions, prefix, mcpInstructions, codebaseInstructions, memoryInstructions, turnInstructions string) fantasy.Instructions {
	return base.Prepend(
		fantasy.StaticInstruction(fantasy.InstructionKindProviderPrefix, prefix),
	).Append(
		fantasy.DynamicInstruction(fantasy.InstructionKindMCP, strings.TrimSpace(mcpInstructions)),
		fantasy.DynamicInstruction(fantasy.InstructionKindRetrieval, strings.TrimSpace(codebaseInstructions)),
		fantasy.DynamicInstruction(fantasy.InstructionKindMemory, strings.TrimSpace(memoryInstructions)),
		fantasy.DynamicInstruction(fantasy.InstructionKindRetrieval, strings.TrimSpace(turnInstructions)),
	)
}

func (a *sessionAgent) runtimeMCPInstructions() string {
	if a.isSubAgent {
		return ""
	}
	return connectedMCPInstructions()
}

func connectedMCPInstructions() string {
	states := mcp.GetStates()
	names := make([]string, 0, len(states))
	for name := range states {
		names = append(names, name)
	}
	slices.Sort(names)
	var instructions strings.Builder
	for _, name := range names {
		server := states[name]
		if server.State != mcp.StateConnected {
			continue
		}
		if value := server.Client.InitializeResult().Instructions; value != "" {
			instructions.WriteString(value)
			instructions.WriteString("\n\n")
		}
	}
	value := strings.TrimSpace(instructions.String())
	if value == "" {
		return ""
	}
	return "<mcp-instructions>\n" + value + "\n</mcp-instructions>"
}

func (a *sessionAgent) Run(ctx context.Context, call SessionAgentCall) (result *fantasy.AgentResult, retErr error) {
	if a.isSubAgent {
		ctx = permission.WithSubagent(ctx)
	}
	if err := ValidateCall(call); err != nil {
		return nil, err
	}
	runtime := a.Runtime()
	if call.runtime != nil {
		runtime = *call.runtime
	}

	// genCtx/cancel are the run context and its cancel func, created under
	// the per-session dispatch mutex below so a concurrent Cancel can observe
	// the activeRequests entry before the assistant message exists.
	var (
		genCtx         context.Context
		cancel         context.CancelFunc
		userMsgCreated bool
	)

	// Serialize the dispatch decision (cancel-on-entry | queued | active)
	// against a concurrent Cancel. Cancel takes the same per-session lock, so
	// every cancel observes at least one of: a cancel mark, an activeRequests
	// entry, or a messageQueue entry it then clears. Holding the lock across
	// the busy check and the active registration also makes them atomic, so
	// two concurrent in-process callers — a burst of channel events, or a
	// channel event racing a typed prompt — cannot both pass the busy check
	// and start two runs on the same session.
	sessMu := a.sessionMu(call.SessionID)
	sessMu.Lock()

	if call.Accepted != nil && a.canceledBySeq(call.SessionID, call.Accepted.seq) {
		// Cancel-on-entry: a cancel arrived while this accepted run was
		// dispatched but not yet active, and this handle's accept sequence
		// is at or below the session's cancel mark. The mark is left in
		// place so sibling handles it also covers observe the same cancel;
		// release the accept reservation, drop the lock, and persist a
		// canceled turn without entering Stream.
		//
		// This path returns before the streaming defer that publishes
		// RunComplete is installed, so emit the terminal event explicitly.
		// Without it, a caller waiting on RunComplete for this RunID (e.g.
		// `crux run`, which ignores message events and blocks on
		// RunComplete) would hang on an immediately-canceled accepted run.
		call.Accepted.Close()
		sessMu.Unlock()
		complete := notify.RunComplete{
			SessionID: call.SessionID,
			RunID:     call.RunID,
			Cancelled: true,
		}
		if err := a.persistCanceledTurnWithModel(ctx, call, false, runtime.LargeModel); err != nil {
			complete.Error = err.Error()
			a.publishRunComplete(ctx, call, complete)
			return nil, err
		}
		a.publishRunComplete(ctx, call, complete)
		return nil, nil
	}

	if a.IsSessionBusy(call.SessionID) {
		// Busy: an earlier prompt is active. Queue this call so it is
		// folded into (or sequenced after) the active turn, and release any
		// accept reservation. A Cancel arriving after this point sees the
		// active entry and clears the queue.
		//
		// enqueueCall strips OnComplete: the caller that supplied the hook
		// (typically coordinator.Run) has its own retry/coalesce scope that
		// ends when it returns, so by the time the queue drains nobody is
		// left to consume the buffered terminal event. The queued turn falls
		// back to the default broker publish, which is what existing
		// subscribers expect.
		a.enqueueCall(call)
		if call.Accepted != nil {
			call.Accepted.Close()
		}
		sessMu.Unlock()
		return nil, nil
	}

	// Idle: become the active run. Register the cancel func before dropping
	// the lock so a Cancel that arrives between here and assistant creation
	// is not lost.
	runCtx := context.WithValue(ctx, tools.SessionIDContextKey, call.SessionID)
	genCtx, cancel = context.WithCancel(runCtx)
	ac := &activeCancel{cancel: cancel}
	a.activeRequests.Set(call.SessionID, ac)
	if call.Accepted != nil {
		call.Accepted.Close()
	}
	sessMu.Unlock()

	defer cancel()
	// Conditional cleanup: only remove our entry if it hasn't been replaced
	// by a newer run. Without this guard, the deferred Del fires after a
	// concurrent run registers in the completion window, silently wiping
	// the new run's cancel and breaking cancellation.
	defer a.activeRequests.CompareAndDelete(call.SessionID, ac)

	// Capture one runtime generation so prompt and callable tools always agree
	// for the full turn, even when configuration reloads concurrently.
	agentTools := runtime.Tools
	planModeTools := runtime.PlanModeTools
	largeModel := runtime.LargeModel
	baseInstructions := runtime.Instructions
	promptPrefix := largeModel.SystemPromptPrefix
	disableAutoSummarize := runtime.DisableAutoSummarize
	agentTools = codexBoundedTools(agentTools, largeModel)
	systemInstructions := appendRuntimeInstructions(
		baseInstructions,
		promptPrefix,
		a.runtimeMCPInstructions(),
		call.CodebaseInstructions,
		call.MemoryInstructions,
		call.TurnInstructions,
	)
	agent := fantasy.NewAgent(
		largeModel.Model,
		fantasy.WithInstructions(systemInstructions, largeModel.InstructionPolicy),
		fantasy.WithTools(agentTools...),
		fantasy.WithUserAgent(userAgent),
	)

	sessionLock := sync.Mutex{}
	currentSession, err := a.sessions.Get(ctx, call.SessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to get session: %w", err)
	}

	msgs, err := a.getSessionMessages(ctx, currentSession)
	if err != nil {
		return nil, fmt.Errorf("failed to get session messages: %w", err)
	}

	// Generate title from the first real (non-shell) user prompt.
	// can take tens of seconds. Blocking Run on it delays the
	// response to the caller. Use a detached context so the title
	// goroutine survives Run's cancel.
	if !a.isSubAgent && !hasUserTextMessage(msgs) {
		titleCtx := context.WithoutCancel(ctx)
		go a.generateTitleWithRuntime(titleCtx, call.SessionID, call.Prompt, runtime)
	}

	// Add the user message to the session.
	_, err = a.createUserMessage(ctx, call)
	if err != nil {
		return nil, err
	}
	userMsgCreated = true

	// Add the session to the context. The run context (genCtx) and its
	// cancel func were already created and registered under the dispatch
	// mutex above for both the accepted and in-process paths.
	ctx = context.WithValue(ctx, tools.SessionIDContextKey, call.SessionID)
	// skipRunComplete is set just before the queued-recursion path so
	// the outer Run doesn't publish a RunComplete that would race
	// with — and be superseded by — the recursive call's own
	// RunComplete (each queued user prompt is its own turn and
	// publishes exactly one terminal event).
	var skipRunComplete bool
	// currentAssistant is declared here so the deferred RunComplete
	// publish below can capture the pointer that PrepareStep will
	// later (re)assign for each streaming step. The final assistant
	// message of the turn is the value reachable through this
	// pointer when the defer runs.
	var currentAssistant *message.Message
	// Drain any debounced message updates before returning. message.Service
	// already flushes synchronously on terminal updates, but a defer here
	// guarantees the contract at every Run exit (success, error, panic
	// recovery upstream) without callers needing to know.
	//
	// After the flush completes — meaning all per-message
	// Publish(UpdatedEvent) calls have fired and been buffered into
	// every subscriber's channel — publish the authoritative
	// RunComplete event for this turn. The flush-then-publish order
	// gives well-behaved clients the best chance of seeing the final
	// message event before RunComplete; the embedded Text field
	// reconciles for clients that observe the events out of order
	// (the pubsub broker fan-in does not serialize publishes from
	// different upstream brokers).
	defer func() {
		// Use a context detached from the run context: workspace
		// shutdown cancels ctx before this goroutine returns, but the
		// buffered streaming deltas must still land before the DB is
		// closed. A short timeout bounds the flush.
		flushCtx, flushCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer flushCancel()
		if flushErr := a.messages.FlushAll(flushCtx); flushErr != nil {
			slog.Error("Failed to flush pending message updates after run", "error", flushErr)
		}
		if skipRunComplete {
			return
		}
		complete := notify.RunComplete{SessionID: call.SessionID, RunID: call.RunID}
		if currentAssistant != nil {
			complete.MessageID = currentAssistant.ID
			complete.Text = currentAssistant.Content().String()
		}
		if retErr != nil {
			complete.Error = retErr.Error()
			complete.Cancelled = errors.Is(retErr, context.Canceled)
		} else if ctx.Err() != nil {
			complete.Cancelled = true
		}
		// Prefer the per-call hook when supplied so the coordinator
		// can coalesce retries (e.g. unauthorized → re-auth → retry)
		// into a single user-visible terminal event. The fallback
		// must-deliver publish applies bounded-blocking semantics to
		// the authoritative terminal event so a momentarily-full
		// subscriber channel can't silently drop it and hang
		// non-interactive clients waiting on RunComplete.
		a.publishRunComplete(ctx, call, complete)
	}()

	history, files := a.preparePrompt(msgs, preparePromptOptions{
		SupportsImages:      largeModel.CatalogModel.SupportsImages,
		IncludeTodoReminder: a.shouldIncludeTodoReminder(call.SessionID, len(currentSession.Todos) == 0),
		Attachments:         call.Attachments,
	})

	var stepMessages []fantasy.Message
	var unseenLocalTokens int64
	var shouldSummarize bool
	sanitizedToolCalls := make(map[string]bool)
	// Don't send MaxOutputTokens if 0 — some providers (e.g. LM Studio) reject it
	var maxOutputTokens *int64
	if call.MaxOutputTokens > 0 {
		maxOutputTokens = &call.MaxOutputTokens
	}
	result, err = agent.Stream(genCtx, fantasy.AgentStreamCall{
		Prompt:           message.PromptWithTextAttachments(call.Prompt, call.Attachments),
		Files:            files,
		Messages:         history,
		Headers:          sessionHeaders(call.SessionID, "conversation"),
		ProviderOptions:  call.ProviderOptions,
		MaxRetries:       modelMaxRetries(largeModel),
		MaxOutputTokens:  maxOutputTokens,
		TopP:             call.TopP,
		Temperature:      call.Temperature,
		PresencePenalty:  call.PresencePenalty,
		TopK:             call.TopK,
		FrequencyPenalty: call.FrequencyPenalty,
		PrepareStep: func(callContext context.Context, options fantasy.PrepareStepFunctionOptions) (_ context.Context, prepared fantasy.PrepareStepResult, err error) {
			prepared.Messages = options.Messages
			for i := range prepared.Messages {
				prepared.Messages[i].ProviderOptions = nil
			}

			currentSession, getSessionErr := a.sessions.Get(callContext, call.SessionID)
			if getSessionErr != nil {
				return callContext, prepared, fmt.Errorf("failed to refresh session mode: %w", getSessionErr)
			}
			stepTools := toolsForSessionMode(currentSession.Mode, agentTools, planModeTools)
			prepared.Tools = codexBoundedTools(stepTools, largeModel)

			// Drain queued follow-up prompts for this step. Calls covered
			// by a cancel recorded while they sat in the queue are dropped:
			// a cancel that arrived after a prompt was queued must not let
			// it run as part of this step. Coverage is per-call by accept
			// sequence so a follow-up queued after the cancel (higher seq)
			// is not dropped. A dropped prompt carrying a RunID still gets
			// its terminal cancelled RunComplete so a caller waiting on it
			// does not hang. Uncanceled prompts without a RunID are folded
			// into this turn; uncanceled prompts with a RunID are left
			// queued so each runs as its own turn (with its own
			// RunComplete) via the recursive run path below.
			fold, canceledRunIDs := a.drainQueueForStep(call.SessionID)
			a.publishCanceledQueueDrops(canceledRunIDs)
			for _, queued := range fold {
				userMessage, createErr := a.createUserMessage(callContext, queued)
				if createErr != nil {
					return callContext, prepared, createErr
				}
				prepared.Messages = append(prepared.Messages, userMessage.ToAIMessage()...)
			}

			prepared.Messages, err = a.workaroundProviderMediaLimitations(prepared.Messages, largeModel)
			if err != nil {
				return callContext, prepared, err
			}

			stepInstructions := baseInstructions
			if runtime.SystemPromptBuilder != nil {
				var buildErr error
				stepInstructions, buildErr = runtime.SystemPromptBuilder(callContext, currentSession, largeModel)
				if buildErr != nil {
					return callContext, prepared, fmt.Errorf("failed to rebuild system prompt: %w", buildErr)
				}
			}
			stepInstructions = appendRuntimeInstructions(
				stepInstructions,
				promptPrefix,
				a.runtimeMCPInstructions(),
				call.CodebaseInstructions,
				call.MemoryInstructions,
				call.TurnInstructions,
			)
			prepared.Messages = replaceSystemInstructions(prepared.Messages, stepInstructions, largeModel.InstructionPolicy)

			sessionLock.Lock()
			stepMessages = cloneFantasyMessages(prepared.Messages)
			sessionLock.Unlock()

			var assistantMsg message.Message
			assistantMsg, err = a.messages.Create(callContext, call.SessionID, message.CreateMessageParams{
				Role:     message.Assistant,
				Parts:    []message.ContentPart{},
				Model:    largeModel.ModelCfg.Model,
				Provider: largeModel.ModelCfg.Provider,
			})
			if err != nil {
				return callContext, prepared, err
			}
			callContext = context.WithValue(callContext, tools.MessageIDContextKey, assistantMsg.ID)
			callContext = context.WithValue(callContext, tools.SupportsImagesContextKey, largeModel.CatalogModel.SupportsImages)
			callContext = context.WithValue(callContext, tools.ModelNameContextKey, largeModel.CatalogModel.Name)
			currentAssistant = &assistantMsg
			return callContext, prepared, err
		},
		OnReasoningStart: func(id string, reasoning fantasy.ReasoningContent) error {
			currentAssistant.AppendReasoningContent(reasoning.Text)
			return a.messages.Update(genCtx, *currentAssistant)
		},
		OnReasoningDelta: func(id string, text string) error {
			currentAssistant.AppendReasoningContent(text)
			return a.messages.Update(genCtx, *currentAssistant)
		},
		OnReasoningEnd: func(id string, reasoning fantasy.ReasoningContent) error {
			opaqueMetadata, metadataErr := message.MetadataFromFantasyMetadata(message.ProviderMetadataScopeReasoning, reasoning.ProviderMetadata)
			if metadataErr != nil {
				return metadataErr
			}
			currentAssistant.SetReasoningProviderMetadata(opaqueMetadata)

			currentAssistant.FinishThinking()
			return a.messages.Update(genCtx, *currentAssistant)
		},
		OnTextDelta: func(id string, text string) error {
			// Strip leading newline from initial text content. This is is
			// particularly important in non-interactive mode where leading
			// newlines are very visible.
			if len(currentAssistant.Parts) == 0 {
				text = strings.TrimPrefix(text, "\n")
			}

			currentAssistant.AppendContent(text)
			return a.messages.Update(genCtx, *currentAssistant)
		},
		OnToolInputStart: func(id string, toolName string) error {
			toolCall := message.ToolCall{
				ID:               id,
				Name:             toolName,
				ProviderExecuted: false,
				Finished:         false,
			}
			currentAssistant.AddToolCall(toolCall)
			// Use parent ctx instead of genCtx to ensure the update succeeds
			// even if the request is canceled mid-stream
			return a.messages.Update(ctx, *currentAssistant)
		},
		OnRetry: func(err *fantasy.ProviderError, delay time.Duration) {
			slog.Warn("Provider request failed, retrying", providerRetryLogFields(err, delay)...)
			// Reset streamed content so the retried response doesn't
			// concatenate with partial content from the failed attempt.
			// On the final attempt (no more retries), any partial content
			// stays in the message as useful context beneath the error.
			currentAssistant.ResetStreamedContent()
			currentAssistant.SetRetrying()
			if updateErr := a.messages.Update(genCtx, *currentAssistant); updateErr != nil {
				slog.Error("Failed to reset message on retry", "error", updateErr)
			}
		},
		OnAuthRefresh: modelAuthRefresh(largeModel, refreshAdmittedModel(&largeModel, func() Model { return a.Runtime().LargeModel }, call.OnAuthRefresh)),
		ModelProvider: func() fantasy.LanguageModel {
			slog.Info("ModelProvider called",
				"provider", largeModel.ModelCfg.Provider,
				"model", largeModel.ModelCfg.Model)
			return largeModel.Model
		},
		OnToolCall: func(tc fantasy.ToolCallContent) error {
			input := tc.Input
			if !tc.ProviderExecuted {
				var wasSanitized bool
				input, wasSanitized = sanitizeToolInput(tc.ToolName, tc.ToolCallID, tc.Input)
				if wasSanitized {
					sanitizedToolCalls[tc.ToolCallID] = true
				}
			}
			opaqueMetadata, metadataErr := message.MetadataFromFantasyMetadata(message.ProviderMetadataScopeToolCall, tc.ProviderMetadata)
			if metadataErr != nil {
				return metadataErr
			}

			toolCall := message.ToolCall{
				ID:               tc.ToolCallID,
				Name:             tc.ToolName,
				Input:            input,
				ProviderExecuted: tc.ProviderExecuted,
				Finished:         true,
				ProviderMetadata: opaqueMetadata,
			}
			currentAssistant.AddToolCall(toolCall)
			// Use parent ctx instead of genCtx to ensure the update succeeds
			// even if the request is canceled mid-stream
			return a.messages.Update(ctx, *currentAssistant)
		},
		OnToolResult: func(result fantasy.ToolResultContent) error {
			toolResult := a.convertToToolResult(result)
			toolResult.Content = toolResultContentForModel(largeModel, toolResult.Content)
			if !result.ProviderExecuted && sanitizedToolCalls[result.ToolCallID] {
				toolResult.Content = "Tool call failed: arguments were not valid JSON. Please check your tool call format and try again."
				toolResult.IsError = true
			}
			if result.ProviderExecuted {
				currentAssistant.AddToolResult(toolResult)
				return a.messages.Update(ctx, *currentAssistant)
			}
			// Use parent ctx instead of genCtx to ensure the message is created
			// even if the request is canceled mid-stream
			_, createMsgErr := a.messages.Create(ctx, currentAssistant.SessionID, message.CreateMessageParams{
				Role: message.Tool,
				Parts: []message.ContentPart{
					toolResult,
				},
			})
			return createMsgErr
		},
		OnStepFinish: func(stepResult fantasy.StepResult) error {
			for _, w := range stepResult.Warnings {
				slog.Warn("Provider warning", "type", w.Type, "message", w.Message)
				if call.OnProviderWarning != nil {
					call.OnProviderWarning(w)
				}
			}
			finishReason := message.FinishReasonUnknown
			switch stepResult.FinishReason {
			case fantasy.FinishReasonLength:
				finishReason = message.FinishReasonMaxTokens
			case fantasy.FinishReasonStop:
				finishReason = message.FinishReasonEndTurn
			case fantasy.FinishReasonToolCalls:
				finishReason = message.FinishReasonToolUse
			case fantasy.FinishReasonContentFilter:
				// Provider safety classifier stopped the model
				// (Anthropic stop_reason=refusal, OpenAI content_filter).
				// The TUI owns the display copy; we only persist the
				// reason so the UI can show a REFUSED banner.
				finishReason = message.FinishReasonContentFilter
				slog.Warn(
					"Provider content filter stopped the model",
					"session_id", call.SessionID,
					"finish_reason", string(stepResult.FinishReason),
				)
			}
			// If a tool result halted the turn (e.g. a hook halt or a
			// permission denial), the step ends on FinishReasonToolCalls but
			// the model will not be called again. Treat it as the end of the
			// turn so the UI can render the assistant footer.
			if finishReason == message.FinishReasonToolUse {
				for _, tr := range stepResult.Content.ToolResults() {
					if tr.StopTurn {
						finishReason = message.FinishReasonEndTurn
						break
					}
				}
			}
			responseMetadata, metadataErr := stepProviderMetadata(largeModel.Metadata, stepResult.ProviderMetadata)
			if metadataErr != nil {
				return metadataErr
			}
			currentAssistant.SetMessageProviderMetadata(responseMetadata)
			currentAssistant.AddFinish(finishReason, "", "")
			sessionLock.Lock()
			defer sessionLock.Unlock()

			updatedSession, getSessionErr := a.sessions.Get(ctx, call.SessionID)
			if getSessionErr != nil {
				return getSessionErr
			}
			usage, estimated := fallbackStepUsage(stepMessages, stepResult)
			a.updateSessionUsage(largeModel, &updatedSession, usage, nil, estimated)
			_, sessionErr := a.sessions.Save(ctx, updatedSession)
			if sessionErr != nil {
				return sessionErr
			}
			currentSession = updatedSession
			unseenLocalTokens = estimateUnseenToolResultTokens(stepResult)
			return a.messages.Update(genCtx, *currentAssistant)
		},
		StopWhen: []fantasy.StopCondition{
			func(_ []fantasy.StepResult) bool {
				cw := int64(largeModel.CatalogModel.ContextWindow)
				retainedTokenBudget := int64(0)
				compactionDisabled := disableAutoSummarize
				if largeModel.Compaction != nil {
					switch largeModel.Compaction.Mode {
					case "local-summary", "remote-operation":
						retainedTokenBudget = largeModel.Compaction.RetainedTokenBudget
					case "none":
						compactionDisabled = true
					}
				}
				triggered, threshold := shouldAutoCompact(
					cw,
					currentSession.PromptTokens,
					currentSession.CompletionTokens,
					unseenLocalTokens,
					retainedTokenBudget,
					compactionDisabled,
				)
				slog.Debug("Context occupancy checked",
					"provider", largeModel.ModelCfg.Provider,
					"model", largeModel.ModelCfg.Model,
					"prompt_tokens", currentSession.PromptTokens,
					"completion_tokens", currentSession.CompletionTokens,
					"unseen_local_tokens", unseenLocalTokens,
					"context_window", cw,
					"threshold", threshold,
					"triggered", triggered,
				)
				if triggered {
					shouldSummarize = true
					return true
				}
				return false
			},
			func(steps []fantasy.StepResult) bool {
				return hasRepeatedToolCalls(steps, loopDetectionWindowSize, loopDetectionMaxRepeats)
			},
		},
	})
	if err != nil {
		isCancelErr := errors.Is(err, context.Canceled)
		slog.Info("Agent stream returned error",
			"error", err.Error(),
			"error_type", fmt.Sprintf("%T", err),
			"is_cancel", isCancelErr)
		if currentAssistant == nil {
			// Cancel-before-assistant-creation window: the run was
			// canceled after activeRequests.Set but before PrepareStep
			// created the assistant message. Without this, the turn
			// would return with no FinishReasonCanceled marker and no
			// user-visible record. The user message was already created
			// above, so persistCanceledTurn only writes the assistant
			// record.
			if isCancelErr {
				if persistErr := a.persistCanceledTurnWithModel(ctx, call, userMsgCreated, runtime.LargeModel); persistErr != nil {
					return nil, persistErr
				}
			}
			return result, err
		}
		// Persist final state with a context detached from the run
		// context. The run context (ctx) is derived from the
		// workspace context, which workspace shutdown cancels before
		// agent goroutines finish; using ctx here would drop the
		// final assistant state. WithoutCancel keeps the values
		// (e.g. session ID) while ignoring cancellation, and a short
		// timeout bounds the cleanup writes.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cleanupCancel()
		// Ensure we finish thinking on error to close the reasoning state.
		currentAssistant.FinishThinking()
		toolCalls := currentAssistant.ToolCalls()
		// INFO: we use the cleanup context here because the genCtx has been cancelled.
		msgs, createErr := a.messages.List(cleanupCtx, currentAssistant.SessionID)
		if createErr != nil {
			return nil, createErr
		}
		for _, tc := range toolCalls {
			if !tc.Finished {
				tc.Finished = true
				tc.Input = "{}"
				currentAssistant.AddToolCall(tc)
				updateErr := a.messages.Update(cleanupCtx, *currentAssistant)
				if updateErr != nil {
					return nil, updateErr
				}
			}

			found := false
			for _, msg := range msgs {
				if msg.Role == message.Tool {
					for _, tr := range msg.ToolResults() {
						if tr.ToolCallID == tc.ID {
							found = true
							break
						}
					}
				}
				if found {
					break
				}
			}
			if found {
				continue
			}
			content := "There was an error while executing the tool"
			if isCancelErr {
				content = "Error: user cancelled assistant tool calling"
			}
			toolResult := message.ToolResult{
				ToolCallID: tc.ID,
				Name:       tc.Name,
				Content:    content,
				IsError:    true,
			}
			_, createErr = a.messages.Create(cleanupCtx, currentAssistant.SessionID, message.CreateMessageParams{
				Role: message.Tool,
				Parts: []message.ContentPart{
					toolResult,
				},
			})
			if createErr != nil {
				return nil, createErr
			}
		}
		var fantasyErr *fantasy.Error
		var providerErr *fantasy.ProviderError
		const defaultTitle = "Provider Error"
		linkStyle := lipgloss.NewStyle().Foreground(charmtone.Guac).Underline(true)
		if isCancelErr {
			currentAssistant.AddFinish(message.FinishReasonCanceled, "User canceled request", "")
		} else if errors.As(err, &providerErr) {
			if providerErr.Message == "The requested model is not supported." {
				url := "https://github.com/settings/copilot/features"
				link := linkStyle.Hyperlink(url, "id=copilot").Render(url)
				currentAssistant.AddFinish(
					message.FinishReasonError,
					"Copilot model not enabled",
					fmt.Sprintf("%q is not enabled in Copilot. Go to the following page to enable it. Then, wait 5 minutes before trying again. %s", largeModel.CatalogModel.Name, link),
				)
			} else {
				currentAssistant.AddFinish(message.FinishReasonError, cmp.Or(stringext.Capitalize(providerErr.Title), defaultTitle), providerErr.Message)
			}
		} else if errors.As(err, &fantasyErr) {
			currentAssistant.AddFinish(message.FinishReasonError, cmp.Or(stringext.Capitalize(fantasyErr.Title), defaultTitle), fantasyErr.Message)
		} else if fantasy.IsTransportError(err) {
			wrapped := fantasy.NewTransportError(err)
			currentAssistant.AddFinish(message.FinishReasonError, stringext.Capitalize(wrapped.Title), wrapped.Message)
		} else {
			currentAssistant.AddFinish(message.FinishReasonError, defaultTitle, err.Error())
		}
		// Note: we use the cleanup context here because the genCtx has been
		// cancelled.
		updateErr := a.messages.Update(cleanupCtx, *currentAssistant)
		if updateErr != nil {
			return nil, updateErr
		}
		return nil, err
	}

	if shouldSummarize {
		if summarizeErr := a.summarizeWithRuntime(genCtx, call.SessionID, call.ProviderOptions, call.OnAuthRefresh, systemInstructions, ac, runtime); summarizeErr != nil {
			return nil, summarizeErr
		}
		if len(currentAssistant.ToolCalls()) > 0 && a.QueuedPrompts(call.SessionID) == 0 {
			a.enqueueCall(compactionContinuationCall(call))
		}
	}

	// Release active request before publishing the notification.
	// TUI handlers poll IsSessionBusy() and only re-evaluate when a
	// tea.Msg arrives, so the cleanup must precede the notify or
	// subscribers see stale busy state at the moment of receipt.
	a.activeRequests.Del(call.SessionID)
	cancel()

	// Send notification that agent has finished its turn (skip for
	// nested/non-interactive sessions).
	if !call.NonInteractive && a.notify != nil {
		a.notify.Publish(pubsub.CreatedEvent, notify.Notification{
			SessionID:    call.SessionID,
			SessionTitle: currentSession.Title,
			Type:         notify.TypeAgentFinished,
		})
	}

	// Hand off to the next queued prompt (if any) under dispatchMu so
	// the transition from this finished run to the queued run is atomic
	// against a concurrent Cancel. activeRequests for this session was
	// just deleted above, so without the lock there is a window in
	// which the session looks idle and a cancel becomes a no-op that
	// fails to stop the queued prompt. Holding the lock lets us observe
	// a pending cancel recorded against the session and drop the queue
	// instead of running it, and (for the recursion) hand a fresh
	// accept reservation to the dequeued call so acceptedRuns stays > 0
	// across the recursive Run's own dispatch handoff — keeping the
	// session observable to Cancel for the entire transition and
	// closing the dequeue -> re-register window.
	mu := a.sessionMu(call.SessionID)
	mu.Lock()
	queuedMessages, _ := a.messageQueue.Get(call.SessionID)
	if mark, ok := a.cancelMark.Get(call.SessionID); ok && mark > 0 && len(queuedMessages) > 0 {
		// A cancel was recorded for this session (e.g. it arrived while
		// this run was active and follow-ups had been queued). Drop the
		// queued prompts it covers (accept sequence at or below the
		// mark, or untracked); keep any queued after the cancel (higher
		// sequence) so they still run.
		var kept []SessionAgentCall
		var canceledRunIDDrops []SessionAgentCall
		for _, q := range queuedMessages {
			if q.acceptSeq == 0 || q.acceptSeq <= mark {
				canceledRunIDDrops = append(canceledRunIDDrops, q)
				continue
			}
			kept = append(kept, q)
		}
		queuedMessages = kept
		a.messageQueue.Set(call.SessionID, kept)
		// A dropped prompt carrying a RunID must still publish its
		// terminal cancelled RunComplete so a caller waiting on that
		// RunID does not hang.
		a.publishCanceledQueueDrops(canceledRunIDDrops)
	}
	if len(queuedMessages) == 0 {
		// No queued work. Clear the cancel mark only when no accepted
		// run remains in flight that it might still cover; otherwise a
		// sibling prompt (sequence at or below the mark) waiting to
		// enter Run would lose its cancellation. When accepted runs are
		// gone, this also clears a stale mark so it can't catch a
		// future run.
		a.messageQueue.Del(call.SessionID)
		a.acceptedMu.Lock()
		inFlight, _ := a.acceptedRuns.Get(call.SessionID)
		a.acceptedMu.Unlock()
		if inFlight == 0 {
			a.cancelMark.Del(call.SessionID)
		}
		mu.Unlock()
		return result, err
	}
	// There are queued messages, restart the loop. Suppress the outer
	// defer's emit: it would otherwise observe the recursive Run's retErr
	// (named-return clobbering through the return below) against this
	// turn's MessageID/Text and publish a mixed, racing event.
	skipRunComplete = true
	// Decide whether this turn still owes its own terminal RunComplete.
	// Each submitted prompt with a RunID has its own lifecycle, so a turn
	// that is finished and handing off to a *different* queued prompt must
	// publish its own RunComplete here — leaving it to the recursive turn
	// (which carries a different RunID) would hang a caller waiting on
	// this turn's RunID. The exception is the summarize-continuation path,
	// which re-queues this same call (same RunID) to resume after a
	// summary; in that case the eventual terminal turn for this RunID
	// publishes, so publishing now would double-emit.
	outerOwesRunComplete := call.RunID != ""
	if outerOwesRunComplete {
		for _, q := range queuedMessages {
			if q.RunID == call.RunID {
				outerOwesRunComplete = false
				break
			}
		}
	}
	firstQueuedMessage := queuedMessages[0]
	a.messageQueue.Set(call.SessionID, queuedMessages[1:])
	// Reserve a fresh accept for the dequeued prompt before dropping the
	// lock so acceptedRuns > 0 across the handoff into the recursive
	// Run. This closes the window between this dequeue and the recursive
	// Run registering its activeRequests entry: a cancel arriving in
	// that window now records a pending cancel (acceptedRuns > 0) that
	// the recursive Run's accepted path observes as cancel-on-entry.
	firstQueuedMessage.Accepted = a.BeginAccepted(call.SessionID)
	mu.Unlock()
	if outerOwesRunComplete {
		complete := notify.RunComplete{SessionID: call.SessionID, RunID: call.RunID}
		if currentAssistant != nil {
			complete.MessageID = currentAssistant.ID
			complete.Text = currentAssistant.Content().String()
		}
		if ctx.Err() != nil {
			complete.Cancelled = true
		}
		a.publishRunComplete(ctx, call, complete)
	}
	return a.Run(ctx, firstQueuedMessage)
}

func (a *sessionAgent) Summarize(ctx context.Context, sessionID string, opts fantasy.ProviderOptions, onAuthRefresh func(context.Context, *fantasy.ProviderError) error) error {
	return a.SummarizeWithRuntime(ctx, sessionID, opts, onAuthRefresh, a.Runtime())
}

func (a *sessionAgent) SummarizeWithRuntime(ctx context.Context, sessionID string, opts fantasy.ProviderOptions, onAuthRefresh func(context.Context, *fantasy.ProviderError) error, runtime InstalledRuntime) error {
	if err := a.summarizeWithRuntime(ctx, sessionID, opts, onAuthRefresh, fantasy.Instructions{}, nil, runtime); err != nil {
		return err
	}
	queuedMessages, ok := a.messageQueue.Get(sessionID)
	if !ok || len(queuedMessages) == 0 {
		return nil
	}
	firstQueuedMessage := queuedMessages[0]
	a.messageQueue.Set(sessionID, queuedMessages[1:])
	_, err := a.Run(ctx, firstQueuedMessage)
	return err
}

func (a *sessionAgent) summarize(ctx context.Context, sessionID string, opts fantasy.ProviderOptions, onAuthRefresh func(context.Context, *fantasy.ProviderError) error, instructions fantasy.Instructions, replace *activeCancel) error {
	return a.summarizeWithRuntime(ctx, sessionID, opts, onAuthRefresh, instructions, replace, a.Runtime())
}

func (a *sessionAgent) summarizeWithRuntime(ctx context.Context, sessionID string, opts fantasy.ProviderOptions, onAuthRefresh func(context.Context, *fantasy.ProviderError) error, instructions fantasy.Instructions, replace *activeCancel, runtime InstalledRuntime) error {
	genCtx, cancel := context.WithCancel(ctx)
	ac := &activeCancel{cancel: cancel}
	dispatchLock := a.sessionMu(sessionID)
	dispatchLock.Lock()
	active, busy := a.activeRequests.Get(sessionID)
	if (replace == nil && busy) || (replace != nil && active != replace) {
		dispatchLock.Unlock()
		cancel()
		return ErrSessionBusy
	}
	a.activeRequests.Set(sessionID, ac)
	dispatchLock.Unlock()
	defer a.activeRequests.CompareAndDelete(sessionID, ac)
	defer cancel()
	defer func() {
		if flushErr := a.messages.FlushAll(ctx); flushErr != nil {
			slog.Error("Failed to flush pending message updates after summarize", "error", flushErr)
		}
	}()

	largeModel := runtime.LargeModel
	compactionMode := "local-summary"
	var initialCompaction manifest.CompactionPolicy
	var initialCompactionRetry manifest.RetryPolicy
	var initialCompactionMetadata manifest.MetadataContract
	if largeModel.Compaction != nil {
		initialCompaction = *largeModel.Compaction
		compactionMode = initialCompaction.Mode
	}
	switch compactionMode {
	case "none":
		return fmt.Errorf("session compaction is disabled by the selected provider")
	case "local-summary":
	case "remote-operation":
		if largeModel.Compactor == nil {
			return fmt.Errorf("remote compaction executor is unavailable")
		}
		if largeModel.CompactionRetry == nil {
			return fmt.Errorf("remote compaction retry policy is unavailable")
		}
		initialCompactionRetry = cloneRetryPolicy(*largeModel.CompactionRetry)
		var err error
		initialCompactionMetadata, err = compactionMetadataContract(largeModel.Metadata, initialCompaction.MetadataNamespace)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("session compaction mode %q is unsupported", compactionMode)
	}

	if instructions.Empty() {
		instructions = appendRuntimeInstructions(
			runtime.Instructions,
			largeModel.SystemPromptPrefix,
			a.runtimeMCPInstructions(),
			"",
			"",
			"",
		)
	}

	currentSession, err := a.sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to get session: %w", err)
	}
	if err := a.messages.FlushAll(ctx); err != nil {
		return fmt.Errorf("flush messages before summarize: %w", err)
	}
	msgs, err := a.getSessionMessages(ctx, currentSession)
	if err != nil {
		return err
	}
	if len(msgs) == 0 {
		return nil
	}

	aiMsgs, _ := a.preparePrompt(msgs, preparePromptOptions{
		SupportsImages: largeModel.CatalogModel.SupportsImages,
	})
	placeholder, err := a.messages.Create(genCtx, sessionID, message.CreateMessageParams{
		Role:             message.Assistant,
		Model:            largeModel.ModelCfg.Model,
		Provider:         largeModel.ModelCfg.Provider,
		IsSummaryMessage: true,
	})
	if err != nil {
		return fmt.Errorf("creating compaction placeholder: %w", err)
	}
	placeholderCommitted := false
	defer func() {
		if placeholderCommitted {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cleanupCancel()
		if deleteErr := a.messages.Delete(cleanupCtx, placeholder.ID); deleteErr != nil {
			slog.Error("Failed to remove compaction placeholder", "error", deleteErr, "message_id", placeholder.ID, "session_id", sessionID)
		}
	}()

	if compactionMode == "remote-operation" {
		result, err := a.generateRemoteCompaction(
			genCtx,
			sessionID,
			currentSession,
			aiMsgs,
			instructions,
			opts,
			onAuthRefresh,
			runtime,
			&largeModel,
			initialCompaction,
			initialCompactionRetry,
			initialCompactionMetadata,
		)
		if err != nil {
			return err
		}
		if err := validateRemoteCompactionAdmission(initialCompaction, initialCompactionRetry, initialCompactionMetadata, largeModel); err != nil {
			return err
		}
		metadata, err := compactionMetadata(largeModel.Metadata, initialCompaction.MetadataNamespace, result.History)
		if err != nil {
			return err
		}
		summary := strings.TrimSpace(result.Summary)
		if summary == "" {
			return fmt.Errorf("remote compaction returned an empty checkpoint summary")
		}
		if result.UsageAvailable {
			a.updateSessionUsage(largeModel, &currentSession, result.Usage, nil, false)
		}
		currentSession.PromptTokens = result.ActiveInputTokens
		currentSession.CompletionTokens = 0
		currentSession.EstimatedUsage = !result.UsageAvailable
		_, err = a.messages.CommitCompaction(genCtx, message.CommitCompactionParams{
			MessageID: placeholder.ID,
			SessionID: sessionID,
			Parts: []message.ContentPart{
				message.TextContent{Text: summary, ProviderMetadata: metadata},
				message.Finish{Reason: message.FinishReasonEndTurn, Time: time.Now().Unix()},
			},
			PromptTokens:     currentSession.PromptTokens,
			CompletionTokens: currentSession.CompletionTokens,
			Cost:             currentSession.Cost,
			EstimatedUsage:   currentSession.EstimatedUsage,
		})
		if err != nil {
			return err
		}
		placeholderCommitted = true
		result.Finalize()
		return nil
	}

	agent := fantasy.NewAgent(
		largeModel.Model,
		fantasy.WithInstructions(instructions, largeModel.InstructionPolicy),
		fantasy.WithUserAgent(userAgent),
	)
	resp, err := agent.Stream(genCtx, fantasy.AgentStreamCall{
		Prompt:          buildSummaryPrompt(currentSession.Todos),
		Messages:        aiMsgs,
		Headers:         sessionHeaders(sessionID, "summary"),
		ProviderOptions: opts,
		OnAuthRefresh:   modelAuthRefresh(largeModel, refreshAdmittedModel(&largeModel, func() Model { return a.Runtime().LargeModel }, onAuthRefresh)),
		MaxRetries:      modelMaxRetries(largeModel),
		ModelProvider: func() fantasy.LanguageModel {
			return largeModel.Model
		},
		PrepareStep: func(callContext context.Context, options fantasy.PrepareStepFunctionOptions) (_ context.Context, prepared fantasy.PrepareStepResult, err error) {
			prepared.Messages = replaceSystemInstructions(options.Messages, instructions, largeModel.InstructionPolicy)
			prepared.Tools = nil
			return callContext, prepared, nil
		},
	})
	if err != nil {
		return err
	}

	summary, err := formatCompactSummary(allResponseText(resp.Response.Content))
	if err != nil {
		return err
	}
	a.updateSessionUsage(largeModel, &currentSession, resp.TotalUsage, nil, false)
	currentSession.PromptTokens = approxTokenCount(summary)
	currentSession.CompletionTokens = 0
	currentSession.EstimatedUsage = true
	_, err = a.messages.CommitCompaction(genCtx, message.CommitCompactionParams{
		MessageID: placeholder.ID,
		SessionID: sessionID,
		Parts: []message.ContentPart{
			message.TextContent{Text: summary},
			message.Finish{Reason: message.FinishReasonEndTurn, Time: time.Now().Unix()},
		},
		PromptTokens:     currentSession.PromptTokens,
		CompletionTokens: currentSession.CompletionTokens,
		Cost:             currentSession.Cost,
		EstimatedUsage:   currentSession.EstimatedUsage,
	})
	if err != nil {
		return err
	}
	placeholderCommitted = true
	if resetter, ok := largeModel.Model.(interface{ ResetConversationChain(string) }); ok {
		resetter.ResetConversationChain(session.HashID(sessionID))
	}
	return nil
}

// sessionHeaders returns the HTTP headers we use for cache affinity on
// every LLM request for a given session.
//
// We use the session hash is used instead of the raw UUID so the header
// value is deterministic and opaque.
func modelMaxRetries(model Model) *int {
	if model.Retry == nil {
		return nil
	}
	return new(0)
}

func modelAuthRefresh(model Model, callback func(context.Context, *fantasy.ProviderError) error) func(context.Context, *fantasy.ProviderError) error {
	if model.Retry == nil {
		return callback
	}
	if model.Retry.Authentication != "refresh-once" {
		return nil
	}
	return callback
}

func refreshAdmittedModel(admitted *Model, current func() Model, callback func(context.Context, *fantasy.ProviderError) error) func(context.Context, *fantasy.ProviderError) error {
	return refreshAdmittedModelValidated(admitted, current, callback, nil)
}

func refreshAdmittedModelValidated(admitted *Model, current func() Model, callback func(context.Context, *fantasy.ProviderError) error, validate func(Model) error) func(context.Context, *fantasy.ProviderError) error {
	if callback == nil {
		return nil
	}
	return func(ctx context.Context, providerErr *fantasy.ProviderError) error {
		if err := callback(ctx, providerErr); err != nil {
			return err
		}
		refreshed := current()
		if refreshed.ModelCfg.Provider != admitted.ModelCfg.Provider || refreshed.ModelCfg.Model != admitted.ModelCfg.Model {
			return fmt.Errorf("model generation changed during authentication refresh")
		}
		if validate != nil {
			if err := validate(refreshed); err != nil {
				return err
			}
		}
		*admitted = refreshed
		return nil
	}
}

func sessionHeaders(sessionID, purpose string) map[string]string {
	hash := session.HashID(sessionID)
	return map[string]string{
		"x-session-id":       hash,
		"x-session-affinity": hash,
		"x-request-purpose":  purpose,
	}
}

func stepProviderMetadata(contracts []manifest.MetadataContract, metadata fantasy.ProviderMetadata) (message.ProviderMetadata, error) {
	compiled, err := manifest.CompileMetadataContracts(contracts)
	if err != nil {
		return nil, err
	}
	declared := make(map[string]manifest.MetadataContract, len(contracts))
	for _, contract := range contracts {
		declared[contract.Namespace] = contract
	}
	messageMetadata := make(fantasy.ProviderMetadata)
	continuationMetadata := make(fantasy.ProviderMetadata)
	for namespace, value := range metadata {
		contract, ok := declared[namespace]
		if !ok {
			messageMetadata[namespace] = value
			continue
		}
		if err := manifest.ValidateMetadataValue(namespace, compiled[namespace], value); err != nil {
			return nil, fmt.Errorf("provider metadata %q: %w", namespace, err)
		}
		switch contract.Scope {
		case string(message.ProviderMetadataScopeMessage):
			messageMetadata[namespace] = value
		case string(message.ProviderMetadataScopeContinuation):
			continuationMetadata[namespace] = value
		default:
			return nil, fmt.Errorf("provider metadata %q with scope %q cannot cross the message persistence boundary", namespace, contract.Scope)
		}
	}
	result, err := message.MetadataFromFantasyMetadata(message.ProviderMetadataScopeMessage, messageMetadata)
	if err != nil {
		return nil, err
	}
	continuation, err := message.MetadataFromFantasyMetadata(message.ProviderMetadataScopeContinuation, continuationMetadata)
	if err != nil {
		return nil, err
	}
	return append(result, continuation...), nil
}

func (a *sessionAgent) createUserMessage(ctx context.Context, call SessionAgentCall) (message.Message, error) {
	parts := []message.ContentPart{message.TextContent{Text: call.Prompt}}
	var attachmentParts []message.ContentPart
	for _, attachment := range call.Attachments {
		attachmentParts = append(attachmentParts, message.BinaryContent{Path: attachment.FilePath, MIMEType: attachment.MimeType, Data: attachment.Content})
	}
	parts = append(parts, attachmentParts...)
	msg, err := a.messages.Create(ctx, call.SessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: parts,
	})
	if err != nil {
		if call.OnDiscarded != nil {
			call.OnDiscarded()
		}
		return message.Message{}, fmt.Errorf("failed to create user message: %w", err)
	}
	if call.OnMessagePersisted != nil {
		call.OnMessagePersisted()
	}
	return msg, nil
}

func (a *sessionAgent) shouldIncludeTodoReminder(sessionID string, todosEmpty bool) bool {
	if a.isSubAgent {
		return false
	}
	a.todoReminderMu.Lock()
	defer a.todoReminderMu.Unlock()
	if !todosEmpty {
		delete(a.todoReminderActive, sessionID)
		return false
	}
	if a.todoReminderActive[sessionID] {
		return false
	}
	a.todoReminderActive[sessionID] = true
	return true
}

type preparePromptOptions struct {
	SupportsImages      bool
	IncludeTodoReminder bool
	Attachments         []message.Attachment
}

func (a *sessionAgent) preparePrompt(msgs []message.Message, options preparePromptOptions) ([]fantasy.Message, []fantasy.FilePart) {
	var history []fantasy.Message
	if options.IncludeTodoReminder && !a.isSubAgent {
		history = append(history, fantasy.NewUserMessage(
			fmt.Sprintf(
				"<system_reminder>%s</system_reminder>",
				`This is a reminder that your todo list is currently empty. DO NOT mention this to the user explicitly because they are already aware.
If you are working on tasks that would benefit from a todo list please use the "todos" tool to create one.
If not, please feel free to ignore. Again do not mention this message to the user.`,
			),
		))
	}

	type toolCallOwner struct {
		messageIndex     int
		providerExecuted bool
	}
	toolCallOwners := make(map[string]toolCallOwner)
	providerResultIDs := make(map[string]struct{})
	for messageIndex, msg := range msgs {
		if msg.Role == message.Assistant {
			for _, call := range msg.ToolCalls() {
				if _, exists := toolCallOwners[call.ID]; !exists {
					toolCallOwners[call.ID] = toolCallOwner{
						messageIndex:     messageIndex,
						providerExecuted: call.ProviderExecuted,
					}
				}
			}
		}
		for _, result := range msg.ToolResults() {
			if result.ProviderExecuted {
				providerResultIDs[result.ToolCallID] = struct{}{}
			}
		}
	}

	clientResultsByAssistant := make(map[int][]fantasy.Message)
	providerResultsByAssistant := make(map[int][]fantasy.ToolResultPart)
	remainingToolMessages := make(map[int][]fantasy.Message)
	clientResultIDs := make(map[string]struct{})
	for messageIndex, msg := range msgs {
		if msg.Role != message.Tool {
			continue
		}
		for _, aiMsg := range msg.ToAIMessage() {
			remainingParts := make([]fantasy.MessagePart, 0, len(aiMsg.Content))
			for _, part := range aiMsg.Content {
				result, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
				if !ok {
					remainingParts = append(remainingParts, part)
					continue
				}
				owner, known := toolCallOwners[result.ToolCallID]
				if !known {
					slog.Warn(
						"Dropping orphaned tool result with no matching tool call",
						"tool_call_id", result.ToolCallID,
					)
					continue
				}
				_, providerResult := providerResultIDs[result.ToolCallID]
				if result.ProviderExecuted || owner.providerExecuted || providerResult {
					result.ProviderExecuted = true
					providerResultsByAssistant[owner.messageIndex] = append(providerResultsByAssistant[owner.messageIndex], result)
					providerResultIDs[result.ToolCallID] = struct{}{}
					continue
				}
				clientResultsByAssistant[owner.messageIndex] = append(clientResultsByAssistant[owner.messageIndex], fantasy.Message{
					Role:            fantasy.MessageRoleTool,
					Content:         []fantasy.MessagePart{result},
					ProviderOptions: aiMsg.ProviderOptions,
				})
				clientResultIDs[result.ToolCallID] = struct{}{}
			}
			if len(remainingParts) > 0 {
				aiMsg.Content = remainingParts
				remainingToolMessages[messageIndex] = append(remainingToolMessages[messageIndex], aiMsg)
			}
		}
	}

	for messageIndex, msg := range msgs {
		if len(msg.Parts) == 0 {
			continue
		}
		if msg.Role == message.Assistant && len(msg.ToolCalls()) == 0 && msg.Content().Text == "" && msg.ReasoningContent().String() == "" && len(msg.MetadataContent()) == 0 {
			continue
		}
		if msg.Role == message.Tool {
			history = append(history, remainingToolMessages[messageIndex]...)
			continue
		}
		aiMsgs := msg.ToAIMessage()
		if !options.SupportsImages {
			for i := range aiMsgs {
				if aiMsgs[i].Role == fantasy.MessageRoleUser {
					aiMsgs[i].Content = filterFileParts(aiMsgs[i].Content)
				}
			}
		}
		if msg.Role == message.Assistant && len(aiMsgs) > 0 {
			for aiMessageIndex := range aiMsgs {
				for partIndex, part := range aiMsgs[aiMessageIndex].Content {
					call, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part)
					if !ok {
						continue
					}
					if _, providerExecuted := providerResultIDs[call.ToolCallID]; providerExecuted {
						call.ProviderExecuted = true
						aiMsgs[aiMessageIndex].Content[partIndex] = call
					}
				}
			}
			for _, result := range providerResultsByAssistant[messageIndex] {
				aiMsgs[len(aiMsgs)-1].Content = append(aiMsgs[len(aiMsgs)-1].Content, result)
			}
		}
		history = append(history, aiMsgs...)

		if msg.Role == message.Assistant {
			history = append(history, clientResultsByAssistant[messageIndex]...)
			if synthetic, ok := syntheticToolResultsForOrphanedCalls(msg, clientResultIDs, providerResultIDs); ok {
				history = append(history, synthetic)
			}
		}
	}

	var files []fantasy.FilePart
	for _, attachment := range options.Attachments {
		if attachment.IsText() {
			continue
		}
		if !options.SupportsImages {
			continue
		}
		files = append(files, fantasy.FilePart{
			Filename:  attachment.FileName,
			Data:      attachment.Content,
			MediaType: attachment.MimeType,
		})
	}

	return history, files
}

// filterFileParts removes fantasy.FilePart entries from a slice of message
// parts. Used to strip image attachments from historical user messages when
// the current model does not support them.
func filterFileParts(parts []fantasy.MessagePart) []fantasy.MessagePart {
	filtered := make([]fantasy.MessagePart, 0, len(parts))
	for _, part := range parts {
		if _, ok := fantasy.AsMessagePart[fantasy.FilePart](part); ok {
			continue
		}
		filtered = append(filtered, part)
	}
	return filtered
}

func omitAgedImageHistory(messages []fantasy.Message, syntheticUserMessages map[int]struct{}) []fantasy.Message {
	result := slices.Clone(messages)
	laterUserTurns := 0
	for index := len(result) - 1; index >= 0; index-- {
		if result[index].Role != fantasy.MessageRoleUser {
			continue
		}
		if laterUserTurns > retainedImageHistoryTurns {
			filtered := make([]fantasy.MessagePart, 0, len(result[index].Content))
			omitted := false
			for _, part := range result[index].Content {
				file, ok := fantasy.AsMessagePart[fantasy.FilePart](part)
				if ok && strings.HasPrefix(strings.ToLower(file.MediaType), "image/") {
					if !omitted {
						filtered = append(filtered, fantasy.TextPart{Text: omittedImageHistoryMessage})
						omitted = true
					}
					continue
				}
				filtered = append(filtered, part)
			}
			if omitted {
				message := result[index]
				message.Content = filtered
				result[index] = message
			}
		}
		if _, synthetic := syntheticUserMessages[index]; !synthetic {
			laterUserTurns++
		}
	}
	return result
}

func syntheticToolResultsForOrphanedCalls(
	msg message.Message,
	clientResultIDs map[string]struct{},
	providerResultIDs map[string]struct{},
) (fantasy.Message, bool) {
	var syntheticParts []fantasy.MessagePart
	for _, call := range msg.ToolCalls() {
		if call.ProviderExecuted {
			continue
		}
		if _, providerExecuted := providerResultIDs[call.ID]; providerExecuted {
			continue
		}
		if _, hasResult := clientResultIDs[call.ID]; hasResult {
			continue
		}
		slog.Warn(
			"Injecting synthetic tool result for orphaned tool call",
			"tool_call_id", call.ID,
			"tool_name", call.Name,
		)
		syntheticParts = append(syntheticParts, fantasy.ToolResultPart{
			ToolCallID: call.ID,
			Output: fantasy.ToolResultOutputContentError{
				Error: errors.New("tool call was interrupted and did not produce a result, you may retry this call if the result is still needed"),
			},
		})
	}
	if len(syntheticParts) == 0 {
		return fantasy.Message{}, false
	}
	return fantasy.Message{
		Role:    fantasy.MessageRoleTool,
		Content: syntheticParts,
	}, true
}

func auxiliaryInstructions(text string, model Model) fantasy.Instructions {
	return fantasy.NewInstructions(
		fantasy.StaticInstruction(fantasy.InstructionKindAuxiliary, text),
	).Prepend(
		fantasy.StaticInstruction(fantasy.InstructionKindProviderPrefix, model.SystemPromptPrefix),
	)
}

func auxiliaryStreamCall(prompt string, headers map[string]string, model Model, modelProvider func() Model, instructions fantasy.Instructions) fantasy.AgentStreamCall {
	return fantasy.AgentStreamCall{
		Prompt:          prompt,
		Headers:         headers,
		ProviderOptions: model.ProviderOptions,
		OnAuthRefresh:   modelAuthRefresh(model, refreshAdmittedModel(&model, modelProvider, model.OnAuthRefresh)),
		MaxRetries:      modelMaxRetries(model),
		ModelProvider:   func() fantasy.LanguageModel { return model.Model },
		PrepareStep: func(callCtx context.Context, options fantasy.PrepareStepFunctionOptions) (_ context.Context, prepared fantasy.PrepareStepResult, err error) {
			prepared.Messages = replaceSystemInstructions(options.Messages, instructions, model.InstructionPolicy)
			return callCtx, prepared, nil
		},
	}
}

func (a *sessionAgent) GenerateMemory(ctx context.Context, purpose, prompt string, maxOutputTokens int64) (string, error) {
	if purpose != "memory_extraction" && purpose != "memory_consolidation" {
		return "", fmt.Errorf("invalid memory request purpose %q", purpose)
	}
	runtime := a.Runtime()
	smallModel := runtime.SmallModel
	instructions := auxiliaryInstructions("You maintain a private persistent memory index. Follow the requested JSON schema exactly and do not use tools.\n /no_think", smallModel)
	memoryAgent := fantasy.NewAgent(
		smallModel.Model,
		fantasy.WithInstructions(instructions, smallModel.InstructionPolicy),
		fantasy.WithMaxOutputTokens(maxOutputTokens),
		fantasy.WithUserAgent(userAgent),
	)
	response, err := memoryAgent.Stream(ctx, auxiliaryStreamCall(
		prompt,
		sessionHeaders("auto-memory", purpose),
		smallModel,
		func() Model { return a.Runtime().SmallModel },
		instructions,
	))
	if err != nil {
		return "", err
	}
	result := response.Response.Content.Text()
	result = thinkTagRegex.ReplaceAllString(result, "")
	result = orphanThinkTagRegex.ReplaceAllString(result, "")
	return strings.TrimSpace(result), nil
}

// SuggestPrompt predicts the user's likely next message using the
// small model with a minimal call. It returns an empty string (no
// error) when there is no meaningful suggestion.
func (a *sessionAgent) SuggestPrompt(ctx context.Context, sessionID string) (string, error) {
	if a.IsSessionBusy(sessionID) {
		return "", nil
	}

	currentSession, err := a.sessions.Get(ctx, sessionID)
	if err != nil {
		return "", fmt.Errorf("failed to get session: %w", err)
	}
	msgs, err := a.getSessionMessages(ctx, currentSession)
	if err != nil {
		return "", err
	}
	if len(msgs) == 0 {
		return "", nil
	}

	// Compact transcript of the most recent exchanges. Tool noise is
	// skipped; only user/assistant text matters for predicting the
	// next user message.
	const maxTranscriptMessages = 10
	const maxMessageChars = 2000
	if len(msgs) > maxTranscriptMessages {
		msgs = msgs[len(msgs)-maxTranscriptMessages:]
	}
	var transcript strings.Builder
	for _, msg := range msgs {
		text := strings.TrimSpace(msg.Content().Text)
		if text == "" {
			continue
		}
		if len(text) > maxMessageChars {
			text = text[:maxMessageChars] + "…"
		}
		switch msg.Role {
		case message.User:
			transcript.WriteString("User: ")
			transcript.WriteString(text)
			transcript.WriteByte('\n')
		case message.Assistant:
			transcript.WriteString("Assistant: ")
			transcript.WriteString(text)
			transcript.WriteByte('\n')
		}
	}
	if transcript.Len() == 0 {
		return "", nil
	}

	runtime := a.Runtime()
	smallModel := runtime.SmallModel
	instructions := auxiliaryInstructions(string(suggestPromptTemplate)+"\n /no_think", smallModel)

	agent := fantasy.NewAgent(
		smallModel.Model,
		fantasy.WithInstructions(instructions, smallModel.InstructionPolicy),
		fantasy.WithMaxOutputTokens(256),
		fantasy.WithUserAgent(userAgent),
	)

	resp, err := agent.Stream(ctx, auxiliaryStreamCall(
		"Conversation:\n\n"+transcript.String()+"\nPredicted next user message:",
		sessionHeaders(sessionID, "suggestion"),
		smallModel,
		func() Model { return a.Runtime().SmallModel },
		instructions,
	))
	if err != nil {
		return "", err
	}

	suggestion := resp.Response.Content.Text()
	suggestion = thinkTagRegex.ReplaceAllString(suggestion, "")
	suggestion = orphanThinkTagRegex.ReplaceAllString(suggestion, "")
	suggestion = strings.TrimSpace(strings.ReplaceAll(suggestion, "\n", " "))
	if resp.Response.FinishReason == fantasy.FinishReasonLength {
		// Truncated mid-sentence: keep the last complete sentence, if
		// any, rather than showing a dangling fragment.
		if idx := strings.LastIndexAny(suggestion, ".!?"); idx > 0 {
			suggestion = strings.TrimSpace(suggestion[:idx+1])
		} else {
			return "", nil
		}
	}
	return suggestion, nil
}

func (a *sessionAgent) getSessionMessages(ctx context.Context, session session.Session) ([]message.Message, error) {
	if session.SummaryMessageID != "" {
		msgs, err := a.messages.ListFrom(ctx, session.ID, session.SummaryMessageID)
		if err != nil {
			return nil, fmt.Errorf("failed to list messages from summary: %w", err)
		}
		if len(msgs) > 0 {
			msgs[0].Role = message.User
			return msgs, nil
		}
	}

	msgs, err := a.messages.List(ctx, session.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to list messages: %w", err)
	}
	return msgs, nil
}

// hasUserTextMessage reports whether any user message in msgs contains
// text content (as opposed to only shell commands or other non-text parts).
func hasUserTextMessage(msgs []message.Message) bool {
	for _, msg := range msgs {
		if msg.Role != message.User {
			continue
		}
		for _, part := range msg.Parts {
			if tc, ok := part.(message.TextContent); ok && tc.Text != "" {
				return true
			}
		}
	}
	return false
}

// GenerateTitle generates a session title based on the initial prompt.
func (a *sessionAgent) GenerateTitle(ctx context.Context, sessionID string, userPrompt string) {
	a.generateTitleWithRuntime(ctx, sessionID, userPrompt, a.Runtime())
}

func (a *sessionAgent) generateTitleWithRuntime(ctx context.Context, sessionID string, userPrompt string, runtime InstalledRuntime) {
	if userPrompt == "" {
		return
	}

	// Ensure the session always gets a title even if every path below
	// fails or the context is cancelled before we finish.
	var titleSaved bool
	defer func() {
		if !titleSaved {
			fallbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if err := a.sessions.Rename(fallbackCtx, sessionID, DefaultSessionName); err != nil {
				slog.Error("Failed to save fallback session title", "error", err)
			}
		}
	}()

	smallModel := runtime.SmallModel
	largeModel := runtime.LargeModel

	prompt := fmt.Sprintf("Generate a concise title for the following content:\n\n%s\n <think>\n\n</think>", userPrompt)

	type modelAttempt struct {
		name  string
		model Model
	}
	attempts := []modelAttempt{
		{"small", smallModel},
		{"large", largeModel},
	}

	var resp *fantasy.AgentResult
	var err error
	var model Model
	var success bool
	for _, attempt := range attempts {
		tok := int64(40)
		if attempt.model.CatalogModel.CanReason {
			tok = attempt.model.CatalogModel.DefaultMaxTokens
		}
		instructions := auxiliaryInstructions(string(titlePrompt)+"\n /no_think", attempt.model)
		agent := fantasy.NewAgent(
			attempt.model.Model,
			fantasy.WithInstructions(instructions, attempt.model.InstructionPolicy),
			fantasy.WithMaxOutputTokens(tok),
			fantasy.WithUserAgent(userAgent),
		)
		modelProvider := func() Model { return a.Runtime().LargeModel }
		if attempt.name == "small" {
			modelProvider = func() Model { return a.Runtime().SmallModel }
		}
		resp, err = agent.Stream(ctx, auxiliaryStreamCall(
			prompt,
			sessionHeaders(sessionID, "title"),
			attempt.model,
			modelProvider,
			instructions,
		))
		if err == nil && resp.Response.FinishReason != fantasy.FinishReasonLength {
			model = attempt.model
			slog.Debug("Generated title with " + attempt.name + " model")
			success = true
			break
		}
		if err != nil {
			slog.Error("Error generating title with "+attempt.name+" model; trying next", "err", err)
		} else {
			slog.Error("Title generation hit token limit with " + attempt.name + " model; trying next")
		}
	}
	if !success {
		// The deferred fallback will save the default session name.
		return
	}

	// Clean up title.
	var title string
	title = strings.ReplaceAll(resp.Response.Content.Text(), "\n", " ")

	// Remove thinking tags if present.
	title = thinkTagRegex.ReplaceAllString(title, "")
	title = orphanThinkTagRegex.ReplaceAllString(title, "")

	title = strings.TrimSpace(title)
	if title == "" {
		// LLM returned empty content. Use the prompt itself as a
		// fallback title, truncated to 50 chars, before resorting to
		// the generic default.
		fallback := strings.ReplaceAll(userPrompt, "\n", " ")
		fallback = strings.TrimSpace(fallback)
		if len(fallback) > 50 {
			fallback = ansi.Truncate(fallback, 50, "…")
		}
		title = cmp.Or(fallback, DefaultSessionName)
	}

	// Calculate usage and cost.
	modelConfig := model.CatalogModel
	cost := modelConfig.CostPer1MInCached/1e6*float64(resp.TotalUsage.CacheCreationTokens) +
		modelConfig.CostPer1MOutCached/1e6*float64(resp.TotalUsage.CacheReadTokens) +
		modelConfig.CostPer1MIn/1e6*float64(resp.TotalUsage.InputTokens) +
		modelConfig.CostPer1MOut/1e6*float64(resp.TotalUsage.OutputTokens)

	// Skip cost accumulation
	if model.FlatRate {
		cost = 0
	}

	// Atomically update only title and cost fields to avoid overriding other
	// concurrent session updates or active conversation occupancy.
	saveErr := a.sessions.UpdateTitleAndCost(ctx, sessionID, title, cost)
	if saveErr != nil {
		slog.Error("Failed to save session title and cost", "error", saveErr)
		return
	}
	titleSaved = true
}

func shouldAutoCompact(contextWindow, promptTokens, completionTokens, unseenLocalTokens, retainedTokenBudget int64, disabled bool) (bool, int64) {
	if contextWindow == 0 {
		return false, 0
	}
	threshold := retainedTokenBudget
	if threshold == 0 {
		threshold = int64(float64(contextWindow) * smallContextWindowRatio)
		if contextWindow > largeContextWindowThreshold {
			threshold = largeContextWindowBuffer
		}
	}
	remaining := contextWindow - promptTokens - completionTokens - unseenLocalTokens
	return remaining <= threshold && !disabled, threshold
}

func modelUsesCodexRuntime(model Model) bool {
	return model.InstructionPolicy == fantasy.InstructionPolicyCodex
}

func toolResultContentForModel(model Model, content string) string {
	if !modelUsesCodexRuntime(model) {
		return content
	}
	return codexresponses.TruncateToolOutput(model.ModelCfg.Model, content)
}

func (a *sessionAgent) updateSessionUsage(model Model, session *session.Session, usage fantasy.Usage, overrideCost *float64, estimated bool) {
	a.applySessionUsage(model, session, usage, overrideCost, estimated)
}

func (a *sessionAgent) applySessionUsage(model Model, session *session.Session, usage fantasy.Usage, overrideCost *float64, estimated bool) float64 {
	if estimated && modelUsesCodexRuntime(model) && !session.EstimatedUsage && (session.PromptTokens != 0 || session.CompletionTokens != 0) {
		return 0
	}
	if !usageIsZero(usage) {
		session.EstimatedUsage = estimated
	}

	billingUsage := usage
	if modelUsesCodexRuntime(model) && usage.TotalTokens > 0 {
		providerInput := max(usage.TotalTokens-usage.OutputTokens, 0)
		billingUsage.CacheReadTokens = min(max(usage.CacheReadTokens, 0), providerInput)
		billingUsage.InputTokens = providerInput - billingUsage.CacheReadTokens
	}
	modelConfig := model.CatalogModel
	cost := modelConfig.CostPer1MInCached/1e6*float64(billingUsage.CacheCreationTokens) +
		modelConfig.CostPer1MOutCached/1e6*float64(billingUsage.CacheReadTokens) +
		modelConfig.CostPer1MIn/1e6*float64(billingUsage.InputTokens) +
		modelConfig.CostPer1MOut/1e6*float64(billingUsage.OutputTokens)

	if estimated {
		cost = 0
	} else {
		if overrideCost != nil {
			cost = *overrideCost
		}

		if model.FlatRate {
			cost = 0
		}
	}

	session.Cost += cost
	updateSessionTokenCounters(model, session, usage)
	return cost
}

func updateSessionTokenCounters(model Model, session *session.Session, usage fantasy.Usage) {
	if modelUsesCodexRuntime(model) && usage.TotalTokens > 0 {
		session.PromptTokens = max(usage.TotalTokens-usage.OutputTokens, 0)
		session.CompletionTokens = max(usage.OutputTokens, 0)
		return
	}
	if usage.OutputTokens != 0 {
		session.CompletionTokens = usage.OutputTokens
	}
	if promptTokens := usage.InputTokens + usage.CacheReadTokens; promptTokens != 0 {
		session.PromptTokens = promptTokens
	}
}

func summaryCompletionTokens(usage fantasy.Usage, summaryMessage message.Message) int64 {
	if usage.OutputTokens != 0 {
		return usage.OutputTokens
	}
	return approxTokenCount(summaryMessage.Content().Text) + approxTokenCount(summaryMessage.ReasoningContent().String())
}

func (a *sessionAgent) Cancel(sessionID string) {
	// Serialize against the dispatch handoff in Run so the accepted ->
	// (cancel-on-entry | queued | active) transition is atomic against
	// this cancel. Every cancel observes at least one of: an active
	// request, an accepted run (recorded as a pending cancel), or a
	// queue entry it then clears. If none of those hold, an idle Escape
	// is a true no-op and must not poison the next prompt.
	mu := a.sessionMu(sessionID)
	mu.Lock()
	defer mu.Unlock()

	// Cancel regular requests. Don't use Take() here - we need the entry to
	// remain in activeRequests so IsBusy() returns true until the goroutine
	// fully completes (including error handling that may access the DB).
	// The defer in processRequest will clean up the entry.
	if ac, ok := a.activeRequests.Get(sessionID); ok && ac != nil {
		slog.Debug("Request cancellation initiated", "session_id", sessionID)
		ac.cancel()
	}

	// Also check for summarize requests.
	if ac, ok := a.activeRequests.Get(sessionID + "-summarize"); ok && ac != nil {
		slog.Debug("Summarize cancellation initiated", "session_id", sessionID)
		ac.cancel()
	}

	// Record a pending cancel only when a dispatched-but-not-yet-active
	// run exists. This catches runs still in the goroutine scheduler or
	// about to enter Run's busy-queue branch, while leaving an idle
	// session untouched. Active and accepted are not mutually exclusive:
	// when a run is active and a follow-up has been accepted, both the
	// cancel above and this pending record fire.
	//
	// Raise the session's cancel mark to the latest accept sequence
	// assigned so far. Every prompt currently accepted-but-not-yet-
	// active has a sequence at or below that value, so one cancel covers
	// all of them; a prompt accepted after this cancel gets a strictly
	// higher sequence and is never poisoned. Using max keeps repeated
	// cancels idempotent while the same prompts are in flight and lets a
	// later cancel extend coverage to prompts accepted since.
	a.acceptedMu.Lock()
	count, ok := a.acceptedRuns.Get(sessionID)
	mark := a.acceptSeqGen
	a.acceptedMu.Unlock()
	if ok && count > 0 {
		slog.Debug("Recording cancel mark for accepted runs", "session_id", sessionID, "count", count, "mark", mark)
		existing, _ := a.cancelMark.Get(sessionID)
		a.cancelMark.Set(sessionID, max(existing, mark))
	}

	if a.QueuedPrompts(sessionID) > 0 {
		slog.Debug("Clearing queued prompts", "session_id", sessionID)
		a.clearQueueAndNotify(sessionID)
	}
}

func (a *sessionAgent) ClearQueue(sessionID string) {
	if a.QueuedPrompts(sessionID) > 0 {
		slog.Debug("Clearing queued prompts", "session_id", sessionID)
		a.clearQueueAndNotify(sessionID)
	}
}

func (a *sessionAgent) CancelAll() {
	if !a.IsBusy() {
		return
	}
	for key := range a.activeRequests.Seq2() {
		a.Cancel(key) // key is sessionID
	}

	timeout := time.After(5 * time.Second)
	for a.IsBusy() {
		select {
		case <-timeout:
			return
		default:
			time.Sleep(200 * time.Millisecond)
		}
	}
}

func (a *sessionAgent) IsBusy() bool {
	var busy bool
	for ac := range a.activeRequests.Seq() {
		if ac != nil {
			busy = true
			break
		}
	}
	return busy
}

func (a *sessionAgent) IsSessionBusy(sessionID string) bool {
	_, busy := a.activeRequests.Get(sessionID)
	return busy
}

func (a *sessionAgent) QueuedPrompts(sessionID string) int {
	l, ok := a.messageQueue.Get(sessionID)
	if !ok {
		return 0
	}
	return len(l)
}

func (a *sessionAgent) QueuedPromptsList(sessionID string) []QueuedPrompt {
	l, ok := a.messageQueue.Get(sessionID)
	if !ok {
		return nil
	}
	prompts := make([]QueuedPrompt, len(l))
	for i, call := range l {
		prompts[i] = QueuedPrompt{
			SubmissionID: call.SubmissionID,
			Prompt:       call.Prompt,
		}
	}
	return prompts
}

func (a *sessionAgent) SetModels(large Model, small Model) {
	a.runtimeMu.Lock()
	defer a.runtimeMu.Unlock()
	a.largeModel.Set(large)
	a.smallModel.Set(small)
}

func (a *sessionAgent) SetTools(tools, planModeTools []fantasy.AgentTool) {
	a.runtimeMu.Lock()
	defer a.runtimeMu.Unlock()
	a.tools.SetSlice(tools)
	a.planModeTools.SetSlice(planModeTools)
}

func (a *sessionAgent) SetInstructions(instructions fantasy.Instructions) {
	a.runtimeMu.Lock()
	defer a.runtimeMu.Unlock()
	a.systemInstructions.Set(instructions)
}

func (a *sessionAgent) SetSystemPrompt(systemPrompt string) {
	a.SetInstructions(fantasy.NewInstructions(
		fantasy.StaticInstruction(fantasy.InstructionKindTooling, systemPrompt),
	))
}

func (a *sessionAgent) SetRuntime(runtime InstalledRuntime) {
	a.runtimeMu.Lock()
	defer a.runtimeMu.Unlock()
	a.largeModel.Set(runtime.LargeModel)
	a.smallModel.Set(runtime.SmallModel)
	a.systemPromptPrefix.Set(runtime.LargeModel.SystemPromptPrefix)
	a.systemInstructions.Set(runtime.Instructions)
	a.tools.SetSlice(runtime.Tools)
	a.planModeTools.SetSlice(runtime.PlanModeTools)
	a.disableAutoSummarize = runtime.DisableAutoSummarize
	a.systemPromptBuilder = runtime.SystemPromptBuilder
	a.runtimeSnapshot = runtime.Snapshot
}

func (a *sessionAgent) Runtime() InstalledRuntime {
	a.runtimeMu.RLock()
	defer a.runtimeMu.RUnlock()
	return InstalledRuntime{
		LargeModel:           a.largeModel.Get(),
		SmallModel:           a.smallModel.Get(),
		Instructions:         a.systemInstructions.Get(),
		Tools:                a.tools.Copy(),
		PlanModeTools:        a.planModeTools.Copy(),
		DisableAutoSummarize: a.disableAutoSummarize,
		SystemPromptBuilder:  a.systemPromptBuilder,
		Snapshot:             a.runtimeSnapshot,
	}
}

func (a *sessionAgent) Model() Model {
	return a.Runtime().LargeModel
}

// convertToToolResult converts a fantasy tool result to a message tool result.
func (a *sessionAgent) convertToToolResult(result fantasy.ToolResultContent) message.ToolResult {
	opaqueMetadata, err := message.MetadataFromFantasyMetadata(message.ProviderMetadataScopeToolResult, result.ProviderMetadata)
	if err != nil {
		slog.Warn("Failed to preserve tool-result provider metadata", "error", err, "tool_call_id", result.ToolCallID)
	}
	baseResult := message.ToolResult{
		ToolCallID:       result.ToolCallID,
		Name:             result.ToolName,
		Metadata:         result.ClientMetadata,
		ProviderExecuted: result.ProviderExecuted,
		ProviderMetadata: opaqueMetadata,
	}
	if result.Result == nil {
		return baseResult
	}

	switch result.Result.GetType() {
	case fantasy.ToolResultContentTypeText:
		if r, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](result.Result); ok {
			baseResult.Content = r.Text
		}
	case fantasy.ToolResultContentTypeError:
		if r, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](result.Result); ok {
			baseResult.Content = r.Error.Error()
			baseResult.IsError = true
		}
	case fantasy.ToolResultContentTypeMedia:
		if r, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](result.Result); ok {
			if !stringext.IsValidBase64(r.Data) {
				slog.Warn(
					"Tool returned media with invalid base64 data, discarding image",
					"tool", result.ToolName,
					"tool_call_id", result.ToolCallID,
				)
				baseResult.Content = "Tool returned image data with invalid encoding"
				baseResult.IsError = true
			} else {
				content := r.Text
				if content == "" {
					content = fmt.Sprintf("Loaded %s content", r.MediaType)
				}
				baseResult.Content = content
				baseResult.Data = r.Data
				baseResult.MIMEType = r.MediaType
			}
		}
	}

	return baseResult
}

// workaroundProviderMediaLimitations converts media content in tool results to
// user messages for providers that don't natively support images in tool results.
//
// Problem: OpenAI, Google, OpenRouter, and other OpenAI-compatible providers
// don't support sending images/media in tool result messages - they only accept
// text in tool results. However, they DO support images in user messages.
//
// If we send media in tool results to these providers, the API returns an error.
//
// Solution: For these providers, we:
//  1. Replace the media in the tool result with a text placeholder
//  2. Inject a user message immediately after with the image as a file attachment
//  3. This maintains the tool execution flow while working around API limitations
//
// Example transformation:
//
//	BEFORE: [tool result: image data]
//	AFTER:  [tool result: "Image loaded - see attached"], [user: image attachment]
func (a *sessionAgent) workaroundProviderMediaLimitations(messages []fantasy.Message, largeModel Model) ([]fantasy.Message, error) {
	supportsImages := largeModel.CatalogModel.SupportsImages

	convertedMessages := make([]fantasy.Message, 0, len(messages))
	syntheticUserMessages := make(map[int]struct{})
	var pendingMediaFiles []fantasy.FilePart
	flushMediaFiles := func() {
		if len(pendingMediaFiles) == 0 {
			return
		}
		syntheticUserMessages[len(convertedMessages)] = struct{}{}
		convertedMessages = append(convertedMessages, fantasy.NewUserMessage(
			"Here is the media content from the tool result:",
			pendingMediaFiles...,
		))
		pendingMediaFiles = nil
	}

	for _, msg := range messages {
		if msg.Role != fantasy.MessageRoleTool {
			flushMediaFiles()
			convertedMessages = append(convertedMessages, msg)
			continue
		}

		textParts := make([]fantasy.MessagePart, 0, len(msg.Content))
		var mediaFiles []fantasy.FilePart

		for _, part := range msg.Content {
			toolResult, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
			if !ok {
				textParts = append(textParts, part)
				continue
			}

			if media, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](toolResult.Output); ok {
				if !supportsImages {
					// Model cannot process images. Replace with a text
					// placeholder and skip creating a synthetic user
					// message with FilePart, which would brick the
					// session on text-only models.
					text := media.Text
					if text != "" {
						text += "\n\n"
					}
					text += "[Image/media content not supported by this model]"
					textParts = append(textParts, fantasy.ToolResultPart{
						ToolCallID: toolResult.ToolCallID,
						Output: fantasy.ToolResultOutputContentText{
							Text: text,
						},
						ProviderExecuted: toolResult.ProviderExecuted,
						ProviderOptions:  toolResult.ProviderOptions,
						ClientMetadata:   toolResult.ClientMetadata,
					})
					continue
				}

				decoded, err := base64.StdEncoding.DecodeString(media.Data)
				if err != nil {
					slog.Warn("Failed to decode media data", "error", err)
					textParts = append(textParts, part)
					continue
				}

				mediaFiles = append(mediaFiles, fantasy.FilePart{
					Data:      decoded,
					MediaType: media.MediaType,
					Filename:  fmt.Sprintf("tool-result-%s", toolResult.ToolCallID),
				})

				text := media.Text
				if text != "" {
					text += "\n\n"
				}
				text += "[Image/media content loaded - see attached file]"
				textParts = append(textParts, fantasy.ToolResultPart{
					ToolCallID: toolResult.ToolCallID,
					Output: fantasy.ToolResultOutputContentText{
						Text: text,
					},
					ProviderExecuted: toolResult.ProviderExecuted,
					ProviderOptions:  toolResult.ProviderOptions,
					ClientMetadata:   toolResult.ClientMetadata,
				})
			} else {
				textParts = append(textParts, part)
			}
		}

		convertedMessages = append(convertedMessages, fantasy.Message{
			Role:    fantasy.MessageRoleTool,
			Content: textParts,
		})

		pendingMediaFiles = append(pendingMediaFiles, mediaFiles...)
	}
	flushMediaFiles()

	retainedMessages := omitAgedImageHistory(convertedMessages, syntheticUserMessages)
	normalizedMessages, err := normalizeProviderPromptImages(retainedMessages, largeModel.ImagePolicy)
	if err != nil {
		return nil, err
	}
	return normalizedMessages, nil
}

func normalizeProviderPromptImages(messages []fantasy.Message, policy *imageattachment.Policy) ([]fantasy.Message, error) {
	if policy == nil {
		return messages, nil
	}
	result := make([]fantasy.Message, len(messages))
	for messageIndex, promptMessage := range messages {
		result[messageIndex] = promptMessage
		result[messageIndex].Content = append([]fantasy.MessagePart(nil), promptMessage.Content...)
		for partIndex, part := range result[messageIndex].Content {
			file, ok := fantasy.AsMessagePart[fantasy.FilePart](part)
			if !ok || !strings.HasPrefix(strings.ToLower(file.MediaType), "image/") {
				continue
			}
			normalized, err := imageattachment.Normalize(*policy, message.Attachment{
				FileName: file.Filename,
				MimeType: strings.ToLower(file.MediaType),
				Content:  file.Data,
			})
			if err != nil {
				return nil, fmt.Errorf("prepare provider image %q: %w", file.Filename, err)
			}
			file.Filename = normalized.FileName
			file.MediaType = normalized.MimeType
			file.Data = normalized.Content
			result[messageIndex].Content[partIndex] = file
		}
	}
	return result, nil
}

// buildSummaryPrompt constructs the prompt text for session summarization.
func buildSummaryPrompt(todos []session.Todo) string {
	prompt := strings.TrimSpace(string(summaryPrompt))
	if len(todos) == 0 {
		return prompt
	}
	var context strings.Builder
	context.WriteString(prompt)
	context.WriteString("\n\nAdditional Context:\nCurrent todo list:\n")
	for _, todo := range todos {
		fmt.Fprintf(&context, "- [%s] %s\n", todo.Status, todo.Content)
	}
	context.WriteString("Preserve these statuses, but only keep tasks that still follow from the newest user direction.")
	return context.String()
}

// allResponseText preserves provider text items in response order. Summary tags
// can span items, so the first-text-only API can discard the body or closing tag
// and prevent an otherwise valid compaction from being saved.
func allResponseText(response fantasy.ResponseContent) string {
	var text strings.Builder
	for _, content := range response {
		if content.GetType() != fantasy.ContentTypeText {
			continue
		}
		textContent, ok := fantasy.AsContentType[fantasy.TextContent](content)
		if ok {
			text.WriteString(textContent.Text)
		}
	}
	return text.String()
}

func formatCompactSummary(response string) (string, error) {
	withoutAnalysis := summaryAnalysisRegex.ReplaceAllString(response, "")
	match := summaryBlockRegex.FindStringSubmatch(withoutAnalysis)
	if len(match) != 2 || strings.TrimSpace(match[1]) == "" {
		return "", errors.New("summary response did not contain a non-empty <summary> block")
	}
	return "This session is being continued from a previous conversation that ran out of context. The summary below covers the earlier portion of the conversation.\n\nSummary:\n" + strings.TrimSpace(match[1]) + "\n\nContinue from the newest unfinished user request. Do not acknowledge this summary, repeat completed work, or resume superseded tasks.", nil
}

func compactionContinuationCall(call SessionAgentCall) SessionAgentCall {
	return SessionAgentCall{
		SessionID:         call.SessionID,
		Prompt:            "Continue the most recent unfinished task from the compacted context. Follow the newest user request and do not resume superseded work.",
		ProviderOptions:   call.ProviderOptions,
		MaxOutputTokens:   call.MaxOutputTokens,
		Temperature:       call.Temperature,
		TopP:              call.TopP,
		TopK:              call.TopK,
		FrequencyPenalty:  call.FrequencyPenalty,
		PresencePenalty:   call.PresencePenalty,
		NonInteractive:    call.NonInteractive,
		OnProviderWarning: call.OnProviderWarning,
		OnAuthRefresh:     call.OnAuthRefresh,
		runtime:           call.runtime,
	}
}

func providerRetryLogFields(err *fantasy.ProviderError, delay time.Duration) []any {
	fields := []any{
		"retry_delay", delay.String(),
	}
	if err == nil {
		return fields
	}
	fields = append(fields, "status_code", err.StatusCode)
	if err.Title != "" {
		fields = append(fields, "title", err.Title)
	}
	if err.Message != "" {
		fields = append(fields, "message", err.Message)
	}
	return fields
}

// sanitizeToolInput validates tool call JSON from the provider.
// Malformed input is replaced with an empty object to prevent
// stuck conversations from truncated or malformed model output.
// The second return value indicates whether sanitization occurred.
func sanitizeToolInput(toolName, toolCallID, input string) (string, bool) {
	if !json.Valid([]byte(input)) {
		slog.Warn(
			"Malformed tool call JSON from provider, replacing with empty object",
			"tool", toolName,
			"id", toolCallID,
			"input_len", len(input),
		)
		return "{}", true
	}
	return input, false
}
