package tools

import (
	"cmp"
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/x/ansi"
	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/permission"
)

const (
	SearchToolName    = "search"
	SearchModeFiles   = "files"
	SearchModeContent = "content"
	maxSearchResults  = 100
)

//go:embed search.md.tpl
var workspaceSearchDescriptionTmpl []byte

var workspaceSearchDescriptionTpl = template.Must(
	template.New("workspaceSearchDescription").
		Parse(string(workspaceSearchDescriptionTmpl)),
)

type workspaceSearchDescriptionData struct {
	MaxResults int
}

func workspaceSearchDescription() string {
	return renderTemplate(workspaceSearchDescriptionTpl, workspaceSearchDescriptionData{
		MaxResults: maxSearchResults,
	})
}

type SearchParams struct {
	Mode        string `json:"mode" enum:"files,content" description:"Search mode: files matches file paths by glob pattern; content searches inside files by regex or literal text"`
	Pattern     string `json:"pattern" description:"The file glob pattern in files mode, or content regex or literal text in content mode"`
	Path        string `json:"path,omitempty" description:"The directory to search in. Defaults to the current working directory."`
	Include     string `json:"include,omitempty" description:"Content mode only: file pattern to include in the search (e.g. \"*.js\", \"*.{ts,tsx}\")"`
	LiteralText bool   `json:"literal_text,omitempty" description:"Content mode only: treat pattern as literal text instead of a regex"`
}

type SearchResponseMetadata struct {
	Mode            string `json:"mode"`
	NumberOfFiles   int    `json:"number_of_files,omitempty"`
	NumberOfMatches int    `json:"number_of_matches,omitempty"`
	Truncated       bool   `json:"truncated"`
}

func NewSearchTool(permissions permission.Service, workingDir string, cfg config.ToolSearch) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		SearchToolName,
		workspaceSearchDescription(),
		func(ctx context.Context, params SearchParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Mode != SearchModeFiles && params.Mode != SearchModeContent {
				return fantasy.NewTextErrorResponse(`mode must be "files" or "content"`), nil
			}
			if params.Pattern == "" {
				return fantasy.NewTextErrorResponse("pattern is required"), nil
			}
			if params.Mode == SearchModeFiles && params.Include != "" {
				return fantasy.NewTextErrorResponse("include is only valid in content mode"), nil
			}
			if params.Mode == SearchModeFiles && params.LiteralText {
				return fantasy.NewTextErrorResponse("literal_text is only valid in content mode"), nil
			}

			searchPath, err := canonicalToolPath(workingDir, cmp.Or(params.Path, workingDir))
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			action := "list"
			description := fmt.Sprintf("Search files outside working directory: %s", searchPath)
			timeout := cfg.GetFilesTimeout()
			if params.Mode == SearchModeContent {
				action = "read"
				description = fmt.Sprintf("Search file contents outside working directory: %s", searchPath)
				timeout = cfg.GetContentTimeout()
			}
			granted, err := authorizeExternalPath(ctx, permissions, workingDir, searchPath, call.ID, SearchToolName, action, description, params)
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if !granted {
				return NewPermissionDeniedResponse(), nil
			}

			searchCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			if params.Mode == SearchModeFiles {
				return searchFilePaths(searchCtx, params, searchPath)
			}
			return searchFileContents(searchCtx, params, searchPath)
		},
	)
}

func searchFilePaths(ctx context.Context, params SearchParams, searchPath string) (fantasy.ToolResponse, error) {
	files, truncated, err := globFiles(ctx, params.Pattern, searchPath, maxSearchResults)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("error finding files: %v", err)), nil
	}

	var output string
	if len(files) == 0 {
		output = "No files found"
	} else {
		normalizeFilePaths(files)
		output = strings.Join(files, "\n")
		if truncated {
			output += "\n\n(Results are truncated. Consider using a more specific path or pattern.)"
		}
	}

	return fantasy.WithResponseMetadata(
		fantasy.NewTextResponse(output),
		SearchResponseMetadata{
			Mode:          SearchModeFiles,
			NumberOfFiles: len(files),
			Truncated:     truncated,
		},
	), nil
}

func searchFileContents(ctx context.Context, params SearchParams, searchPath string) (fantasy.ToolResponse, error) {
	searchPattern := params.Pattern
	if params.LiteralText {
		searchPattern = escapeRegexPattern(params.Pattern)
	}

	matches, truncated, err := searchFiles(ctx, searchPattern, searchPath, params.Include, maxSearchResults)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("error searching files: %v", err)), nil
	}

	var output strings.Builder
	if len(matches) == 0 {
		output.WriteString("No files found")
	} else {
		fmt.Fprintf(&output, "Found %d matches\n", len(matches))

		currentFile := ""
		for _, match := range matches {
			if currentFile != match.path {
				if currentFile != "" {
					output.WriteString("\n")
				}
				currentFile = match.path
				fmt.Fprintf(&output, "%s:\n", filepath.ToSlash(match.path))
			}
			if match.lineNum > 0 {
				lineText := match.lineText
				if ansi.StringWidth(lineText) > maxSearchContentWidth {
					lineText = ansi.Truncate(lineText, maxSearchContentWidth, "...")
				}
				if match.charNum > 0 {
					fmt.Fprintf(&output, "  Line %d, Char %d: %s\n", match.lineNum, match.charNum, lineText)
				} else {
					fmt.Fprintf(&output, "  Line %d: %s\n", match.lineNum, lineText)
				}
			} else {
				fmt.Fprintf(&output, "  %s\n", match.path)
			}
		}

		if truncated {
			output.WriteString("\n(Results are truncated. Consider using a more specific path or pattern.)")
		}
	}

	return fantasy.WithResponseMetadata(
		fantasy.NewTextResponse(output.String()),
		SearchResponseMetadata{
			Mode:            SearchModeContent,
			NumberOfMatches: len(matches),
			Truncated:       truncated,
		},
	), nil
}
