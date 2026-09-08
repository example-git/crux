package tools

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"strings"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	cruxlog "github.com/example-git/crux/internal/log"
	"github.com/example-git/crux/internal/question"
	"github.com/example-git/crux/internal/redact"
)

//go:embed web_fetch.md.tpl
var webFetchDescriptionTmpl []byte

var webFetchDescriptionTpl = template.Must(
	template.New("webFetchDescription").
		Parse(string(webFetchDescriptionTmpl)),
)

// NewWebFetchTool creates a simple web fetch tool for sub-agents (no permissions needed).
func NewWebFetchTool(workingDir string, client *http.Client) fantasy.AgentTool {
	return NewWebFetchToolWithIdentity(workingDir, client, FetchIdentity{})
}

func NewWebFetchToolWithIdentity(workingDir string, client *http.Client, identity FetchIdentity) fantasy.AgentTool {
	if client == nil {
		transport := cruxlog.CloneDefaultHTTPTransport()
		transport.MaxIdleConns = 100
		transport.MaxIdleConnsPerHost = 10
		transport.IdleConnTimeout = 90 * time.Second

		client = &http.Client{
			Timeout:   30 * time.Second,
			Transport: cruxlog.WrapHTTPTransport(transport),
		}
	}

	return fantasy.NewParallelAgentTool(
		WebFetchToolName,
		renderToolDescription(webFetchDescriptionTpl),
		func(ctx context.Context, params WebFetchParams, call fantasy.ToolCall) (response fantasy.ToolResponse, err error) {
			if identity.Mode == "user" {
				defer func() { response.Content = redact.String(response.Content) }()
			}
			if params.URL == "" {
				return fantasy.NewTextErrorResponse("url is required"), nil
			}

			content, err := FetchURLAndConvertWithIdentity(ctx, client, params.URL, call.ID, identity)
			if errors.Is(err, question.ErrCancelled) {
				return NewPermissionDeniedResponse(), nil
			}
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to fetch URL: %s", err)), nil
			}

			hasLargeContent := len(content) > LargeContentThreshold
			var result strings.Builder

			if hasLargeContent {
				tempFile, err := os.CreateTemp(workingDir, "page-*.md")
				if err != nil {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to create temporary file: %s", err)), nil
				}
				tempFilePath := tempFile.Name()

				if _, err := tempFile.WriteString(content); err != nil {
					_ = tempFile.Close() // Best effort close
					return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to write content to file: %s", err)), nil
				}
				if err := tempFile.Close(); err != nil {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to close temporary file: %s", err)), nil
				}

				fmt.Fprintf(&result, "Fetched content from %s (large page)\n\n", params.URL)
				fmt.Fprintf(&result, "Content saved to: %s\n\n", tempFilePath)
				result.WriteString("Use view and Search in content mode to analyze this file.")
			} else {
				fmt.Fprintf(&result, "Fetched content from %s:\n\n", params.URL)
				result.WriteString(content)
			}

			return fantasy.NewTextResponse(result.String()), nil
		},
	)
}
