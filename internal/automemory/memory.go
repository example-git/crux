package automemory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/example-git/crux/internal/config"
)

const (
	EntrypointName     = "MEMORY.md"
	MaxEntrypointLines = 200
	MaxEntrypointBytes = 25_000
)

var unsafePathCharacter = regexp.MustCompile(`[^a-zA-Z0-9]`)

type ConfigurationError struct {
	Err error
}

func (e *ConfigurationError) Error() string {
	return e.Err.Error()
}

func (e *ConfigurationError) Unwrap() error {
	return e.Err
}

type Memory struct {
	Directory      string
	Entrypoint     string
	Content        string
	Managed        bool
	UserDirectory  string
	UserEntrypoint string
	UserContent    string
}

func Load(ctx context.Context, workingDir string) (Memory, error) {
	disabled, err := disabled()
	if err != nil {
		return Memory{}, err
	}
	if disabled {
		return Memory{}, nil
	}

	directory, managed, err := Directory(ctx, workingDir)
	if err != nil {
		return Memory{}, err
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return Memory{}, fmt.Errorf("creating auto-memory directory %q: %w", directory, err)
	}

	entrypoint := filepath.Join(directory, EntrypointName)
	content, err := readEntrypoint(entrypoint)
	if err != nil {
		return Memory{}, err
	}
	userDirectory := UserDirectory()
	if err := os.MkdirAll(userDirectory, 0o700); err != nil {
		return Memory{}, fmt.Errorf("creating user memory directory %q: %w", userDirectory, err)
	}
	userEntrypoint := filepath.Join(userDirectory, EntrypointName)
	userContent, err := readEntrypoint(userEntrypoint)
	if err != nil {
		return Memory{}, err
	}

	return Memory{
		Directory:      directory,
		Entrypoint:     entrypoint,
		Content:        content,
		Managed:        managed,
		UserDirectory:  userDirectory,
		UserEntrypoint: userEntrypoint,
		UserContent:    userContent,
	}, nil
}

func Directory(ctx context.Context, workingDir string) (string, bool, error) {
	if override := os.Getenv("CRUX_AUTO_MEMORY_DIR"); override != "" {
		if strings.IndexByte(override, 0) >= 0 || !filepath.IsAbs(override) {
			return "", false, &ConfigurationError{Err: fmt.Errorf("CRUX_AUTO_MEMORY_DIR must be a safe absolute path")}
		}
		cleaned := filepath.Clean(override)
		if filepath.Dir(cleaned) == cleaned {
			return "", false, &ConfigurationError{Err: fmt.Errorf("CRUX_AUTO_MEMORY_DIR cannot be a filesystem root")}
		}
		return cleaned, false, nil
	}

	root, err := canonicalProjectRoot(ctx, workingDir)
	if err != nil {
		return "", false, err
	}
	return filepath.Join(config.GlobalWorkspaceDir(), "projects", sanitizePath(root), "memory"), true, nil
}

func UserDirectory() string {
	return filepath.Join(config.GlobalWorkspaceDir(), "memory")
}

func readEntrypoint(path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("reading auto-memory index %q: %w", path, err)
	}
	return truncateEntrypoint(content), nil
}

