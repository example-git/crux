package automemory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

type Scope string

const (
	ScopeProject Scope = "project"
	ScopeUser    Scope = "user"
)

type Entry struct {
	File        string `json:"file"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Type        string `json:"type"`
	Content     string `json:"content,omitempty"`
}

type Service struct {
	workingDirectory string
}

const maxMemoryFileBytes = maxMemoryContentBytes + 4096

var mutationMu sync.Mutex

func NewService(workingDirectory string) *Service {
	return &Service{workingDirectory: workingDirectory}
}

func (s *Service) List(ctx context.Context, scope Scope) ([]Entry, error) {
	memory, err := s.resolve(ctx, scope, false)
	if err != nil {
		return nil, err
	}
	topics, err := scanTopics(memory.Directory)
	if err != nil {
		return nil, fmt.Errorf("listing %s memories: %w", scope, err)
	}
	entries := make([]Entry, 0, len(topics))
	for _, topic := range topics {
		entries = append(entries, Entry{
			File:        filepath.Base(topic.Path),
			Name:        topic.Name,
			Description: topic.Description,
			Type:        topic.Type,
		})
	}
	slices.SortFunc(entries, func(left, right Entry) int {
		return strings.Compare(left.File, right.File)
	})
	return entries, nil
}

func (s *Service) Get(ctx context.Context, scope Scope, topic string) (Entry, error) {
	memory, err := s.resolve(ctx, scope, false)
	if err != nil {
		return Entry{}, err
	}
	file, err := normalizeTopicFile(topic)
	if err != nil {
		return Entry{}, err
	}
	path := filepath.Join(memory.Directory, file)
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Entry{}, fmt.Errorf("%s memory %q does not exist", scope, file)
		}
		return Entry{}, fmt.Errorf("reading %s memory %q: %w", scope, file, err)
	}
	if len(content) > maxMemoryFileBytes {
		return Entry{}, fmt.Errorf("%s memory %q exceeds the %d-byte service limit", scope, file, maxMemoryFileBytes)
	}
	name, description, memoryType := parseFrontmatter(content)
	if name == "" || description == "" || memoryType == "" {
		return Entry{}, fmt.Errorf("%s memory %q has invalid frontmatter", scope, file)
	}
	return Entry{
		File:        file,
		Name:        name,
		Description: description,
		Type:        memoryType,
		Content:     memoryBody(string(content)),
	}, nil
}

func (s *Service) Upsert(ctx context.Context, scope Scope, entry Entry) (Entry, error) {
	memory, err := s.resolve(ctx, scope, true)
	if err != nil {
		return Entry{}, err
	}
	file, err := normalizeTopicFile(entry.File)
	if err != nil {
		return Entry{}, err
	}
	mutation := memoryMutation{
		File:        file,
		Action:      "upsert",
		Name:        entry.Name,
		Description: entry.Description,
		Type:        entry.Type,
		Content:     entry.Content,
	}
	if err := applyMemoryMutations(memory, []memoryMutation{mutation}); err != nil {
		return Entry{}, fmt.Errorf("saving %s memory %q: %w", scope, file, err)
	}
	return s.Get(ctx, scope, file)
}

func (s *Service) Remove(ctx context.Context, scope Scope, topic string) error {
	memory, err := s.resolve(ctx, scope, true)
	if err != nil {
		return err
	}
	file, err := normalizeTopicFile(topic)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(memory.Directory, file)); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s memory %q does not exist", scope, file)
		}
		return fmt.Errorf("checking %s memory %q: %w", scope, file, err)
	}
	if err := applyMemoryMutations(memory, []memoryMutation{{File: file, Action: "delete"}}); err != nil {
		return fmt.Errorf("removing %s memory %q: %w", scope, file, err)
	}
	return nil
}

func (s *Service) resolve(ctx context.Context, scope Scope, requireManaged bool) (Memory, error) {
	disabled, err := disabled()
	if err != nil {
		return Memory{}, err
	}
	if disabled {
		return Memory{}, fmt.Errorf("persistent memory is disabled")
	}
	var memory Memory
	switch scope {
	case ScopeProject:
		directory, managed, err := Directory(ctx, s.workingDirectory)
		if err != nil {
			return Memory{}, err
		}
		memory = Memory{Directory: directory, Entrypoint: filepath.Join(directory, EntrypointName), Managed: managed}
	case ScopeUser:
		directory := UserDirectory()
		memory = Memory{Directory: directory, Entrypoint: filepath.Join(directory, EntrypointName), Managed: true}
	default:
		return Memory{}, fmt.Errorf("invalid memory scope %q: expected project or user", scope)
	}
	if requireManaged && !memory.Managed {
		return Memory{}, fmt.Errorf("typed memory mutations are unavailable for custom memory directories")
	}
	if err := os.MkdirAll(memory.Directory, 0o700); err != nil {
		return Memory{}, fmt.Errorf("creating %s memory directory: %w", scope, err)
	}
	return memory, nil
}

func normalizeTopicFile(value string) (string, error) {
	value = strings.TrimSpace(value)
	if filepath.Ext(value) == "" {
		value += ".md"
	}
	if !safeTopicName(value) {
		return "", fmt.Errorf("invalid memory topic %q: use letters, numbers, hyphens, or underscores", value)
	}
	return value, nil
}

func memoryBody(content string) string {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return strings.TrimSpace(content)
	}
	for index := 1; index < len(lines); index++ {
		if strings.TrimSpace(lines[index]) == "---" {
			return strings.TrimSpace(strings.Join(lines[index+1:], "\n"))
		}
	}
	return ""
}

func applyMemoryMutations(memory Memory, mutations []memoryMutation) error {
	mutationMu.Lock()
	defer mutationMu.Unlock()
	if len(mutations) > maxMutationCount {
		return fmt.Errorf("memory response contains too many mutations")
	}
	normalized := make([]memoryMutation, len(mutations))
	for index, mutation := range mutations {
		if !safeTopicName(mutation.File) {
			return fmt.Errorf("invalid memory topic name %q", mutation.File)
		}
		switch mutation.Action {
		case "delete":
		case "upsert":
			if !slices.Contains([]string{"user", "feedback", "project", "reference"}, mutation.Type) {
				return fmt.Errorf("invalid memory type %q", mutation.Type)
			}
			mutation.Name = oneLine(mutation.Name, 120)
			mutation.Description = oneLine(mutation.Description, 200)
			mutation.Content = strings.TrimSpace(mutation.Content)
			if mutation.Name == "" || mutation.Description == "" || mutation.Content == "" || len(mutation.Content) > maxMemoryContentBytes {
				return fmt.Errorf("invalid memory content for %q", mutation.File)
			}
		default:
			return fmt.Errorf("invalid memory action %q", mutation.Action)
		}
		normalized[index] = mutation
	}
	for _, mutation := range normalized {
		path := filepath.Join(memory.Directory, mutation.File)
		switch mutation.Action {
		case "delete":
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		case "upsert":
			content := fmt.Sprintf("---\nname: %s\ndescription: %s\ntype: %s\n---\n\n%s\n", mutation.Name, mutation.Description, mutation.Type, mutation.Content)
			if err := atomicWrite(path, []byte(content), 0o600); err != nil {
				return err
			}
		}
	}
	return rebuildMemoryIndex(memory)
}

func rebuildMemoryIndex(memory Memory) error {
	topics, err := scanTopics(memory.Directory)
	if err != nil {
		return err
	}
	slices.SortFunc(topics, func(left, right Topic) int {
		return strings.Compare(filepath.Base(left.Path), filepath.Base(right.Path))
	})
	var builder strings.Builder
	for _, topic := range topics {
		fmt.Fprintf(&builder, "- [%s](%s) - %s\n", oneLine(topic.Name, 120), filepath.Base(topic.Path), oneLine(topic.Description, 200))
	}
	return atomicWrite(memory.Entrypoint, []byte(builder.String()), 0o600)
}
