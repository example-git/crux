package proto

import "time"

type CodebaseIndexStatus struct {
	Enabled          bool      `json:"enabled"`
	State            string    `json:"state"`
	ProjectRoot      string    `json:"project_root,omitempty"`
	DatabasePath     string    `json:"database_path,omitempty"`
	StoreDirectory   string    `json:"store_directory,omitempty"`
	SourceMode       string    `json:"source_mode,omitempty"`
	CredentialStatus string    `json:"credential_status,omitempty"`
	Model            string    `json:"model,omitempty"`
	IncludePaths     []string  `json:"include_paths,omitempty"`
	ExcludePaths     []string  `json:"exclude_paths,omitempty"`
	FilesTotal       int       `json:"files_total,omitempty"`
	FilesProcessed   int       `json:"files_processed,omitempty"`
	ChunksCreated    int       `json:"chunks_created,omitempty"`
	FilesSkipped     int       `json:"files_skipped,omitempty"`
	CurrentPath      string    `json:"current_path,omitempty"`
	Stage            string    `json:"stage,omitempty"`
	StartedAt        time.Time `json:"started_at,omitempty"`
	FinishedAt       time.Time `json:"finished_at,omitempty"`
	Error            string    `json:"error,omitempty"`
	MemoryActivity   string    `json:"memory_activity,omitempty"`
}

type CodebaseIndexUpdate struct {
	Enabled        bool     `json:"enabled"`
	Reindex        bool     `json:"reindex,omitempty"`
	DatabasePath   string   `json:"database_path,omitempty"`
	StoreDirectory string   `json:"store_directory,omitempty"`
	IncludePaths   []string `json:"include_paths,omitempty"`
	ExcludePaths   []string `json:"exclude_paths,omitempty"`
}
