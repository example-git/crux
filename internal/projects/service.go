package projects

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/example-git/crux/internal/home"
	"gopkg.in/yaml.v3"
)

const (
	projectVersion = 1
	maxProjectSize = 1024 * 1024
)

var (
	projectSlugPattern   = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)
	projectItemIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	taskLinePattern      = regexp.MustCompile("^(\\s*-\\s+\\[)([ xX])(\\]\\s+`?([A-Za-z0-9][A-Za-z0-9._-]*)`?\\s+(.+))$")
	serviceMu            sync.Mutex
)

type Status string

const (
	StatusActive    Status = "active"
	StatusPaused    Status = "paused"
	StatusCompleted Status = "completed"
)

type Metadata struct {
	Version int      `yaml:"version" json:"version"`
	Name    string   `yaml:"name" json:"name"`
	Slug    string   `yaml:"slug" json:"slug"`
	Status  Status   `yaml:"status" json:"status"`
	Roots   []string `yaml:"roots" json:"roots"`
}

type Task struct {
	ID        string `json:"id"`
	Content   string `json:"content"`
	Completed bool   `json:"completed"`
	Level     int    `json:"level"`
	line      int
}

type Document struct {
	Metadata  Metadata `json:"metadata"`
	Path      string   `json:"path"`
	NotesPath string   `json:"notes_path"`
	Content   string   `json:"content"`
	Notes     string   `json:"notes"`
	Tasks     []Task   `json:"tasks"`
}

type Definition struct {
	Name            string
	Slug            string
	Goal            string
	SuccessCriteria []string
	Tasks           []DefinitionTask
}

type DefinitionTask struct {
	ID       string
	Content  string
	ParentID string
}

type Service struct {
	dir string
}

type selections struct {
	Workspaces map[string]string `json:"workspaces"`
}

func NewService() *Service {
	return &Service{dir: filepath.Join(home.Dir(), ".ai-cli", "projects")}
}

func NewServiceAt(dir string) *Service {
	return &Service{dir: dir}
}

func (s *Service) Directory() string {
	return s.dir
}

func (s *Service) Create(definition Definition, root string) (Document, error) {
	serviceMu.Lock()
	defer serviceMu.Unlock()

	definition.Name = strings.TrimSpace(definition.Name)
	definition.Slug = strings.TrimSpace(definition.Slug)
	definition.Goal = strings.TrimSpace(definition.Goal)
	if definition.Name == "" {
		return Document{}, errors.New("project name is required")
	}
	if err := validateSlug(definition.Slug); err != nil {
		return Document{}, err
	}
	if definition.Goal == "" {
		return Document{}, errors.New("project goal is required")
	}
	if len(definition.SuccessCriteria) == 0 {
		return Document{}, errors.New("project requires at least one success criterion")
	}
	if len(definition.Tasks) == 0 {
		return Document{}, errors.New("project requires at least one task")
	}
	canonicalRoot, err := canonicalPath(root)
	if err != nil {
		return Document{}, fmt.Errorf("resolving project root: %w", err)
	}
	metadata := Metadata{
		Version: projectVersion,
		Name:    definition.Name,
		Slug:    definition.Slug,
		Status:  StatusActive,
		Roots:   []string{canonicalRoot},
	}
	body, err := definitionBody(definition)
	if err != nil {
		return Document{}, err
	}
	content, err := encodeDocument(metadata, body)
	if err != nil {
		return Document{}, err
	}
	notes := fmt.Sprintf("# %s Notes\n", definition.Name)
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return Document{}, fmt.Errorf("creating projects directory %q: %w", s.dir, err)
	}
	path, notesPath := s.paths(definition.Slug)
	if err := createExclusive(path, content); err != nil {
		return Document{}, err
	}
	if err := createExclusive(notesPath, notes); err != nil {
		_ = os.Remove(path)
		return Document{}, err
	}
	if err := s.selectUnlocked(canonicalRoot, definition.Slug); err != nil {
		_ = os.Remove(path)
		_ = os.Remove(notesPath)
		return Document{}, err
	}
	return s.getUnlocked(definition.Slug)
}

func (s *Service) Get(slug string) (Document, error) {
	serviceMu.Lock()
	defer serviceMu.Unlock()
	return s.getUnlocked(slug)
}

func (s *Service) List() ([]Document, error) {
	serviceMu.Lock()
	defer serviceMu.Unlock()
	return s.listUnlocked()
}

func (s *Service) Active(workingDir string) (Document, bool, error) {
	serviceMu.Lock()
	defer serviceMu.Unlock()
	return s.activeUnlocked(workingDir)
}

