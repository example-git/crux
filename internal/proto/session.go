package proto

// Session represents a session in the proto layer.
//
// IsBusy is computed on read (it is not persisted with the session) and
// reflects whether an agent run is currently in flight for this session.
// It is populated by REST handlers in internal/server/proto.go from the
// workspace's AgentCoordinator. The Session SSE event path does not set
// it, since SSE consumers can compute presence from other agent signals.
//
// AttachedClients counts the number of clients currently viewing this
// session — i.e. entries in the workspace's clients map whose
// currentSessionID equals this session's ID and which have at least one
// live SSE stream. Hold-only clients (streams == 0) do not contribute.
// Like IsBusy, it is computed on read by REST handlers.
type Session struct {
	ID                  string  `json:"id"`
	ParentSessionID     string  `json:"parent_session_id"`
	ForkedFromSessionID string  `json:"forked_from_session_id"`
	Archived            bool    `json:"archived"`
	Title               string  `json:"title"`
	MessageCount        int64   `json:"message_count"`
	PromptTokens        int64   `json:"prompt_tokens"`
	CompletionTokens    int64   `json:"completion_tokens"`
	EstimatedUsage      bool    `json:"estimated_usage"`
	UnseenLocalTokens   int64   `json:"unseen_local_tokens"`
	SummaryMessageID    string  `json:"summary_message_id"`
	Cost                float64 `json:"cost"`
	Todos               []Todo  `json:"todos,omitempty"`
	Mode                string  `json:"mode"`
	Plan                string  `json:"plan,omitempty"`
	Goal                Goal    `json:"goal"`
	AnalysisEffort      string  `json:"analysis_effort,omitempty"`
	Model               string  `json:"model,omitempty"`
	Provider            string  `json:"provider,omitempty"`
	CreatedAt           int64   `json:"created_at"`
	UpdatedAt           int64   `json:"updated_at"`
	IsBusy              bool    `json:"is_busy"`
	AttachedClients     int     `json:"attached_clients"`
}

// Todo represents a single todo entry on a session in the proto layer.
type Todo struct {
	Content    string `json:"content"`
	Status     string `json:"status"`
	ActiveForm string `json:"active_form"`
}

type Goal struct {
	Objective  string `json:"objective,omitempty"`
	Status     string `json:"status,omitempty"`
	Generation int64  `json:"generation,omitempty"`
	Error      string `json:"error,omitempty"`
	UpdatedAt  int64  `json:"updated_at,omitempty"`
}

type SessionGoalRequest struct {
	Action     string `json:"action"`
	Objective  string `json:"objective,omitempty"`
	Status     string `json:"status,omitempty"`
	Generation int64  `json:"generation,omitempty"`
	Error      string `json:"error,omitempty"`
}

type SessionRollbackResponse struct {
	Session  Session   `json:"session"`
	Messages []Message `json:"messages"`
}
