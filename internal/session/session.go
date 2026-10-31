package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/example-git/crux/internal/db"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/google/uuid"
	"github.com/zeebo/xxh3"
)

type TodoStatus string

type Mode string

type GoalStatus string

var (
	ErrAlreadyArchived = errors.New("session is already archived")
	ErrNotArchived     = errors.New("session is not archived")
	ErrGoalChanged     = errors.New("session goal changed")
)

const (
	TodoStatusPending    TodoStatus = "pending"
	TodoStatusInProgress TodoStatus = "in_progress"
	TodoStatusCompleted  TodoStatus = "completed"

	ModeDefault       Mode = "default"
	ModePlan          Mode = "plan"
	ModePlanRevision  Mode = "plan_revision"
	ModePlanExecution Mode = "plan_execution"

	GoalStatusActive   GoalStatus = "active"
	GoalStatusPaused   GoalStatus = "paused"
	GoalStatusBlocked  GoalStatus = "blocked"
	GoalStatusComplete GoalStatus = "complete"
)

func (m Mode) IsPlan() bool {
	return m == ModePlan || m == ModePlanRevision || m == ModePlanExecution
}

func (m Mode) IsReadOnlyPlan() bool {
	return m == ModePlan || m == ModePlanRevision
}

// HashID returns the XXH3 hash of a session ID (UUID) as a hex string.
func HashID(id string) string {
	h := xxh3.New()
	h.WriteString(id)
	return fmt.Sprintf("%x", h.Sum(nil))
}

// ShortID is the session identifier displayed in resume commands and the UI.
// An unassigned session must never acquire a display ID by hashing a blank.
func ShortID(id string) string {
	if strings.TrimSpace(id) == "" {
		return ""
	}
	return HashID(id)[:7]
}

type Todo struct {
	Content    string     `json:"content"`
	Status     TodoStatus `json:"status"`
	ActiveForm string     `json:"active_form"`
}

// HasIncompleteTodos returns true if there are any non-completed todos.
func HasIncompleteTodos(todos []Todo) bool {
	for _, todo := range todos {
		if todo.Status != TodoStatusCompleted {
			return true
		}
	}
	return false
}

type Goal struct {
	Objective  string
	Status     GoalStatus
	Generation int64
	Error      string
	UpdatedAt  int64
}

func (g Goal) Active() bool {
	return g.Objective != "" && g.Status == GoalStatusActive
}

type Session struct {
	ID                  string
	ParentSessionID     string
	ForkedFromSessionID string
	Archived            bool
	Title               string
	MessageCount        int64
	PromptTokens        int64
	CompletionTokens    int64
	UnseenLocalTokens   int64
	EstimatedUsage      bool
	SummaryMessageID    string
	Cost                float64
	Todos               []Todo
	Mode                Mode
	Plan                string
	Goal                Goal
	AnalysisEffort      string
	Model               string
	Provider            string
	CreatedAt           int64
	UpdatedAt           int64
}

func (s Session) ContextTokens() int64 {
	return s.PromptTokens + s.CompletionTokens + s.UnseenLocalTokens
}

func (s Session) ContextEstimated() bool {
	return s.EstimatedUsage || s.UnseenLocalTokens > 0
}

