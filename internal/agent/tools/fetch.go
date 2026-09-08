package tools

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/PuerkitoBio/goquery"
	fantasy "github.com/example-git/crux/foundation"
	cruxlog "github.com/example-git/crux/internal/log"
	"github.com/example-git/crux/internal/permission"
	"github.com/example-git/crux/internal/question"
	"github.com/example-git/crux/internal/redact"
)

const (
	FetchToolName = "fetch"
	MaxFetchSize  = 100 * 1024 // 100KB
)

//go:embed fetch.md.tpl
var fetchDescriptionTmpl []byte

var fetchDescriptionTpl = template.Must(
	template.New("fetchDescription").
		Parse(string(fetchDescriptionTmpl)),
)

type fetchDescriptionData struct {
	GhAvailable    bool
	MaxFetchSizeKB int
}

func fetchDescription() string {
	return renderTemplate(fetchDescriptionTpl, fetchDescriptionData{
		GhAvailable:    ghAvailable,
		MaxFetchSizeKB: MaxFetchSize / 1024,
	})
}

func NewFetchTool(permissions permission.Service, workingDir string, client *http.Client, browser *BrowserFetchService, environment []string) fantasy.AgentTool {
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
		FetchToolName,
		fetchDescription(),
		func(ctx context.Context, params FetchParams, call fantasy.ToolCall) (response fantasy.ToolResponse, err error) {
			if params.Mode != "" && params.Mode != "normal" && params.Mode != "user" {
				return fantasy.NewTextErrorResponse("Mode must be normal or user"), nil
			}
			defer func() {
				if ctx.Err() != nil {
					err = ctx.Err()
				} else if err != nil {
					response = fantasy.NewTextErrorResponse(err.Error())
					err = nil
				}
				if params.Mode == "user" {
					response.Content = redact.String(response.Content)
				}
			}()
			if params.URL == "" {
				return fantasy.NewTextErrorResponse("URL parameter is required"), nil
			}

			format := strings.ToLower(params.Format)
			if format == "" {
				format = "markdown"
			}
			if format != "text" && format != "markdown" && format != "html" {
				return fantasy.NewTextErrorResponse("Format must be one of: text, markdown, html"), nil
			}

			target, parseErr := url.Parse(params.URL)
			if parseErr != nil || target.Hostname() == "" || (target.Scheme != "http" && target.Scheme != "https") {
				return fantasy.NewTextErrorResponse("URL must be a valid HTTP or HTTPS URL"), nil
			}
			if params.Mode == "user" && target.User != nil {
				return fantasy.NewTextErrorResponse("User-mode URLs must not contain embedded credentials"), nil
			}

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for fetching a URL")
			}

			requestClient := client
			if params.Mode == "user" {
				requestClient, err = browser.client(ctx, sessionID, call.ID, params.URL, environment, client)
				if errors.Is(err, question.ErrCancelled) {
					return NewPermissionDeniedResponse(), nil
				}
				if err != nil {
					return fantasy.NewTextErrorResponse(err.Error()), nil
				}
			} else {
				p, err := permissions.Request(
					ctx,
					permission.CreatePermissionRequest{
						SessionID:   sessionID,
						Path:        workingDir,
						ToolCallID:  call.ID,
						ToolName:    FetchToolName,
						Action:      "fetch",
						Description: fmt.Sprintf("Fetch content from URL: %s", params.URL),
						Params:      FetchPermissionsParams(params),
					},
				)
				if err != nil {
					return fantasy.ToolResponse{}, err
				}
				if !p {
					return NewPermissionDeniedResponse(), nil
				}
			}

			// maxFetchTimeoutSeconds is the maximum allowed timeout for fetch requests (2 minutes)
			const maxFetchTimeoutSeconds = 120

			// Handle timeout with context
			requestCtx := ctx
			if params.Timeout > 0 {
				if params.Timeout > maxFetchTimeoutSeconds {
					params.Timeout = maxFetchTimeoutSeconds
				}
				var cancel context.CancelFunc
				requestCtx, cancel = context.WithTimeout(ctx, time.Duration(params.Timeout)*time.Second)
				defer cancel()
			}

			req, err := http.NewRequestWithContext(requestCtx, "GET", params.URL, nil)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to create request: %w", err)
			}

			req.Header.Set("User-Agent", "crux/1.0")

			resp, err := requestClient.Do(req)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to fetch URL: %w", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Request failed with status code: %d", resp.StatusCode)), nil
			}

			content, isHTML, err := readFetchContent(resp)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			if params.Mode == "user" {
				content = redact.String(content)
			}

			switch format {
			case "text":
				if isHTML {
					text, err := extractTextFromHTML(content)
					if err != nil {
						return fantasy.NewTextErrorResponse("Failed to extract text from HTML: " + err.Error()), nil
					}
					content = text
				}

			case "markdown":
				if isHTML {
					markdown, err := convertFetchHTML(requestCtx, content, resp.Request.URL.String())
					if err != nil {
						return fantasy.NewTextErrorResponse("Failed to convert HTML to Markdown: " + err.Error()), nil
					}
					content = markdown
				}

			case "html":
				// return only the body of the HTML document
				if isHTML {
					doc, err := goquery.NewDocumentFromReader(strings.NewReader(content))
					if err != nil {
						return fantasy.NewTextErrorResponse("Failed to parse HTML: " + err.Error()), nil
					}
					body, err := doc.Find("body").Html()
					if err != nil {
						return fantasy.NewTextErrorResponse("Failed to extract body from HTML: " + err.Error()), nil
					}
					if body == "" {
						return fantasy.NewTextErrorResponse("No body content found in HTML"), nil
					}
					content = "<html>\n<body>\n" + body + "\n</body>\n</html>"
				}
			}
			if len(content) > MaxFetchSize {
				notice := fmt.Sprintf("\n\n[Content truncated to %d bytes]", MaxFetchSize)
				end := MaxFetchSize - len(notice)
				for !utf8.RuneStart(content[end]) {
					end--
				}
				content = content[:end] + notice
			}

			return fantasy.NewTextResponse(content), nil
		},
	)
}

func extractTextFromHTML(html string) (string, error) {
	doc, err := parseFetchHTML(html)
	if err != nil {
		return "", err
	}

	text := doc.Find("body").Text()
	text = strings.Join(strings.Fields(text), " ")

	return text, nil
}