func (s *Service) Activate(slug, workingDir string) (Document, error) {
	serviceMu.Lock()
	defer serviceMu.Unlock()

	selected, err := s.getUnlocked(slug)
	if err != nil {
		return Document{}, err
	}
	if selected.Metadata.Status == StatusCompleted {
		return Document{}, fmt.Errorf("project %q is completed and cannot be activated", slug)
	}
	canonicalWorkingDir, err := canonicalPath(workingDir)
	if err != nil {
		return Document{}, fmt.Errorf("resolving working directory: %w", err)
	}
	original := selected
	original.Metadata.Roots = slices.Clone(selected.Metadata.Roots)
	if !matchesAnyRoot(canonicalWorkingDir, selected.Metadata.Roots) {
		selected.Metadata.Roots = append(selected.Metadata.Roots, canonicalWorkingDir)
	}
	selected.Metadata.Status = StatusActive
	if err := s.writeDocumentUnlocked(selected); err != nil {
		return Document{}, err
	}
	if err := s.selectUnlocked(canonicalWorkingDir, slug); err != nil {
		return Document{}, errors.Join(err, s.writeDocumentUnlocked(original))
	}
	return s.getUnlocked(slug)
}

func (s *Service) Disable(workingDir string) error {
	serviceMu.Lock()
	defer serviceMu.Unlock()

	canonicalWorkingDir, err := canonicalPath(workingDir)
	if err != nil {
		return fmt.Errorf("resolving working directory: %w", err)
	}
	state, err := s.loadSelectionsUnlocked()
	if err != nil {
		return err
	}
	delete(state.Workspaces, canonicalWorkingDir)
	return s.writeSelectionsUnlocked(state)
}

func (s *Service) UpdateTask(workingDir, taskID string, completed bool, note string) (Document, error) {
	serviceMu.Lock()
	defer serviceMu.Unlock()

	document, ok, err := s.activeUnlocked(workingDir)
	if err != nil {
		return Document{}, err
	}
	if !ok {
		return Document{}, errors.New("no active project matches the current workspace")
	}
	taskID = strings.TrimSpace(taskID)
	index := slices.IndexFunc(document.Tasks, func(task Task) bool { return task.ID == taskID })
	if index < 0 {
		return Document{}, fmt.Errorf("project %q has no task or criterion %q", document.Metadata.Slug, taskID)
	}
	task := document.Tasks[index]
	if lineUnderTasks(document.Content, task.line) {
		if completed {
			for _, descendant := range document.Tasks[index+1:] {
				if !lineUnderTasks(document.Content, descendant.line) || descendant.Level <= task.Level {
					break
				}
				if !descendant.Completed {
					return Document{}, fmt.Errorf("task %q cannot be completed before subtask %q", taskID, descendant.ID)
				}
			}
		} else {
			for ancestorIndex := index - 1; ancestorIndex >= 0; ancestorIndex-- {
				ancestor := document.Tasks[ancestorIndex]
				if !lineUnderTasks(document.Content, ancestor.line) {
					break
				}
				if ancestor.Level >= task.Level {
					continue
				}
				if ancestor.Completed {
					return Document{}, fmt.Errorf("task %q cannot be reopened while parent task %q is complete", taskID, ancestor.ID)
				}
				break
			}
		}
	}
	lines := strings.Split(document.Content, "\n")
	match := taskLinePattern.FindStringSubmatch(lines[task.line])
	if match == nil {
		return Document{}, fmt.Errorf("task %q changed while updating project", taskID)
	}
	mark := " "
	if completed {
		mark = "x"
	}
	lines[task.line] = match[1] + mark + match[3]
	updatedContent := strings.Join(lines, "\n")
	note = strings.TrimSpace(note)
	if note == "" {
		if err := atomicWrite(document.Path, updatedContent); err != nil {
			return Document{}, err
		}
		return s.getUnlocked(document.Metadata.Slug)
	}
	originalNotes := document.Notes
	entry := fmt.Sprintf("\n- %s `%s`: %s\n", time.Now().UTC().Format(time.RFC3339), taskID, note)
	if err := atomicWrite(document.NotesPath, originalNotes+entry); err != nil {
		return Document{}, err
	}
	if err := atomicWrite(document.Path, updatedContent); err != nil {
		return Document{}, errors.Join(err, atomicWrite(document.NotesPath, originalNotes))
	}
	return s.getUnlocked(document.Metadata.Slug)
}