type Service interface {
	pubsub.Subscriber[Session]
	Create(ctx context.Context, title string) (Session, error)
	CreateFork(ctx context.Context, forkedFromSessionID, title string) (Session, error)
	CreateTitleSession(ctx context.Context, parentSessionID string) (Session, error)
	CreateTaskSession(ctx context.Context, toolCallID, parentSessionID, title string) (Session, error)
	Get(ctx context.Context, id string) (Session, error)
	GetLast(ctx context.Context) (Session, error)
	List(ctx context.Context) ([]Session, error)
	ListArchived(ctx context.Context) ([]Session, error)
	Subtree(ctx context.Context, id string) ([]Session, error)
	Archive(ctx context.Context, id string) ([]Session, error)
	Unarchive(ctx context.Context, id string) (Session, error)
	Save(ctx context.Context, session Session) (Session, error)
	SetMode(ctx context.Context, sessionID string, mode Mode) error
	SetPlanState(ctx context.Context, sessionID string, mode Mode, plan string) error
	SetGoal(ctx context.Context, sessionID, objective string) (Session, error)
	SetGoalStatus(ctx context.Context, sessionID string, generation int64, status GoalStatus, goalError string) (Session, error)
	StopGoal(ctx context.Context, sessionID string, generation int64) (Session, error)
	ClearGoal(ctx context.Context, sessionID string) (Session, error)
	SetAnalysisEffort(ctx context.Context, sessionID, effort string) error
	UpdateTitleAndCost(ctx context.Context, sessionID, title string, cost float64) error
	PublishCreated(session Session)
	PublishCompaction(ctx context.Context, sessionID string, estimatedUsage bool) error
	PublishRollback(ctx context.Context, sessionID string) (Session, error)
	Rename(ctx context.Context, id string, title string) error
	Delete(ctx context.Context, id string) error
	DeleteSubtree(ctx context.Context, id string) ([]Session, error)

	// Agent tool session management
	CreateAgentToolSessionID(messageID, toolCallID string) string
	ParseAgentToolSessionID(sessionID string) (messageID string, toolCallID string, ok bool)
	IsAgentToolSession(sessionID string) bool
}

type service struct {
	*pubsub.Broker[Session]
	db          *sql.DB
	q           *db.Queries
	planStateMu sync.Mutex
	goalStateMu sync.Mutex
}

func (s *service) Create(ctx context.Context, title string) (Session, error) {
	dbSession, err := s.q.CreateSession(ctx, db.CreateSessionParams{
		ID:    uuid.New().String(),
		Title: title,
	})
	if err != nil {
		return Session{}, err
	}
	session := s.fromDBItem(dbSession)
	s.Publish(pubsub.CreatedEvent, session)
	return session, nil
}

func (s *service) CreateFork(ctx context.Context, forkedFromSessionID, title string) (Session, error) {
	dbSession, err := s.q.CreateSession(ctx, db.CreateSessionParams{
		ID:                  uuid.New().String(),
		ForkedFromSessionID: sql.NullString{String: forkedFromSessionID, Valid: true},
		Title:               title,
	})
	if err != nil {
		return Session{}, err
	}
	session := s.fromDBItem(dbSession)
	s.Publish(pubsub.CreatedEvent, session)
	return session, nil
}

func (s *service) CreateTaskSession(ctx context.Context, toolCallID, parentSessionID, title string) (Session, error) {
	dbSession, err := s.q.CreateSession(ctx, db.CreateSessionParams{
		ID:              toolCallID,
		ParentSessionID: sql.NullString{String: parentSessionID, Valid: true},
		Title:           title,
	})
	if err != nil {
		return Session{}, err
	}
	session := s.fromDBItem(dbSession)
	s.Publish(pubsub.CreatedEvent, session)
	return session, nil
}

func (s *service) CreateTitleSession(ctx context.Context, parentSessionID string) (Session, error) {
	dbSession, err := s.q.CreateSession(ctx, db.CreateSessionParams{
		ID:              "title-" + parentSessionID,
		ParentSessionID: sql.NullString{String: parentSessionID, Valid: true},
		Title:           "Generate a title",
	})
	if err != nil {
		return Session{}, err
	}
	session := s.fromDBItem(dbSession)
	s.Publish(pubsub.CreatedEvent, session)
	return session, nil
}

