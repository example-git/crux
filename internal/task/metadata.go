package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	RecordVersion  = 1
	maxRecordBytes = 8 << 20
)

type RecordState struct {
	Status          Status `json:"status"`
	StartedAt       int64  `json:"started_at,omitempty"`
	EndedAt         int64  `json:"ended_at,omitempty"`
	StopRequestedAt int64  `json:"stop_requested_at,omitempty"`
	ExitCode        *int   `json:"exit_code,omitempty"`
	Interrupted     bool   `json:"interrupted,omitempty"`
	ErrorCode       string `json:"error_code,omitempty"`
	ErrorMessage    string `json:"error_message,omitempty"`
	LostReason      string `json:"lost_reason,omitempty"`
}

type ShellRecord struct {
	Command             string `json:"command"`
	WorkingDirectory    string `json:"working_directory"`
	Backgrounded        bool   `json:"backgrounded,omitempty"`
	NotificationEmitted bool   `json:"notification_emitted,omitempty"`
}

type AgentRecord struct {
	Prompt              string     `json:"prompt"`
	AgentType           string     `json:"agent_type"`
	ChildSessionID      string     `json:"child_session_id,omitempty"`
	ContinuationOf      string     `json:"continuation_of,omitempty"`
	FinalOutput         string     `json:"final_output,omitempty"`
	UsageBaseline       AgentUsage `json:"usage_baseline"`
	Usage               AgentUsage `json:"usage"`
	NotificationEmitted bool       `json:"notification_emitted,omitempty"`
}

type ImageRecord struct {
	PluginID            string   `json:"plugin_id,omitempty"`
	PluginVersion       string   `json:"plugin_version,omitempty"`
	PluginDigest        string   `json:"plugin_digest,omitempty"`
	OutputExtension     string   `json:"output_extension,omitempty"`
	Mode                string   `json:"mode"`
	Backend             string   `json:"backend,omitempty"`
	Prompt              string   `json:"prompt"`
	Model               string   `json:"model,omitempty"`
	Count               int      `json:"count"`
	Quality             string   `json:"quality,omitempty"`
	Size                string   `json:"size,omitempty"`
	Background          string   `json:"background,omitempty"`
	InputPaths          []string `json:"input_paths,omitempty"`
	OutputPaths         []string `json:"output_paths"`
	Force               bool     `json:"force,omitempty"`
	FinalOutput         string   `json:"final_output,omitempty"`
	NotificationEmitted bool     `json:"notification_emitted,omitempty"`
}

type Record struct {
	Version      int           `json:"version"`
	ID           string        `json:"id"`
	Type         Type          `json:"type"`
	Description  string        `json:"description,omitempty"`
	Ownership    Ownership     `json:"ownership"`
	CreatedAt    int64         `json:"created_at"`
	UpdatedAt    int64         `json:"updated_at"`
	State        RecordState   `json:"state"`
	OutputRef    string        `json:"output_ref,omitempty"`
	Shell        *ShellRecord  `json:"shell,omitempty"`
	Agent        *AgentRecord  `json:"agent,omitempty"`
	Image        *ImageRecord  `json:"image,omitempty"`
	Notification *Notification `json:"notification,omitempty"`
}

type Store struct {
	root string
	dir  *os.File
	mu   sync.Mutex
	now  func() time.Time
}

func NewStore(root string) (*Store, error) {
	if root == "" || !filepath.IsAbs(root) {
		return nil, fmt.Errorf("task metadata root must be absolute")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("creating task metadata directory: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, fmt.Errorf("protecting task metadata directory: %w", err)
	}
	dir, err := openSecureDir(root)
	if err != nil {
		return nil, fmt.Errorf("opening task metadata directory: %w", err)
	}
	return &Store{root: root, dir: dir, now: time.Now}, nil
}

func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir == nil {
		return nil
	}
	err := s.dir.Close()
	s.dir = nil
	return err
}

func (s *Store) Put(record Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.putLocked(record)
}

