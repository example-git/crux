package task

import "time"

type Status string

const (
	RecentTerminalLimit = 15

	StatusPending   Status = "pending"
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusKilled    Status = "killed"
	StatusLost      Status = "lost"
)

func (s Status) Terminal() bool {
	switch s {
	case StatusCompleted, StatusFailed, StatusKilled, StatusLost:
		return true
	default:
		return false
	}
}

type State struct {
	Status          Status    `json:"status"`
	StartedAt       time.Time `json:"started_at,omitempty"`
	EndedAt         time.Time `json:"ended_at,omitempty"`
	StopRequestedAt time.Time `json:"stop_requested_at,omitempty"`
	ExitCode        *int      `json:"exit_code,omitempty"`
	Interrupted     bool      `json:"interrupted,omitempty"`
	ErrorCode       string    `json:"error_code,omitempty"`
	ErrorMessage    string    `json:"error_message,omitempty"`
	LostReason      string    `json:"lost_reason,omitempty"`
}

type AgentUsage struct {
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	Cost             float64 `json:"cost"`
	ToolUseCount     int     `json:"tool_use_count"`
}

type View struct {
	ID             string     `json:"id"`
	Type           Type       `json:"type"`
	Description    string     `json:"description"`
	Command        string     `json:"command,omitempty"`
	Ownership      Ownership  `json:"ownership"`
	State          State      `json:"state"`
	OutputRef      string     `json:"output_ref,omitempty"`
	ChildSessionID string     `json:"child_session_id,omitempty"`
	ContinuationOf string     `json:"continuation_of,omitempty"`
	AgentType      string     `json:"agent_type,omitempty"`
	FinalOutput    string     `json:"final_output,omitempty"`
	Usage          AgentUsage `json:"usage"`
}

type OutputResult struct {
	Task            View            `json:"task"`
	Output          string          `json:"output"`
	RetrievalStatus RetrievalStatus `json:"retrieval_status"`
	Status          RetrievalStatus `json:"-"`
	NextOffset      int64           `json:"next_offset"`
	OutputTruncated bool            `json:"output_truncated"`
}

type Notification struct {
	ID               string     `json:"notification_id"`
	TaskID           string     `json:"task_id"`
	TaskType         Type       `json:"task_type"`
	ToolUseID        string     `json:"tool_use_id,omitempty"`
	WorkspaceID      string     `json:"workspace_id"`
	ParentSessionID  string     `json:"parent_session_id"`
	Status           Status     `json:"status"`
	Summary          string     `json:"summary"`
	EndedAt          time.Time  `json:"ended_at"`
	OutputRef        string     `json:"output_ref,omitempty"`
	OutputTruncated  bool       `json:"output_truncated,omitempty"`
	ExitCode         *int       `json:"exit_code,omitempty"`
	Interrupted      bool       `json:"interrupted,omitempty"`
	ErrorCode        string     `json:"error_code,omitempty"`
	ErrorMessage     string     `json:"error_message,omitempty"`
	LostReason       string     `json:"lost_reason,omitempty"`
	FinalOutput      string     `json:"final_output,omitempty"`
	Usage            AgentUsage `json:"usage"`
	ModelDeliveredAt time.Time  `json:"model_delivered_at,omitempty"`
	ReadAt           time.Time  `json:"read_at,omitempty"`
}