func (s *Service) AppendNotes(workingDir, content string) (Document, error) {
	serviceMu.Lock()
	defer serviceMu.Unlock()

	document, ok, err := s.activeUnlocked(workingDir)
	if err != nil {
		return Document{}, err
	}
	if !ok {
		return Document{}, errors.New("no active project matches the current workspace")
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return Document{}, errors.New("project note content is required")
	}
	entry := fmt.Sprintf("\n## %s\n\n%s\n", time.Now().UTC().Format(time.RFC3339), content)
	if err := appendFile(document.NotesPath, entry); err != nil {
		return Document{}, err
	}
	return s.getUnlocked(document.Metadata.Slug)
}

func (s *Service) Complete(workingDir string) (Document, error) {
	serviceMu.Lock()
	defer serviceMu.Unlock()

	document, ok, err := s.activeUnlocked(workingDir)
	if err != nil {
		return Document{}, err
	}
	if !ok {
		return Document{}, errors.New("no active project matches the current workspace")
	}
	if len(document.Tasks) == 0 {
		return Document{}, errors.New("project has no tasks or success criteria")
	}
	var incomplete []string
	for _, task := range document.Tasks {
		if !task.Completed {
			incomplete = append(incomplete, task.ID)
		}
	}
	if len(incomplete) > 0 {
		return Document{}, fmt.Errorf("project cannot be completed; incomplete items: %s", strings.Join(incomplete, ", "))
	}
	state, err := s.loadSelectionsUnlocked()
	if err != nil {
		return Document{}, err
	}
	canonicalWorkingDir, err := canonicalPath(workingDir)
	if err != nil {
		return Document{}, err
	}
	original := document
	document.Metadata.Status = StatusCompleted
	if err := s.writeDocumentUnlocked(document); err != nil {
		return Document{}, err
	}
	delete(state.Workspaces, canonicalWorkingDir)
	if err := s.writeSelectionsUnlocked(state); err != nil {
		return Document{}, errors.Join(err, s.writeDocumentUnlocked(original))
	}
	return s.getUnlocked(document.Metadata.Slug)
}

func (d Document) CurrentGoal() (Task, []Task, bool) {
	minimumLevel := -1
	for _, task := range d.Tasks {
		if task.Completed || !lineUnderTasks(d.Content, task.line) {
			continue
		}
		if minimumLevel == -1 || task.Level < minimumLevel {
			minimumLevel = task.Level
		}
	}
	if minimumLevel == -1 {
		return Task{}, nil, false
	}
	for index, task := range d.Tasks {
		if task.Completed || task.Level != minimumLevel || !lineUnderTasks(d.Content, task.line) {
			continue
		}
		var subtasks []Task
		for _, candidate := range d.Tasks[index+1:] {
			if candidate.Level <= task.Level {
				break
			}
			if !candidate.Completed {
				subtasks = append(subtasks, candidate)
			}
		}
		return task, subtasks, true
	}
	return Task{}, nil, false
}

func (s *Service) getUnlocked(slug string) (Document, error) {
	if err := validateSlug(strings.TrimSpace(slug)); err != nil {
		return Document{}, err
	}
	path, notesPath := s.paths(strings.TrimSpace(slug))
	content, err := readBounded(path)
	if err != nil {
		return Document{}, fmt.Errorf("reading project %q: %w", slug, err)
	}
	notes, err := readBounded(notesPath)
	if err != nil {
		return Document{}, fmt.Errorf("reading project notes %q: %w", slug, err)
	}
	metadata, _, err := decodeDocument(content)
	if err != nil {
		return Document{}, fmt.Errorf("parsing project %q: %w", slug, err)
	}
	if metadata.Slug != slug {
		return Document{}, fmt.Errorf("project filename slug %q does not match frontmatter slug %q", slug, metadata.Slug)
	}
	tasks, err := parseTasks(content)
	if err != nil {
		return Document{}, fmt.Errorf("parsing project %q tasks: %w", slug, err)
	}
	return Document{
		Metadata:  metadata,
		Path:      path,
		NotesPath: notesPath,
		Content:   content,
		Notes:     notes,
		Tasks:     tasks,
	}, nil
}

func (s *Service) listUnlocked() ([]Document, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading projects directory %q: %w", s.dir, err)
	}
	var documents []Document
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".md") || strings.HasSuffix(name, ".notes.md") {
			continue
		}
		document, err := s.getUnlocked(strings.TrimSuffix(name, ".md"))
		if err != nil {
			return nil, err
		}
		documents = append(documents, document)
	}
	slices.SortFunc(documents, func(a, b Document) int { return strings.Compare(a.Metadata.Slug, b.Metadata.Slug) })
	return documents, nil
}