func (s *Store) putLocked(record Record) error {
	if s.dir == nil {
		return os.ErrClosed
	}
	if record.Version == 0 {
		record.Version = RecordVersion
	}
	now := s.now().UnixMilli()
	if record.CreatedAt == 0 {
		record.CreatedAt = now
	}
	record.UpdatedAt = now
	if err := validateRecord(record); err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encoding task metadata: %w", err)
	}
	if len(data) > maxRecordBytes {
		return fmt.Errorf("task metadata exceeds %d bytes", maxRecordBytes)
	}
	temporaryName := record.ID + "." + uuid.NewString() + ".tmp"
	file, err := createSecureFile(s.dir, s.root, temporaryName, 0o600)
	if err != nil {
		return fmt.Errorf("creating task metadata temporary file: %w", err)
	}
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = removeSecureFile(s.dir, s.root, temporaryName)
		}
	}()
	if _, err := file.Write(data); err != nil {
		file.Close()
		return fmt.Errorf("writing task metadata: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("syncing task metadata: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("closing task metadata: %w", err)
	}
	if err := replaceSecureFile(s.dir, s.root, temporaryName, recordName(record.ID)); err != nil {
		return fmt.Errorf("committing task metadata: %w", err)
	}
	removeTemporary = false
	return nil
}

