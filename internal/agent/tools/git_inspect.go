package tools

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	fantasy "github.com/example-git/crux/foundation"
)

const GitInspectToolName = "git_inspect"

var safeGitRevision = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/@{}~^:+-]*$`)

//go:embed git_inspect.md
var gitInspectDescription string

type GitInspectParams struct {
	Action   string `json:"action" description:"Read-only Git action: status, diff, log, or show"`
	Staged   bool   `json:"staged,omitempty" description:"For diff, inspect staged changes instead of the working tree"`
	Path     string `json:"path,omitempty" description:"For status or diff, optionally restrict output to a workspace-relative path"`
	Revision string `json:"revision,omitempty" description:"For show, the revision or object to inspect"`
	Limit    int    `json:"limit,omitempty" description:"For log, maximum commits from 1 to 100 (default 20)"`
}

func NewGitInspectTool(workingDir string) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		GitInspectToolName,
		gitInspectDescription,
		func(ctx context.Context, params GitInspectParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			args, err := gitInspectArgs(params)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			command := exec.CommandContext(ctx, "git", args...)
			command.Dir = workingDir
			command.Env = append(os.Environ(), "GIT_PAGER=cat", "GIT_TERMINAL_PROMPT=0")
			output, err := command.CombinedOutput()
			text := strings.TrimSpace(string(output))
			if err != nil {
				if text == "" {
					text = err.Error()
				}
				return fantasy.NewTextErrorResponse(TruncateOutput(text)), nil
			}
			if text == "" {
				text = "Git returned no output."
			}
			return fantasy.NewTextResponse(TruncateOutput(text)), nil
		},
	)
}

func gitInspectArgs(params GitInspectParams) ([]string, error) {
	path, err := safeGitPath(params.Path)
	if err != nil {
		return nil, err
	}
	base := []string{"-c", "core.pager=cat", "-c", "diff.external=", "-c", "diff.trustExitCode=false"}
	switch strings.TrimSpace(params.Action) {
	case "status":
		args := append(base, "status", "--short", "--branch", "--untracked-files=all")
		if path != "" {
			args = append(args, "--", path)
		}
		return args, nil
	case "diff":
		args := append(base, "diff", "--no-ext-diff", "--no-textconv", "--unified=3")
		if params.Staged {
			args = append(args, "--cached")
		}
		if path != "" {
			args = append(args, "--", path)
		}
		return args, nil
	case "log":
		if path != "" || params.Staged || strings.TrimSpace(params.Revision) != "" {
			return nil, fmt.Errorf("log accepts only limit")
		}
		limit := params.Limit
		if limit == 0 {
			limit = 20
		}
		if limit < 1 || limit > 100 {
			return nil, fmt.Errorf("log limit must be between 1 and 100")
		}
		return append(base, "log", "--oneline", "--decorate", fmt.Sprintf("-%d", limit)), nil
	case "show":
		if path != "" || params.Staged || params.Limit != 0 {
			return nil, fmt.Errorf("show accepts only revision")
		}
		revision := strings.TrimSpace(params.Revision)
		if revision == "" {
			return nil, fmt.Errorf("revision is required for show")
		}
		if !safeGitRevision.MatchString(revision) {
			return nil, fmt.Errorf("invalid Git revision %q", revision)
		}
		return append(base, "show", "--no-ext-diff", "--no-textconv", "--format=fuller", "--stat", "--patch", revision), nil
	default:
		return nil, fmt.Errorf("invalid Git action %q: expected status, diff, log, or show", params.Action)
	}
}

func safeGitPath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	cleaned := filepath.Clean(value)
	if filepath.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("Git path must stay within the current workspace")
	}
	return cleaned, nil
}