func (s *service) Delete(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	qtx := s.q.WithTx(tx)

	dbSession, err := qtx.GetSessionByID(ctx, id)
	if err != nil {
		return err
	}
	if err = qtx.DeleteSessionMessages(ctx, dbSession.ID); err != nil {
		return fmt.Errorf("deleting session messages: %w", err)
	}
	if err = qtx.DeleteSessionFiles(ctx, dbSession.ID); err != nil {
		return fmt.Errorf("deleting session files: %w", err)
	}
	if err = qtx.DeleteSession(ctx, dbSession.ID); err != nil {
		return fmt.Errorf("deleting session: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}

	session := s.fromDBItem(dbSession)
	s.Publish(pubsub.DeletedEvent, session)
	return nil
}

func (s *service) DeleteSubtree(ctx context.Context, id string) ([]Session, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	qtx := s.q.WithTx(tx)
	root, err := qtx.GetSessionByID(ctx, id)
	if err != nil {
		return nil, err
	}
	subtree, err := s.dbSubtree(ctx, qtx, root)
	if err != nil {
		return nil, err
	}
	ordered := make([]db.Session, 0, len(subtree))
	for index := len(subtree) - 1; index > 0; index-- {
		ordered = append(ordered, subtree[index])
	}
	ordered = append(ordered, root)
	deleted := make([]Session, 0, len(ordered))
	for _, current := range ordered {
		if err = qtx.DeleteSessionMessages(ctx, current.ID); err != nil {
			return nil, fmt.Errorf("deleting session messages: %w", err)
		}
		if err = qtx.DeleteSessionFiles(ctx, current.ID); err != nil {
			return nil, fmt.Errorf("deleting session files: %w", err)
		}
		if err = qtx.DeleteSession(ctx, current.ID); err != nil {
			return nil, fmt.Errorf("deleting session: %w", err)
		}
		deleted = append(deleted, s.fromDBItem(current))
	}
	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing transaction: %w", err)
	}
	for _, current := range deleted {
		s.Publish(pubsub.DeletedEvent, current)
	}
	return deleted, nil
}

func (s *service) Get(ctx context.Context, id string) (Session, error) {
	dbSession, err := s.q.GetSessionByID(ctx, id)
	if err != nil {
		return Session{}, err
	}
	return s.withGoal(ctx, s.fromDBItem(dbSession))
}

func (s *service) GetLast(ctx context.Context) (Session, error) {
	dbSession, err := s.q.GetLastSession(ctx)
	if err != nil {
		return Session{}, err
	}
	return s.withGoal(ctx, s.fromDBItem(dbSession))
}

func (s *service) Save(ctx context.Context, session Session) (Session, error) {
	todosJSON, err := marshalTodos(session.Todos)
	if err != nil {
		return Session{}, err
	}

	dbSession, err := s.q.UpdateSession(ctx, db.UpdateSessionParams{
		ID:               session.ID,
		Title:            session.Title,
		PromptTokens:     session.PromptTokens,
		CompletionTokens: session.CompletionTokens,
		SummaryMessageID: sql.NullString{
			String: session.SummaryMessageID,
			Valid:  session.SummaryMessageID != "",
		},
		Cost: session.Cost,
		Todos: sql.NullString{
			String: todosJSON,
			Valid:  todosJSON != "",
		},
		EstimatedUsage:    boolToInt64(session.EstimatedUsage),
		UnseenLocalTokens: session.UnseenLocalTokens,
		Model:             session.Model,
		Provider:          session.Provider,
	})
	if err != nil {
		return Session{}, err
	}
	session, err = s.withGoal(ctx, s.fromDBItem(dbSession))
	if err != nil {
		return Session{}, err
	}
	s.Publish(pubsub.UpdatedEvent, session)
	return session, nil
}

func (s *service) SetMode(ctx context.Context, sessionID string, mode Mode) error {
	switch mode {
	case ModeDefault, ModePlan:
	default:
		return fmt.Errorf("invalid session mode %q", mode)
	}
	return s.SetPlanState(ctx, sessionID, mode, "")
}

func ValidateAnalysisEffort(effort string) error {
	if !slices.Contains([]string{"", "none", "low", "medium", "high", "xhigh", "max"}, effort) {
		return fmt.Errorf("invalid analysis effort %q", effort)
	}
	return nil
}

func (s *service) SetAnalysisEffort(ctx context.Context, sessionID, effort string) error {
	if err := ValidateAnalysisEffort(effort); err != nil {
		return err
	}
	if err := s.q.SetSessionAnalysisEffort(ctx, db.SetSessionAnalysisEffortParams{
		ID:             sessionID,
		AnalysisEffort: effort,
	}); err != nil {
		return err
	}
	s.publishSessionUpdate(ctx, sessionID)
	return nil
}

func (s *service) SetPlanState(ctx context.Context, sessionID string, mode Mode, plan string) error {
	s.planStateMu.Lock()
	defer s.planStateMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	plan = strings.TrimSpace(plan)
	switch mode {
	case ModeDefault, ModePlan:
		if plan != "" {
			return fmt.Errorf("session mode %q cannot retain a plan", mode)
		}
	case ModePlanRevision, ModePlanExecution:
		if plan == "" {
			return fmt.Errorf("session mode %q requires a plan", mode)
		}
	default:
		return fmt.Errorf("invalid session mode %q", mode)
	}
	if err := s.q.SetSessionPlanState(ctx, db.SetSessionPlanStateParams{
		ID:   sessionID,
		Mode: string(mode),
		Plan: plan,
	}); err != nil {
		return err
	}
	s.publishSessionUpdate(ctx, sessionID)
	return nil
}

func (s *service) SetGoal(ctx context.Context, sessionID, objective string) (Session, error) {
	s.goalStateMu.Lock()
	defer s.goalStateMu.Unlock()
	objective = strings.TrimSpace(objective)
	if objective == "" {
		return Session{}, errors.New("goal objective is required")
	}
	if _, err := s.q.GetSessionByID(ctx, sessionID); err != nil {
		return Session{}, err
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO session_goals (session_id, objective, status, generation, error, updated_at)
VALUES (?, ?, 'active', 1, '', strftime('%s', 'now'))
ON CONFLICT(session_id) DO UPDATE SET
    objective = excluded.objective,
    status = 'active',
    generation = session_goals.generation + 1,
    error = '',
    updated_at = strftime('%s', 'now')`, sessionID, objective)
	if err != nil {
		return Session{}, err
	}
	return s.publishGoalUpdate(ctx, sessionID)
}

func (s *service) SetGoalStatus(ctx context.Context, sessionID string, generation int64, status GoalStatus, goalError string) (Session, error) {
	s.goalStateMu.Lock()
	defer s.goalStateMu.Unlock()
	switch status {
	case GoalStatusActive, GoalStatusPaused, GoalStatusBlocked, GoalStatusComplete:
	default:
		return Session{}, fmt.Errorf("invalid goal status %q", status)
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE session_goals
SET status = ?, error = ?, updated_at = strftime('%s', 'now')
WHERE session_id = ? AND generation = ? AND objective != ''`, string(status), strings.TrimSpace(goalError), sessionID, generation)
	if err != nil {
		return Session{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return Session{}, err
	}
	if changed != 1 {
		return Session{}, ErrGoalChanged
	}
	return s.publishGoalUpdate(ctx, sessionID)
}

func (s *service) StopGoal(ctx context.Context, sessionID string, generation int64) (Session, error) {
	s.goalStateMu.Lock()
	defer s.goalStateMu.Unlock()
	result, err := s.db.ExecContext(ctx, `
UPDATE session_goals
SET status = 'paused', generation = generation + 1, error = '', updated_at = strftime('%s', 'now')
WHERE session_id = ? AND generation = ? AND objective != ''`, sessionID, generation)
	if err != nil {
		return Session{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return Session{}, err
	}
	if changed != 1 {
		return Session{}, ErrGoalChanged
	}
	return s.publishGoalUpdate(ctx, sessionID)
}

func (s *service) ClearGoal(ctx context.Context, sessionID string) (Session, error) {
	s.goalStateMu.Lock()
	defer s.goalStateMu.Unlock()
	if _, err := s.q.GetSessionByID(ctx, sessionID); err != nil {
		return Session{}, err
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO session_goals (session_id, objective, status, generation, error, updated_at)
VALUES (?, '', '', 1, '', strftime('%s', 'now'))
ON CONFLICT(session_id) DO UPDATE SET
    objective = '',
    status = '',
    generation = session_goals.generation + 1,
    error = '',
    updated_at = strftime('%s', 'now')`, sessionID)
	if err != nil {
		return Session{}, err
	}
	return s.publishGoalUpdate(ctx, sessionID)
}

func (s *service) publishGoalUpdate(ctx context.Context, sessionID string) (Session, error) {
	current, err := s.Get(ctx, sessionID)
	if err != nil {
		return Session{}, err
	}
	s.Publish(pubsub.UpdatedEvent, current)
	return current, nil
}

// UpdateTitleAndCost updates only the title and cost fields atomically.
// This is safer than fetching, modifying, and saving the entire session.
func (s *service) UpdateTitleAndCost(ctx context.Context, sessionID, title string, cost float64) error {
	if err := s.q.UpdateSessionTitleAndCost(ctx, db.UpdateSessionTitleAndCostParams{
		ID:    sessionID,
		Title: title,
		Cost:  cost,
	}); err != nil {
		return err
	}
	s.publishSessionUpdate(ctx, sessionID)
	return nil
}

func (s *service) PublishCreated(session Session) {
	s.Publish(pubsub.CreatedEvent, session)
}

func (s *service) PublishCompaction(ctx context.Context, sessionID string, estimatedUsage bool) error {
	session, err := s.Get(ctx, sessionID)
	if err != nil {
		return err
	}
	if session.EstimatedUsage != estimatedUsage {
		return fmt.Errorf("persisted compaction estimate state does not match committed state")
	}
	s.Publish(pubsub.UpdatedEvent, session)
	return nil
}

func (s *service) PublishRollback(ctx context.Context, sessionID string) (Session, error) {
	session, err := s.Get(ctx, sessionID)
	if err != nil {
		return Session{}, err
	}
	s.Publish(pubsub.UpdatedEvent, session)
	return session, nil
}

// Rename updates only the title of a session without touching updated_at or
// usage fields.
func (s *service) Rename(ctx context.Context, id string, title string) error {
	if err := s.q.RenameSession(ctx, db.RenameSessionParams{
		ID:    id,
		Title: title,
	}); err != nil {
		return err
	}
	s.publishSessionUpdate(ctx, id)
	return nil
}

func (s *service) List(ctx context.Context) ([]Session, error) {
	dbSessions, err := s.q.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	return s.withGoals(ctx, s.fromDBItems(dbSessions))
}

func (s *service) ListArchived(ctx context.Context) ([]Session, error) {
	dbSessions, err := s.q.ListArchivedSessions(ctx)
	if err != nil {
		return nil, err
	}
	return s.withGoals(ctx, s.fromDBItems(dbSessions))
}

func (s *service) Subtree(ctx context.Context, id string) ([]Session, error) {
	root, err := s.q.GetSessionByID(ctx, id)
	if err != nil {
		return nil, err
	}
	subtree, err := s.dbSubtree(ctx, s.q, root)
	if err != nil {
		return nil, err
	}
	return s.withGoals(ctx, s.fromDBItems(subtree))
}

func (s *service) Archive(ctx context.Context, id string) ([]Session, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	qtx := s.q.WithTx(tx)
	root, err := qtx.GetSessionByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if root.Archived != 0 {
		return nil, ErrAlreadyArchived
	}
	subtree, err := s.dbSubtree(ctx, qtx, root)
	if err != nil {
		return nil, err
	}
	ordered := []db.Session{root}
	for index := len(subtree) - 1; index > 0; index-- {
		ordered = append(ordered, subtree[index])
	}
	archived := make([]Session, 0, len(ordered))
	for _, current := range ordered {
		if current.Archived != 0 {
			continue
		}
		updated, updateErr := qtx.SetSessionArchived(ctx, db.SetSessionArchivedParams{Archived: 1, ID: current.ID})
		if updateErr != nil {
			return nil, updateErr
		}
		archived = append(archived, s.fromDBItem(updated))
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing transaction: %w", err)
	}
	archived, err = s.withGoals(ctx, archived)
	if err != nil {
		return nil, err
	}
	for _, current := range archived {
		s.Publish(pubsub.UpdatedEvent, current)
	}
	return archived, nil
}

type childSessionLister interface {
	ListChildSessions(context.Context, sql.NullString) ([]db.Session, error)
}

func (s *service) dbSubtree(ctx context.Context, queries childSessionLister, root db.Session) ([]db.Session, error) {
	subtree := []db.Session{root}
	for index := 0; index < len(subtree); index++ {
		children, err := queries.ListChildSessions(ctx, sql.NullString{String: subtree[index].ID, Valid: true})
		if err != nil {
			return nil, err
		}
		subtree = append(subtree, children...)
	}
	return subtree, nil
}

func (s *service) Unarchive(ctx context.Context, id string) (Session, error) {
	current, err := s.q.GetSessionByID(ctx, id)
	if err != nil {
		return Session{}, err
	}
	if current.Archived == 0 {
		return Session{}, ErrNotArchived
	}
	updated, err := s.q.SetSessionArchived(ctx, db.SetSessionArchivedParams{Archived: 0, ID: id})
	if err != nil {
		return Session{}, err
	}
	result, err := s.withGoal(ctx, s.fromDBItem(updated))
	if err != nil {
		return Session{}, err
	}
	s.Publish(pubsub.UpdatedEvent, result)
	return result, nil
}

// publishSessionUpdate re-fetches a session and publishes an UpdatedEvent so
// that UI subscribers reflect title or usage changes.
func (s *service) publishSessionUpdate(ctx context.Context, sessionID string) {
	session, err := s.Get(ctx, sessionID)
	if err != nil {
		slog.Error("Failed to re-fetch session for event publish", "error", err, "sessionID", sessionID)
		return
	}
	s.Publish(pubsub.UpdatedEvent, session)
}

func (s *service) withGoal(ctx context.Context, current Session) (Session, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT objective, status, generation, error, updated_at
FROM session_goals
WHERE session_id = ?`, current.ID)
	var status string
	err := row.Scan(
		&current.Goal.Objective,
		&status,
		&current.Goal.Generation,
		&current.Goal.Error,
		&current.Goal.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return current, nil
	}
	if err != nil {
		return Session{}, err
	}
	current.Goal.Status = GoalStatus(status)
	return current, nil
}

func (s *service) withGoals(ctx context.Context, sessions []Session) ([]Session, error) {
	for index := range sessions {
		current, err := s.withGoal(ctx, sessions[index])
		if err != nil {
			return nil, err
		}
		sessions[index] = current
	}
	return sessions, nil
}

func (s *service) fromDBItems(items []db.Session) []Session {
	sessions := make([]Session, len(items))
	for index, item := range items {
		sessions[index] = s.fromDBItem(item)
	}
	return sessions
}

func (s *service) fromDBItem(item db.Session) Session {
	todos, err := unmarshalTodos(item.Todos.String)
	if err != nil {
		slog.Error("Failed to unmarshal todos", "session_id", item.ID, "error", err)
	}
	return Session{
		ID:                  item.ID,
		ParentSessionID:     item.ParentSessionID.String,
		ForkedFromSessionID: item.ForkedFromSessionID.String,
		Archived:            item.Archived != 0,
		Title:               item.Title,
		MessageCount:        item.MessageCount,
		PromptTokens:        item.PromptTokens,
		CompletionTokens:    item.CompletionTokens,
		EstimatedUsage:      item.EstimatedUsage != 0,
		UnseenLocalTokens:   item.UnseenLocalTokens,
		SummaryMessageID:    item.SummaryMessageID.String,
		Cost:                item.Cost,
		Todos:               todos,
		Mode:                Mode(item.Mode),
		Plan:                item.Plan,
		AnalysisEffort:      item.AnalysisEffort,
		Model:               item.Model,
		Provider:            item.Provider,
		CreatedAt:           item.CreatedAt,
		UpdatedAt:           item.UpdatedAt,
	}
}

func boolToInt64(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

func marshalTodos(todos []Todo) (string, error) {
	if len(todos) == 0 {
		return "", nil
	}
	data, err := json.Marshal(todos)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func unmarshalTodos(data string) ([]Todo, error) {
	if data == "" {
		return []Todo{}, nil
	}
	var todos []Todo
	if err := json.Unmarshal([]byte(data), &todos); err != nil {
		return []Todo{}, err
	}
	return todos, nil
}

func NewService(q *db.Queries, conn *sql.DB) Service {
	broker := pubsub.NewBroker[Session]()
	return &service{
		Broker: broker,
		db:     conn,
		q:      q,
	}
}

// CreateAgentToolSessionID creates a session ID for agent tool sessions using the format "messageID$$toolCallID"
func (s *service) CreateAgentToolSessionID(messageID, toolCallID string) string {
	return fmt.Sprintf("%s$$%s", messageID, toolCallID)
}

// ParseAgentToolSessionID parses an agent tool session ID into its components
func (s *service) ParseAgentToolSessionID(sessionID string) (messageID string, toolCallID string, ok bool) {
	parts := strings.Split(sessionID, "$$")
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// IsAgentToolSession checks if a session ID follows the agent tool session format
func (s *service) IsAgentToolSession(sessionID string) bool {
	_, _, ok := s.ParseAgentToolSessionID(sessionID)
	return ok
}