func (s *Service) activeUnlocked(workingDir string) (Document, bool, error) {
	canonicalWorkingDir, err := canonicalPath(workingDir)
	if err != nil {
		return Document{}, false, fmt.Errorf("resolving working directory: %w", err)
	}
	state, err := s.loadSelectionsUnlocked()
	if err != nil {
		return Document{}, false, err
	}
	slug := state.Workspaces[canonicalWorkingDir]
	if slug == "" {
		return Document{}, false, nil
	}
	document, err := s.getUnlocked(slug)
	if err != nil {
		return Document{}, false, fmt.Errorf("loading selected project %q: %w", slug, err)
	}
	if document.Metadata.Status == StatusCompleted {
		return Document{}, false, nil
	}
	return document, true, nil
}

func (s *Service) selectUnlocked(workingDir, slug string) error {
	state, err := s.loadSelectionsUnlocked()
	if err != nil {
		return err
	}
	state.Workspaces[workingDir] = slug
	return s.writeSelectionsUnlocked(state)
}

func (s *Service) loadSelectionsUnlocked() (selections, error) {
	path := filepath.Join(s.dir, "selections.json")
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return selections{Workspaces: make(map[string]string)}, nil
	}
	if err != nil {
		return selections{}, fmt.Errorf("reading project selections: %w", err)
	}
	var state selections
	if err := json.Unmarshal(content, &state); err != nil {
		return selections{}, fmt.Errorf("parsing project selections: %w", err)
	}
	if state.Workspaces == nil {
		state.Workspaces = make(map[string]string)
	}
	return state, nil
}

func (s *Service) writeSelectionsUnlocked(state selections) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	content, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding project selections: %w", err)
	}
	return atomicWrite(filepath.Join(s.dir, "selections.json"), string(content)+"\n")
}

func (s *Service) writeDocumentUnlocked(document Document) error {
	_, body, err := decodeDocument(document.Content)
	if err != nil {
		return err
	}
	content, err := encodeDocument(document.Metadata, body)
	if err != nil {
		return err
	}
	return atomicWrite(document.Path, content)
}

func (s *Service) paths(slug string) (string, string) {
	return filepath.Join(s.dir, slug+".md"), filepath.Join(s.dir, slug+".notes.md")
}

func definitionBody(definition Definition) (string, error) {
	var builder strings.Builder
	builder.WriteString("# Goal\n\n")
	builder.WriteString(strings.TrimSpace(definition.Goal))
	builder.WriteString("\n\n# Success Criteria\n\n")
	seen := make(map[string]struct{}, len(definition.SuccessCriteria)+len(definition.Tasks))
	for index, criterion := range definition.SuccessCriteria {
		criterion = strings.TrimSpace(criterion)
		if criterion == "" || strings.ContainsAny(criterion, "\r\n") {
			return "", fmt.Errorf("success criterion %d must be non-empty and single-line", index+1)
		}
		id := fmt.Sprintf("C%d", index+1)
		seen[id] = struct{}{}
		fmt.Fprintf(&builder, "- [ ] `%s` %s\n", id, criterion)
	}
	builder.WriteString("\n# Tasks\n\n")
	levels := make(map[string]int, len(definition.Tasks))
	for index, task := range definition.Tasks {
		task.ID = strings.TrimSpace(task.ID)
		task.Content = strings.TrimSpace(task.Content)
		task.ParentID = strings.TrimSpace(task.ParentID)
		if task.ID == "" {
			return "", fmt.Errorf("task %d ID is required", index+1)
		}
		if !projectItemIDPattern.MatchString(task.ID) {
			return "", fmt.Errorf("invalid task ID %q", task.ID)
		}
		if _, exists := seen[task.ID]; exists {
			return "", fmt.Errorf("duplicate task or criterion ID %q", task.ID)
		}
		if task.Content == "" || strings.ContainsAny(task.Content, "\r\n") {
			return "", fmt.Errorf("task %q content must be non-empty and single-line", task.ID)
		}
		level := 0
		if task.ParentID != "" {
			parentLevel, exists := levels[task.ParentID]
			if !exists {
				return "", fmt.Errorf("task %q parent %q must appear first", task.ID, task.ParentID)
			}
			level = parentLevel + 1
		}
		seen[task.ID] = struct{}{}
		levels[task.ID] = level
		fmt.Fprintf(&builder, "%s- [ ] `%s` %s\n", strings.Repeat("  ", level), task.ID, task.Content)
	}
	return builder.String(), nil
}

func validateSlug(slug string) error {
	if !projectSlugPattern.MatchString(slug) {
		return fmt.Errorf("invalid project slug %q: use lowercase letters, numbers, and single hyphen-separated words", slug)
	}
	return nil
}

