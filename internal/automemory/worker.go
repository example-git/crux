package automemory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	defaultWorkerTimeout  = 60 * time.Second
	defaultDreamInterval  = 24 * time.Hour
	defaultDreamSessions  = 5
	maxTranscriptTurns    = 12
	maxTranscriptBytes    = 32_000
	maxMutationCount      = 50
	maxMemoryContentBytes = 16_000
	maxDreamInputBytes    = 100_000
)

type Turn struct {
	Role string
	Text string
}

type SessionInfo struct {
	ID        string
	UpdatedAt time.Time
}

type (
	Generator        func(ctx context.Context, purpose, prompt string, maxOutputTokens int64) (string, error)
	TranscriptLoader func(ctx context.Context, sessionID string) ([]Turn, error)
	SessionLoader    func(ctx context.Context) ([]SessionInfo, error)
)

type WorkerOptions struct {
	Memory         Memory
	Generate       Generator
	LoadTranscript TranscriptLoader
	LoadSessions   SessionLoader
	Now            func() time.Time
	Timeout        time.Duration
	DreamInterval  time.Duration
	DreamSessions  int
}

type Worker struct {
	memory         Memory
	generate       Generator
	loadTranscript TranscriptLoader
	loadSessions   SessionLoader
	now            func() time.Time
	timeout        time.Duration
	dreamInterval  time.Duration
	dreamSessions  int

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu           sync.Mutex
	activeCancel context.CancelFunc
	pending      string
	running      bool
	closed       bool
	processed    map[string]string
	lastScan     time.Time
	activity     string
}

type mutationResponse struct {
	Memories []memoryMutation `json:"memories"`
}

