package automemory

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxTopicFiles       = 200
	maxRelevantMemories = 5
	maxTopicLines       = 200
	maxTopicBytes       = 4096
)

type Topic struct {
	Path        string
	Name        string
	Description string
	Type        string
	ModifiedAt  time.Time
}

var wordPattern = regexp.MustCompile(`[\pL\pN]+`)

func Relevant(ctx context.Context, workingDir, query string, now time.Time) (string, error) {
	memory, err := Load(ctx, workingDir)
	if err != nil || memory.Directory == "" {
		return "", err
	}
	topics, err := scanMemoryTopics(memory)
	if err != nil {
		return "", err
	}

	var builder strings.Builder
	for _, topic := range relevantTopics(topics, query, maxRelevantMemories) {
		content, readErr := readTopic(topic.Path)
		if readErr != nil {
			continue
		}
		if builder.Len() == 0 {
			builder.WriteString("<relevant_memories>\n")
		}
		age := memoryAge(now, topic.ModifiedAt)
		if age > 1 {
			fmt.Fprintf(&builder, "Memory saved %d days ago is a point-in-time observation. Verify current code, behavior, and file references before relying on it.\n", age)
		}
		fmt.Fprintf(&builder, "<memory path=%q saved=%q>\n%s\n</memory>\n", topic.Path, ageLabel(age), content)
	}
	if builder.Len() == 0 {
		return "", nil
	}
	builder.WriteString("</relevant_memories>")
	return builder.String(), nil
}

func scanMemoryTopics(memory Memory) ([]Topic, error) {
	var topics []Topic
	for _, directory := range []string{memory.Directory, memory.UserDirectory} {
		if directory == "" {
			continue
		}
		directoryTopics, err := scanTopics(directory)
		if err != nil {
			return nil, err
		}
		topics = append(topics, directoryTopics...)
	}
	return topics, nil
}

func relevantTopics(topics []Topic, query string, limit int) []Topic {
	words := queryWords(query)
	if len(words) == 0 || limit <= 0 {
		return nil
	}
	type scoredTopic struct {
		topic Topic
		score int
	}
	var scored []scoredTopic
	for _, topic := range topics {
		haystack := queryWords(topic.Name + " " + topic.Description + " " + filepath.Base(topic.Path))
		score := 0
		for word := range words {
			if haystack[word] {
				score++
			}
		}
		if score > 0 {
			scored = append(scored, scoredTopic{topic: topic, score: score})
		}
	}
	slices.SortFunc(scored, func(left, right scoredTopic) int {
		if left.score != right.score {
			return right.score - left.score
		}
		if !left.topic.ModifiedAt.Equal(right.topic.ModifiedAt) {
			if left.topic.ModifiedAt.After(right.topic.ModifiedAt) {
				return -1
			}
			return 1
		}
		return strings.Compare(left.topic.Path, right.topic.Path)
	})
	if len(scored) > limit {
		scored = scored[:limit]
	}
	selected := make([]Topic, len(scored))
	for index, value := range scored {
		selected[index] = value.topic
	}
	return selected
}

func scanTopics(directory string) ([]Topic, error) {
	if directory == "" {
		return nil, nil
	}
	var topics []Topic
	err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.EqualFold(filepath.Ext(entry.Name()), ".md") || strings.EqualFold(entry.Name(), EntrypointName) {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		header, err := readPrefix(path, 30, 8192)
		if err != nil {
			return nil
		}
		name, description, memoryType := parseFrontmatter(header)
		if name == "" || description == "" {
			return nil
		}
		topics = append(topics, Topic{
			Path:        path,
			Name:        oneLine(name, 120),
			Description: oneLine(description, 200),
			Type:        oneLine(memoryType, 20),
			ModifiedAt:  info.ModTime(),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(topics, func(left, right Topic) int {
		if left.ModifiedAt.After(right.ModifiedAt) {
			return -1
		}
		if right.ModifiedAt.After(left.ModifiedAt) {
			return 1
		}
		return strings.Compare(left.Path, right.Path)
	})
	if len(topics) > maxTopicFiles {
		topics = topics[:maxTopicFiles]
	}
	return topics, nil
}

func readTopic(path string) (string, error) {
	content, err := readPrefix(path, maxTopicLines, maxTopicBytes)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err == nil && (info.Size() > maxTopicBytes || bytes.Count(content, []byte("\n")) >= maxTopicLines) {
		marker := []byte("\n\n[... memory truncated; use view for the complete file ...]")
		limit := maxTopicBytes - len(marker)
		if len(content) > limit {
			content = content[:limit]
			for !utf8.Valid(content) && len(content) > 0 {
				content = content[:len(content)-1]
			}
		}
		content = append(content, marker...)
	}
	return string(content), nil
}

func readPrefix(path string, maxLines, maxBytes int) ([]byte, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := bytes.Split(content, []byte("\n"))
	if len(lines) > maxLines {
		lines = lines[:maxLines]
	}
	content = bytes.Join(lines, []byte("\n"))
	if len(content) > maxBytes {
		content = content[:maxBytes]
		for !utf8.Valid(content) && len(content) > 0 {
			content = content[:len(content)-1]
		}
	}
	return bytes.TrimSpace(content), nil
}

func parseFrontmatter(content []byte) (name, description, memoryType string) {
	lines := strings.Split(string(content), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return "", "", ""
	}
	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if line == "---" {
			break
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		switch strings.TrimSpace(key) {
		case "name":
			name = value
		case "description":
			description = value
		case "type":
			memoryType = value
		}
	}
	return name, description, memoryType
}

func queryWords(value string) map[string]bool {
	words := make(map[string]bool)
	for _, match := range wordPattern.FindAllString(strings.ToLower(value), -1) {
		if utf8.RuneCountInString(match) < 3 || isStopWord(match) {
			continue
		}
		words[match] = true
	}
	return words
}

func isStopWord(word string) bool {
	return slices.Contains([]string{"and", "are", "but", "for", "from", "have", "how", "that", "the", "this", "was", "what", "when", "where", "with", "you", "your"}, word)
}

func memoryAge(now, modifiedAt time.Time) int {
	days := int(now.Sub(modifiedAt).Hours() / 24)
	if days < 0 {
		return 0
	}
	return days
}

func ageLabel(days int) string {
	switch days {
	case 0:
		return "today"
	case 1:
		return "yesterday"
	default:
		return fmt.Sprintf("%d days ago", days)
	}
}

func safeTopicName(value string) bool {
	if value == "" || value != filepath.Base(value) || !strings.EqualFold(filepath.Ext(value), ".md") || strings.EqualFold(value, EntrypointName) {
		return false
	}
	base := strings.TrimSuffix(value, filepath.Ext(value))
	for _, char := range base {
		if !unicode.IsLetter(char) && !unicode.IsDigit(char) && char != '-' && char != '_' {
			return false
		}
	}
	return base != ""
}