func validateMetadata(metadata Metadata) error {
	if metadata.Version != projectVersion {
		return fmt.Errorf("unsupported project version %d", metadata.Version)
	}
	if strings.TrimSpace(metadata.Name) == "" {
		return errors.New("project name is required")
	}
	if err := validateSlug(metadata.Slug); err != nil {
		return err
	}
	switch metadata.Status {
	case StatusActive, StatusPaused, StatusCompleted:
	default:
		return fmt.Errorf("invalid project status %q", metadata.Status)
	}
	if len(metadata.Roots) == 0 {
		return errors.New("project must declare at least one root")
	}
	for _, root := range metadata.Roots {
		if !filepath.IsAbs(root) {
			return fmt.Errorf("project root %q must be absolute", root)
		}
	}
	return nil
}

func decodeDocument(content string) (Metadata, string, error) {
	normalized := strings.ReplaceAll(strings.TrimPrefix(content, "\uFEFF"), "\r\n", "\n")
	if !strings.HasPrefix(normalized, "---\n") {
		return Metadata{}, "", errors.New("no YAML frontmatter found")
	}
	end := strings.Index(normalized[4:], "\n---\n")
	if end < 0 {
		return Metadata{}, "", errors.New("unclosed YAML frontmatter")
	}
	end += 4
	var metadata Metadata
	decoder := yaml.NewDecoder(strings.NewReader(normalized[4:end]))
	decoder.KnownFields(true)
	if err := decoder.Decode(&metadata); err != nil {
		return Metadata{}, "", fmt.Errorf("decoding YAML frontmatter: %w", err)
	}
	if err := validateMetadata(metadata); err != nil {
		return Metadata{}, "", err
	}
	return metadata, strings.TrimPrefix(normalized[end+5:], "\n"), nil
}

func encodeDocument(metadata Metadata, body string) (string, error) {
	if err := validateMetadata(metadata); err != nil {
		return "", err
	}
	frontmatter, err := yaml.Marshal(metadata)
	if err != nil {
		return "", fmt.Errorf("encoding project frontmatter: %w", err)
	}
	return "---\n" + string(frontmatter) + "---\n\n" + strings.TrimLeft(body, "\n"), nil
}

func parseTasks(content string) ([]Task, error) {
	lines := strings.Split(content, "\n")
	seen := make(map[string]struct{})
	var tasks []Task
	for index, line := range lines {
		match := taskLinePattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		id := match[4]
		if _, ok := seen[id]; ok {
			return nil, fmt.Errorf("duplicate task or criterion ID %q", id)
		}
		seen[id] = struct{}{}
		indent := strings.TrimSuffix(match[1], strings.TrimLeft(match[1], " \t"))
		level := 0
		for _, char := range indent {
			if char == '\t' {
				level += 4
			} else {
				level++
			}
		}
		tasks = append(tasks, Task{
			ID:        id,
			Content:   strings.TrimSpace(match[5]),
			Completed: match[2] == "x" || match[2] == "X",
			Level:     level,
			line:      index,
		})
	}
	return tasks, nil
}

func lineUnderTasks(content string, lineIndex int) bool {
	lines := strings.Split(content, "\n")
	underTasks := false
	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.EqualFold(trimmed, "# Tasks") {
			underTasks = true
		} else if strings.HasPrefix(trimmed, "# ") {
			underTasks = false
		}
		if index == lineIndex {
			return underTasks
		}
	}
	return false
}

func matchesAnyRoot(path string, roots []string) bool {
	for _, root := range roots {
		canonicalRoot, err := canonicalPath(root)
		if err != nil {
			continue
		}
		relative, err := filepath.Rel(canonicalRoot, path)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func canonicalPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err == nil {
		return filepath.Clean(resolved), nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return filepath.Clean(absolute), nil
	}
	return "", err
}

func readBounded(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.Size() > maxProjectSize {
		return "", fmt.Errorf("file exceeds %d bytes", maxProjectSize)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(content), nil
}

func createExclusive(path, content string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("project file %q already exists", path)
	}
	if err != nil {
		return fmt.Errorf("creating project file %q: %w", path, err)
	}
	if _, err := file.WriteString(content); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return fmt.Errorf("writing project file %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("closing project file %q: %w", path, err)
	}
	return nil
}

func atomicWrite(path, content string) error {
	if len(content) > maxProjectSize {
		return fmt.Errorf("project file exceeds %d bytes", maxProjectSize)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".project-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temporary project file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.WriteString(content); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replacing project file %q: %w", path, err)
	}
	return nil
}

func appendFile(path, content string) error {
	current, err := readBounded(path)
	if err != nil {
		return err
	}
	return atomicWrite(path, current+content)
}