type memoryMutation struct {
	File        string `json:"file"`
	Action      string `json:"action"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Type        string `json:"type"`
	Content     string `json:"content"`
}

func NewWorker(options WorkerOptions) (*Worker, error) {
	if options.Memory.Directory == "" || !options.Memory.Managed {
		return nil, nil
	}
	if options.Generate == nil || options.LoadTranscript == nil || options.LoadSessions == nil {
		return nil, fmt.Errorf("auto-memory worker requires generation, transcript, and session services")
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = defaultWorkerTimeout
	}
	dreamInterval := options.DreamInterval
	if dreamInterval <= 0 {
		dreamInterval = defaultDreamInterval
	}
	dreamSessions := options.DreamSessions
	if dreamSessions <= 0 {
		dreamSessions = defaultDreamSessions
	}
	ctx, cancel := context.WithCancel(context.Background())
	worker := &Worker{
		memory:         options.Memory,
		generate:       options.Generate,
		loadTranscript: options.LoadTranscript,
		loadSessions:   options.LoadSessions,
		now:            options.Now,
		timeout:        timeout,
		dreamInterval:  dreamInterval,
		dreamSessions:  dreamSessions,
		ctx:            ctx,
		cancel:         cancel,
		processed:      make(map[string]string),
	}
	if worker.now == nil {
		worker.now = time.Now
	}
	return worker, nil
}

func (w *Worker) Interrupt() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.pending = ""
	if w.activeCancel != nil {
		w.activeCancel()
	}
	w.mu.Unlock()
}

func (w *Worker) Activity() string {
	if w == nil {
		return ""
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.activity
}

func (w *Worker) setActivity(activity string) {
	w.mu.Lock()
	w.activity = activity
	w.mu.Unlock()
}

func (w *Worker) Enqueue(sessionID string) {
	if w == nil || sessionID == "" {
		return
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.pending = sessionID
	if w.running {
		w.mu.Unlock()
		return
	}
	w.running = true
	w.wg.Add(1)
	w.mu.Unlock()
	go w.run()
}

func (w *Worker) Close() {
	if w == nil {
		return
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	w.pending = ""
	w.cancel()
	if w.activeCancel != nil {
		w.activeCancel()
	}
	w.mu.Unlock()

	done := make(chan struct{})
	go func() {
		w.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}

func (w *Worker) run() {
	defer w.wg.Done()
	for {
		w.mu.Lock()
		sessionID := w.pending
		w.pending = ""
		if sessionID == "" || w.closed {
			w.running = false
			w.activeCancel = nil
			w.mu.Unlock()
			return
		}
		ctx, cancel := context.WithTimeout(w.ctx, w.timeout)
		w.activeCancel = cancel
		w.mu.Unlock()

		if err := w.maybeDream(ctx, sessionID); err != nil && ctx.Err() == nil {
			slog.Debug("Auto-memory maintenance failed", "error", err)
		}
		w.setActivity("updating")
		if err := w.extract(ctx, sessionID); err != nil && ctx.Err() == nil {
			slog.Debug("Auto-memory extraction failed", "error", err)
		}
		w.setActivity("")
		if ctx.Err() == nil {
			if err := w.maybeDream(ctx, sessionID); err != nil && ctx.Err() == nil {
				slog.Debug("Auto-memory consolidation failed", "error", err)
			}
		}
		w.setActivity("")
		cancel()

		w.mu.Lock()
		w.activeCancel = nil
		w.mu.Unlock()
	}
}

func (w *Worker) extract(ctx context.Context, sessionID string) error {
	turns, err := w.loadTranscript(ctx, sessionID)
	if err != nil {
		return err
	}
	transcript := boundedTranscript(turns)
	if transcript == "" {
		return nil
	}
	digest := sha256.Sum256([]byte(transcript))
	fingerprint := hex.EncodeToString(digest[:])
	w.mu.Lock()
	if w.processed[sessionID] == fingerprint {
		w.mu.Unlock()
		return nil
	}
	w.mu.Unlock()

	manifest, err := w.manifest()
	if err != nil {
		return err
	}
	relatedMemories, err := w.relatedMemoryContext(transcript)
	if err != nil {
		return err
	}
	prompt := extractionPrompt(manifest, relatedMemories, transcript)
	response, err := w.generate(ctx, "memory_extraction", prompt, 4096)
	if err != nil {
		return err
	}
	mutations, err := parseMutations(response)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := w.apply(mutations); err != nil {
		return err
	}
	w.mu.Lock()
	w.processed[sessionID] = fingerprint
	w.mu.Unlock()
	return nil
}

func (w *Worker) maybeDream(ctx context.Context, currentSessionID string) error {
	topics, err := scanTopics(w.memory.Directory)
	if err != nil {
		return err
	}
	pressure := len(topics) >= ProjectMemorySlots*4/5
	interval := w.dreamInterval
	if pressure {
		interval = min(interval, time.Hour)
	}
	if len(topics) > ProjectMemorySlots {
		interval = min(interval, 10*time.Minute)
	}
	lockPath := filepath.Join(w.memory.Directory, ".consolidate-lock")
	lastConsolidated := time.Time{}
	if info, err := os.Stat(lockPath); err == nil {
		lastConsolidated = info.ModTime()
	} else if !os.IsNotExist(err) {
		return err
	}

	changed := 0
	for _, topic := range topics {
		if topic.ModifiedAt.After(lastConsolidated) {
			changed++
		}
	}
	maintenance := pressure || changed >= w.dreamSessions
	if maintenance {
		interval = min(interval, time.Hour)
	}
	if w.now().Sub(lastConsolidated) < interval {
		return nil
	}
	w.mu.Lock()
	if !w.lastScan.IsZero() && w.now().Sub(w.lastScan) < 10*time.Minute {
		w.mu.Unlock()
		return nil
	}
	w.lastScan = w.now()
	w.mu.Unlock()

	sessions, err := w.loadSessions(ctx)
	if err != nil {
		return err
	}
	var candidates []SessionInfo
	for _, session := range sessions {
		if session.ID != currentSessionID && session.UpdatedAt.After(lastConsolidated) {
			candidates = append(candidates, session)
		}
	}
	if !maintenance && len(candidates) < w.dreamSessions {
		return nil
	}
	slices.SortFunc(candidates, func(left, right SessionInfo) int {
		if left.UpdatedAt.After(right.UpdatedAt) {
			return -1
		}
		if right.UpdatedAt.After(left.UpdatedAt) {
			return 1
		}
		return strings.Compare(left.ID, right.ID)
	})
	if len(candidates) > w.dreamSessions {
		candidates = candidates[:w.dreamSessions]
	}

	_, acquired, err := acquireDreamLock(lockPath, lastConsolidated, w.now())
	if err != nil || !acquired {
		return err
	}
	defer os.Remove(lockPath + ".active")
	w.setActivity("consolidating")
	defer w.setActivity("")

	memoryContext, reviewed, nextTopic, err := w.memoryContext(maxDreamInputBytes / 2)
	if err != nil {
		return err
	}
	var transcriptContext strings.Builder
	for _, session := range candidates {
		turns, loadErr := w.loadTranscript(ctx, session.ID)
		if loadErr != nil {
			continue
		}
		text := boundedTranscript(turns)
		if text == "" {
			continue
		}
		fmt.Fprintf(&transcriptContext, "<session updated=%q>\n%s\n</session>\n", session.UpdatedAt.Format(time.RFC3339), text)
		if transcriptContext.Len() >= maxDreamInputBytes/2 {
			break
		}
	}
	manifest, err := w.manifest()
	if err != nil {
		return err
	}
	prompt := consolidationPrompt(manifest+"\nComplete topic files for this maintenance batch:\n"+memoryContext, truncateUTF8Prefix(transcriptContext.String(), maxDreamInputBytes/2))
	response, err := w.generate(ctx, "memory_consolidation", prompt, 8192)
	if err != nil {
		return err
	}
	mutations, err := parseMutations(response)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := applyMemoryMutations(w.memory, mutations, reviewed); err != nil {
		return err
	}
	if err := atomicWrite(filepath.Join(w.memory.Directory, ".consolidate-next"), []byte(nextTopic), 0o600); err != nil {
		return err
	}
	return commitDreamLock(lockPath, w.now())
}

func (w *Worker) manifest() (string, error) {
	topics, err := scanTopics(w.memory.Directory)
	if err != nil {
		return "", err
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "Project memory slots: %d/%d occupied. Consolidate toward %d topics when near capacity.\n", len(topics), ProjectMemorySlots, ProjectMemorySlots*4/5)
	for _, topic := range topics {
		fmt.Fprintf(&builder, "- %s | type=%s | name=%s | description=%s\n", filepath.Base(topic.Path), topic.Type, topic.Name, topic.Description)
	}
	return builder.String(), nil
}

func (w *Worker) relatedMemoryContext(query string) (string, error) {
	topics, err := scanTopics(w.memory.Directory)
	if err != nil {
		return "", err
	}
	var builder strings.Builder
	for _, topic := range relevantTopics(topics, query, maxRelevantMemories) {
		content, readErr := readTopic(topic.Path)
		if readErr != nil {
			continue
		}
		fmt.Fprintf(&builder, "<memory file=%q>\n%s\n</memory>\n", filepath.Base(topic.Path), content)
	}
	return builder.String(), nil
}

func (w *Worker) memoryContext(limit int) (string, map[string]string, string, error) {
	topics, err := scanTopics(w.memory.Directory)
	if err != nil {
		return "", nil, "", err
	}
	slices.SortFunc(topics, func(left, right Topic) int { return strings.Compare(left.Path, right.Path) })
	cursor, err := os.ReadFile(filepath.Join(w.memory.Directory, ".consolidate-next"))
	if err != nil && !os.IsNotExist(err) {
		return "", nil, "", err
	}
	start := 0
	for i, topic := range topics {
		if filepath.Base(topic.Path) >= string(cursor) {
			start = i
			break
		}
	}
	var builder strings.Builder
	reviewed := make(map[string]string)
	next := ""
	for i := range len(topics) {
		topic := topics[(start+i)%len(topics)]
		info, err := os.Stat(topic.Path)
		if err != nil {
			return "", nil, "", err
		}
		if info.Size() > maxMemoryFileBytes {
			return "", nil, "", fmt.Errorf("memory %s exceeds maintenance file limit", filepath.Base(topic.Path))
		}
		content, err := os.ReadFile(topic.Path)
		if err != nil {
			return "", nil, "", err
		}
		file := filepath.Base(topic.Path)
		block := fmt.Sprintf("<memory file=%q>\n%s\n</memory>\n", file, content)
		if builder.Len()+len(block) > limit {
			next = file
			break
		}
		builder.WriteString(block)
		reviewed[file] = string(content)
	}
	return builder.String(), reviewed, next, nil
}

func (w *Worker) apply(mutations []memoryMutation) error {
	return applyMemoryMutations(w.memory, mutations)
}

func parseMutations(response string) ([]memoryMutation, error) {
	response = strings.TrimSpace(response)
	start := strings.IndexByte(response, '{')
	end := strings.LastIndexByte(response, '}')
	if start < 0 || end < start {
		return nil, fmt.Errorf("memory response did not contain a JSON object")
	}
	var parsed mutationResponse
	if err := json.Unmarshal([]byte(response[start:end+1]), &parsed); err != nil {
		return nil, fmt.Errorf("decode memory response: %w", err)
	}
	return parsed.Memories, nil
}

func boundedTranscript(turns []Turn) string {
	if len(turns) > maxTranscriptTurns {
		turns = turns[len(turns)-maxTranscriptTurns:]
	}
	var builder strings.Builder
	for _, turn := range turns {
		text := strings.TrimSpace(turn.Text)
		if text == "" || (turn.Role != "user" && turn.Role != "assistant") {
			continue
		}
		fmt.Fprintf(&builder, "%s: %s\n", turn.Role, text)
		if builder.Len() >= maxTranscriptBytes {
			break
		}
	}
	result := truncateUTF8Tail(builder.String(), maxTranscriptBytes)
	return strings.TrimSpace(result)
}

func extractionPrompt(manifest, relatedMemories, transcript string) string {
	return "Analyze only the recent conversation below for durable memories. Before proposing any mutation, inspect the target project scope's existing memory manifest and the supplied full content of related memories. If a related memory exists and remains relevant, upsert that same file, preserving its useful details while adding the new durable information instead of creating a duplicate. Delete an existing memory when the conversation establishes that it is no longer relevant, stale, or superseded. Create a new topic only when no related memory exists. Each project has a hard limit of 50 memory slots. Keep MEMORY.md as a compact index of short titles and relevance hooks pointing to detailed topic files. Consolidate related facts in existing topic files instead of creating one memory per fact. At capacity, update existing topics or merge related topics and delete the superseded files before adding another; never discard useful details merely to free a slot. Do not save code structure, paths, architecture, Git history, secrets, debugging recipes represented in code, or current task state. Save user preferences/role, feedback with reasons, non-derivable project decisions/motivations/deadlines, and external references. Return JSON only in this schema: {\"memories\":[{\"file\":\"semantic-name.md\",\"action\":\"upsert|delete\",\"name\":\"title\",\"description\":\"specific relevance hook\",\"type\":\"user|feedback|project|reference\",\"content\":\"durable body\"}]}. Return {\"memories\":[]} when nothing qualifies.\n\nExisting memory manifest:\n" + manifest + "\nFull content for related memories:\n" + relatedMemories + "\nRecent conversation:\n" + transcript
}

func consolidationPrompt(memories, transcripts string) string {
	return "Consolidate the memory files using recent cross-session signal. First inspect the current target-scope memories before proposing any mutation. Merge duplicates by updating the best existing topic, correct contradictions, remove memories that are no longer relevant, stale, or superseded, preserve useful reasons and applicability, and keep each topic focused. Each project has a hard limit of 50 memory slots. Keep MEMORY.md as a compact index of short titles and relevance hooks pointing to detailed topic files. Consolidate related facts in existing topic files instead of creating one memory per fact. At capacity, update existing topics or merge related topics and delete the superseded files before adding another; never discard useful details merely to free a slot. When near or above capacity, actively merge overlapping topics toward 40 occupied slots, preserving useful detail in the retained files. Update or delete only topics whose complete content is supplied in this batch; the manifest alone is not enough. Return no mutations if no evidence-based merge or removal is appropriate. Do not create activity logs or copy repository-derived facts. Return JSON only using the same mutation schema: {\"memories\":[{\"file\":\"semantic-name.md\",\"action\":\"upsert|delete\",\"name\":\"title\",\"description\":\"specific relevance hook\",\"type\":\"user|feedback|project|reference\",\"content\":\"durable body\"}]}. Return {\"memories\":[]} if no changes are needed.\n\nCurrent memories:\n" + memories + "\nRecent session excerpts:\n" + transcripts
}

func acquireDreamLock(path string, previous, now time.Time) (time.Time, bool, error) {
	activePath := path + ".active"
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return previous, false, err
	}
	file, err := os.OpenFile(activePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if os.IsExist(err) {
		info, statErr := os.Stat(activePath)
		if statErr != nil {
			return previous, false, statErr
		}
		if now.Sub(info.ModTime()) < time.Hour {
			return previous, false, nil
		}
		if removeErr := os.Remove(activePath); removeErr != nil && !os.IsNotExist(removeErr) {
			return previous, false, removeErr
		}
		file, err = os.OpenFile(activePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	}
	if os.IsExist(err) {
		return previous, false, nil
	}
	if err != nil {
		return previous, false, err
	}
	_, writeErr := file.WriteString(strconv.Itoa(os.Getpid()))
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(activePath)
		if writeErr != nil {
			return previous, false, writeErr
		}
		return previous, false, closeErr
	}
	return previous, true, nil
}

func commitDreamLock(path string, now time.Time) error {
	if err := atomicWrite(path, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		return err
	}
	return os.Chtimes(path, now, now)
}

func atomicWrite(path string, content []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".memory-*.tmp")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func oneLine(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	return truncateUTF8Prefix(value, limit)
}

func truncateUTF8Prefix(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) && len(value) > 0 {
		value = value[:len(value)-1]
	}
	return value
}

func truncateUTF8Tail(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	value = value[len(value)-limit:]
	for !utf8.ValidString(value) && len(value) > 0 {
		value = value[1:]
	}
	return value
}