func (s *Store) Get(id string) (Record, error) {
	if _, err := ParseID(id); err != nil {
		return Record{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readLocked(id)
}

func (s *Store) List() ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir == nil {
		return nil, os.ErrClosed
	}
	if _, err := s.dir.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewinding task metadata directory: %w", err)
	}
	entries, err := s.dir.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("listing task metadata: %w", err)
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasSuffix(name, ".tmp") {
			_ = removeSecureFile(s.dir, s.root, name)
			continue
		}
		if !strings.HasSuffix(name, ".task.json") {
			continue
		}
		id := strings.TrimSuffix(name, ".task.json")
		if _, err := ParseID(id); err != nil {
			return nil, fmt.Errorf("invalid task metadata filename %q", name)
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	records := make([]Record, 0, len(ids))
	for _, id := range ids {
		record, err := s.readLocked(id)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}

func (s *Store) ListNotifications(workspaceID, parentSessionID string, unreadOnly, undeliveredOnly bool) ([]Notification, error) {
	records, err := s.List()
	if err != nil {
		return nil, err
	}
	notifications := make([]Notification, 0, len(records))
	for _, record := range records {
		if record.Notification == nil || record.Notification.WorkspaceID != workspaceID {
			continue
		}
		if parentSessionID != "" && record.Notification.ParentSessionID != parentSessionID {
			continue
		}
		if unreadOnly && !record.Notification.ReadAt.IsZero() {
			continue
		}
		if undeliveredOnly && !record.Notification.ModelDeliveredAt.IsZero() {
			continue
		}
		notifications = append(notifications, *record.Notification)
	}
	sort.Slice(notifications, func(i, j int) bool {
		if notifications[i].EndedAt.Equal(notifications[j].EndedAt) {
			return notifications[i].TaskID < notifications[j].TaskID
		}
		return notifications[i].EndedAt.Before(notifications[j].EndedAt)
	})
	return notifications, nil
}

func (s *Store) MarkNotificationRead(notificationID string) (Notification, error) {
	return s.updateNotification(notificationID, func(notification *Notification) {
		if notification.ReadAt.IsZero() {
			notification.ReadAt = s.now()
		}
	})
}

func (s *Store) MarkNotificationDelivered(notificationID string) (Notification, error) {
	return s.updateNotification(notificationID, func(notification *Notification) {
		if notification.ModelDeliveredAt.IsZero() {
			notification.ModelDeliveredAt = s.now()
		}
	})
}

func (s *Store) updateNotification(notificationID string, update func(*Notification)) (Notification, error) {
	if notificationID == "" {
		return Notification{}, fmt.Errorf("notification ID is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir == nil {
		return Notification{}, os.ErrClosed
	}
	if _, err := s.dir.Seek(0, io.SeekStart); err != nil {
		return Notification{}, fmt.Errorf("rewinding task metadata directory: %w", err)
	}
	entries, err := s.dir.ReadDir(-1)
	if err != nil {
		return Notification{}, fmt.Errorf("listing task metadata: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".task.json") {
			continue
		}
		id := strings.TrimSuffix(name, ".task.json")
		record, err := s.readLocked(id)
		if err != nil {
			return Notification{}, err
		}
		if record.Notification == nil || record.Notification.ID != notificationID {
			continue
		}
		update(record.Notification)
		if err := s.putLocked(record); err != nil {
			return Notification{}, err
		}
		return *record.Notification, nil
	}
	return Notification{}, fmt.Errorf("task notification not found: %s", notificationID)
}

func (s *Store) Remove(id string) error {
	if _, err := ParseID(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir == nil {
		return os.ErrClosed
	}
	if err := removeSecureFile(s.dir, s.root, recordName(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing task metadata: %w", err)
	}
	return nil
}

func (s *Store) readLocked(id string) (Record, error) {
	if s.dir == nil {
		return Record{}, os.ErrClosed
	}
	file, err := openSecureFile(s.dir, s.root, recordName(id))
	if err != nil {
		return Record{}, fmt.Errorf("opening task metadata: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxRecordBytes+1))
	if err != nil {
		return Record{}, fmt.Errorf("reading task metadata: %w", err)
	}
	if len(data) > maxRecordBytes {
		return Record{}, fmt.Errorf("task metadata exceeds %d bytes", maxRecordBytes)
	}
	var record Record
	if err := json.Unmarshal(data, &record); err != nil {
		return Record{}, fmt.Errorf("decoding task metadata for %s: %w", id, err)
	}
	if record.ID != id {
		return Record{}, fmt.Errorf("task metadata ID %q does not match filename %q", record.ID, id)
	}
	if err := validateRecord(record); err != nil {
		return Record{}, err
	}
	return record, nil
}

func validateRecord(record Record) error {
	taskType, err := ParseID(record.ID)
	if err != nil {
		return err
	}
	if record.Version != RecordVersion {
		return fmt.Errorf("unsupported task metadata version %d", record.Version)
	}
	if record.Type != taskType {
		return fmt.Errorf("task metadata type %q does not match ID %q", record.Type, record.ID)
	}
	if record.Ownership.WorkspaceID == "" || record.Ownership.ParentSessionID == "" {
		return fmt.Errorf("task metadata requires workspace and parent session ownership")
	}
	if record.Notification != nil {
		notification := record.Notification
		if notification.ID == "" || notification.TaskID != record.ID || notification.TaskType != record.Type || notification.WorkspaceID != record.Ownership.WorkspaceID || notification.ParentSessionID != record.Ownership.ParentSessionID || !notification.Status.Terminal() {
			return fmt.Errorf("invalid task notification for %s", record.ID)
		}
	}
	switch record.State.Status {
	case StatusPending, StatusRunning, StatusCompleted, StatusFailed, StatusKilled, StatusLost:
	default:
		return fmt.Errorf("invalid task status %q", record.State.Status)
	}
	switch record.Type {
	case TypeShell:
		if record.Shell == nil || record.Agent != nil || record.Image != nil || record.OutputRef != "task-output:"+record.ID {
			return fmt.Errorf("invalid shell task metadata for %s", record.ID)
		}
	case TypeAgent:
		if record.Agent == nil || record.Shell != nil || record.Image != nil {
			return fmt.Errorf("invalid agent task metadata for %s", record.ID)
		}
		if record.Agent.ChildSessionID != "" && record.OutputRef != "session:"+record.Agent.ChildSessionID {
			return fmt.Errorf("invalid agent output reference for %s", record.ID)
		}
		if record.Agent.ContinuationOf != "" {
			continuationType, err := ParseID(record.Agent.ContinuationOf)
			if err != nil || continuationType != TypeAgent {
				return fmt.Errorf("invalid agent continuation task ID %q", record.Agent.ContinuationOf)
			}
		}
	case TypeImage:
		if record.Image == nil || record.Shell != nil || record.Agent != nil || len(record.Image.OutputPaths) == 0 {
			return fmt.Errorf("invalid image task metadata for %s", record.ID)
		}
		if record.OutputRef != "file:"+record.Image.OutputPaths[0] {
			return fmt.Errorf("invalid image output reference for %s", record.ID)
		}
	}
	return nil
}

func recordName(id string) string {
	return id + ".task.json"
}

func StateToRecord(state State) RecordState {
	return RecordState{
		Status:          state.Status,
		StartedAt:       unixMillis(state.StartedAt),
		EndedAt:         unixMillis(state.EndedAt),
		StopRequestedAt: unixMillis(state.StopRequestedAt),
		ExitCode:        state.ExitCode,
		Interrupted:     state.Interrupted,
		ErrorCode:       state.ErrorCode,
		ErrorMessage:    state.ErrorMessage,
		LostReason:      state.LostReason,
	}
}

func StateFromRecord(state RecordState) State {
	return State{
		Status:          state.Status,
		StartedAt:       timeFromMillis(state.StartedAt),
		EndedAt:         timeFromMillis(state.EndedAt),
		StopRequestedAt: timeFromMillis(state.StopRequestedAt),
		ExitCode:        state.ExitCode,
		Interrupted:     state.Interrupted,
		ErrorCode:       state.ErrorCode,
		ErrorMessage:    state.ErrorMessage,
		LostReason:      state.LostReason,
	}
}

func unixMillis(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.UnixMilli()
}

func timeFromMillis(value int64) time.Time {
	if value == 0 {
		return time.Time{}
	}
	return time.UnixMilli(value)
}