func Prompt(memory Memory) string {
	if memory.Directory == "" {
		return ""
	}

	var builder strings.Builder
	builder.WriteString("<persistent_memory>\n")
	builder.WriteString("# Auto memory\n\n")
	fmt.Fprintf(&builder, "You have persistent project memory at `%s` and user-wide memory at `%s`. Both directories already exist.\n\n", memory.Directory, memory.UserDirectory)
	builder.WriteString("Build this memory over time so future conversations retain durable user preferences, feedback, non-derivable project context, and external references. If the user explicitly asks you to remember something, save it immediately. If they ask you to forget something, remove or update the relevant memory.\n\n")
	builder.WriteString("## Memory types\n\n")
	builder.WriteString("- `user`: the user's role, goals, responsibilities, knowledge, and collaboration preferences.\n")
	builder.WriteString("- `feedback`: guidance about what to avoid or repeat. Include why and when it applies.\n")
	builder.WriteString("- `project`: ongoing goals, decisions, incidents, deadlines, or motivations not derivable from code or Git history. Convert relative dates to absolute dates.\n")
	builder.WriteString("- `reference`: pointers to authoritative information in external systems and when to consult it.\n\n")
	builder.WriteString("## Do not save\n\n")
	builder.WriteString("Do not save code structure, file paths, architecture, Git history, debugging recipes already represented in code, information already in project instruction files, secrets, or ephemeral task state. Use tasks for current work.\n\n")
	builder.WriteString("## Saving memories\n\n")
	builder.WriteString("Use the memory_list, memory_upsert, and memory_remove tools instead of generic filesystem tools. Save project-specific decisions and references in project scope. Save preferences and feedback that apply across projects in user scope. Before every memory mutation, inspect the target scope with memory_list. If a related memory exists and remains relevant, update that topic to incorporate the new durable information instead of creating a duplicate. Remove memories that are no longer relevant to their scope, stale, or superseded. Each memory uses this frontmatter:\n\n")
	builder.WriteString("```markdown\n---\nname: {{memory name}}\ndescription: {{specific one-line relevance description}}\ntype: {{user|feedback|project|reference}}\n---\n\n{{durable content}}\n```\n\n")
	fmt.Fprintf(&builder, "The memory tools maintain each scope's `%s` index atomically. Keep topics focused and the collection naturally concise. Aim for roughly 30 to 50 memories per scope depending on project complexity; treat that range as a soft target, not a hard limit, and never delete useful memory solely to reach it. Each index remains bounded to %d lines.\n\n", EntrypointName, MaxEntrypointLines)
	builder.WriteString("## Recalling memories\n\n")
	builder.WriteString("The indices below are always loaded. When an entry is relevant, use memory_list with its scope and topic to read it before relying on it. You must consult memory when the user asks you to recall prior work. If the user says to ignore memory, act as if both indices were empty and do not mention remembered content.\n\n")
	builder.WriteString("Memory can become stale. Verify claims about current files, functions, flags, or project state against the repository before recommending action. Trust current evidence over memory and update stale records.\n\n")
	builder.WriteString("## Project MEMORY.md\n\n")
	writeMemoryIndex(&builder, memory.Content)
	builder.WriteString("\n## User MEMORY.md\n\n")
	writeMemoryIndex(&builder, memory.UserContent)
	builder.WriteString("</persistent_memory>")
	return builder.String()
}

func writeMemoryIndex(builder *strings.Builder, content string) {
	if strings.TrimSpace(content) == "" {
		builder.WriteString("The memory index is currently empty.\n")
		return
	}
	builder.WriteString(content)
	builder.WriteByte('\n')
}

func disabled() (bool, error) {
	value, ok := os.LookupEnv("CRUX_DISABLE_AUTO_MEMORY")
	if !ok || value == "" {
		return false, nil
	}
	disabled, err := strconv.ParseBool(value)
	if err != nil {
		return false, &ConfigurationError{Err: fmt.Errorf("CRUX_DISABLE_AUTO_MEMORY must be a boolean: %w", err)}
	}
	return disabled, nil
}

func canonicalProjectRoot(ctx context.Context, workingDir string) (string, error) {
	root, err := filepath.Abs(workingDir)
	if err != nil {
		return "", fmt.Errorf("resolving project directory %q: %w", workingDir, err)
	}
	root = filepath.Clean(root)

	command := exec.CommandContext(ctx, "git", "rev-parse", "--path-format=absolute", "--git-common-dir")
	command.Dir = root
	output, commandErr := command.Output()
	if commandErr != nil {
		return root, nil
	}

	commonDirectory := filepath.Clean(strings.TrimSpace(string(output)))
	if commonDirectory == "" {
		return root, nil
	}
	if !filepath.IsAbs(commonDirectory) {
		commonDirectory = filepath.Join(root, commonDirectory)
	}
	if filepath.Base(commonDirectory) == ".git" {
		return filepath.Dir(commonDirectory), nil
	}
	return root, nil
}

func sanitizePath(path string) string {
	sanitized := unsafePathCharacter.ReplaceAllString(path, "-")
	if len(sanitized) <= 200 {
		return sanitized
	}
	digest := sha256.Sum256([]byte(path))
	return sanitized[:200] + "-" + hex.EncodeToString(digest[:6])
}

func truncateEntrypoint(raw []byte) string {
	trimmed := bytes.TrimSpace(raw)
	lines := bytes.Split(trimmed, []byte("\n"))
	lineTruncated := len(lines) > MaxEntrypointLines
	byteTruncated := len(trimmed) > MaxEntrypointBytes
	if !lineTruncated && !byteTruncated {
		return string(trimmed)
	}

	if lineTruncated {
		lines = lines[:MaxEntrypointLines]
	}
	truncated := bytes.Join(lines, []byte("\n"))
	if len(truncated) > MaxEntrypointBytes {
		cut := bytes.LastIndexByte(truncated[:MaxEntrypointBytes], '\n')
		if cut <= 0 {
			cut = MaxEntrypointBytes
		}
		truncated = truncated[:cut]
		for !utf8.Valid(truncated) && len(truncated) > 0 {
			truncated = truncated[:len(truncated)-1]
		}
	}

	reason := fmt.Sprintf("more than %d lines or %d bytes", MaxEntrypointLines, MaxEntrypointBytes)
	return string(truncated) + fmt.Sprintf("\n\n> WARNING: %s contains %s. Only part was loaded. Keep index entries concise and move details into topic files.", EntrypointName, reason)
}
