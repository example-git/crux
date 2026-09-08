package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/strikethrough"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/table"
	"github.com/PuerkitoBio/goquery"
	"github.com/example-git/crux/internal/question"
	"github.com/example-git/crux/internal/redact"
	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

// BrowserUserAgent is a realistic browser User-Agent for better compatibility.
const BrowserUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

const maxFetchInputSize = 5 * 1024 * 1024

// FetchURLAndConvert fetches a URL and converts HTML content to markdown.
func FetchURLAndConvert(ctx context.Context, client *http.Client, address string) (string, error) {
	return fetchURLAndConvert(ctx, client, address, false)
}

type FetchIdentity struct {
	Mode        string
	Browser     *BrowserFetchService
	SessionID   string
	Environment []string
}

func FetchURLAndConvertWithIdentity(ctx context.Context, client *http.Client, address, toolCallID string, identity FetchIdentity) (content string, err error) {
	if identity.Mode != "" && identity.Mode != "normal" && identity.Mode != "user" {
		return "", errors.New("mode must be normal or user")
	}
	if identity.Mode != "user" {
		return FetchURLAndConvert(ctx, client, address)
	}
	defer func() {
		content = redact.String(content)
		if err != nil && ctx.Err() == nil && !errors.Is(err, question.ErrCancelled) {
			err = errors.New(redact.String(err.Error()))
		}
	}()
	target, err := url.Parse(address)
	if err != nil || target.Hostname() == "" || (target.Scheme != "http" && target.Scheme != "https") {
		return "", errors.New("URL must be a valid HTTP or HTTPS URL")
	}
	if target.User != nil {
		return "", errors.New("user-mode URLs must not contain embedded credentials")
	}
	if identity.SessionID == "" {
		return "", errors.New("session ID is required for fetching a URL")
	}
	requestClient, err := identity.Browser.client(ctx, identity.SessionID, toolCallID, address, identity.Environment, client)
	if err != nil {
		return "", err
	}
	return fetchURLAndConvert(ctx, requestClient, address, true)
}

func fetchURLAndConvert(ctx context.Context, client *http.Client, address string, userMode bool) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", address, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("User-Agent", BrowserUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.5")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to fetch URL: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("request failed with status code: %d", resp.StatusCode)
	}

	content, isHTML, err := readFetchContent(resp)
	if err != nil {
		return "", err
	}
	if userMode {
		content = redact.String(content)
	}
	if isHTML {
		content, err = convertFetchHTML(ctx, content, resp.Request.URL.String())
		if err != nil {
			return "", fmt.Errorf("failed to convert HTML to markdown: %w", err)
		}
	} else if contentType := resp.Header.Get("Content-Type"); strings.Contains(contentType, "application/json") || strings.Contains(contentType, "text/json") {
		if formatted, err := FormatJSON(content); err == nil {
			content = formatted
		}
	}
	return content, nil
}

func readFetchContent(resp *http.Response) (string, bool, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchInputSize+1))
	if err != nil {
		return "", false, fmt.Errorf("failed to read response body: %w", err)
	}
	if len(body) > maxFetchInputSize {
		return "", false, errors.New("response exceeds the 5MB input limit")
	}
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = http.DetectContentType(body)
	}
	mediaType, _, _ := mime.ParseMediaType(contentType)
	isHTML := mediaType == "text/html" || mediaType == "application/xhtml+xml"
	if isHTML {
		reader, err := charset.NewReader(bytes.NewReader(body), contentType)
		if err != nil {
			return "", true, fmt.Errorf("failed to decode HTML charset: %w", err)
		}
		body, err = io.ReadAll(reader)
		if err != nil {
			return "", true, fmt.Errorf("failed to decode HTML: %w", err)
		}
	}
	if !utf8.Valid(body) {
		return "", isHTML, errors.New("response content is not valid UTF-8")
	}
	return string(body), isHTML, nil
}

func parseFetchHTML(content string) (*goquery.Document, error) {
	root, err := html.ParseWithOptions(strings.NewReader(content), html.ParseOptionEnableScripting(false))
	if err != nil {
		return nil, err
	}
	doc := goquery.NewDocumentFromNode(root)
	doc.Find("script, style, template, iframe, object, embed, [hidden], [aria-hidden='true']").Remove()
	doc.Find("img").Each(func(_ int, image *goquery.Selection) {
		source, _ := image.Attr("src")
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(source)), "data:") {
			alt, _ := image.Attr("alt")
			image.ReplaceWithNodes(&html.Node{Type: html.TextNode, Data: alt})
		}
	})
	doc.Find("svg").Each(func(_ int, image *goquery.Selection) {
		image.ReplaceWithNodes(&html.Node{Type: html.TextNode, Data: image.Find("title, desc").Text()})
	})
	return doc, nil
}

// ConvertHTMLToMarkdown converts HTML content to markdown format.
func ConvertHTMLToMarkdown(htmlContent string) (string, error) {
	return convertFetchHTML(context.Background(), htmlContent, "")
}

func convertFetchHTML(ctx context.Context, content, pageURL string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	doc, err := parseFetchHTML(content)
	if err != nil {
		return "", err
	}
	if href, ok := doc.Find("base[href]").First().Attr("href"); ok {
		if baseURL, err := url.Parse(pageURL); err == nil {
			if reference, err := url.Parse(href); err == nil {
				resolved := baseURL.ResolveReference(reference)
				if resolved.Scheme == "http" || resolved.Scheme == "https" {
					pageURL = resolved.String()
				}
			}
		}
	}
	doc.Find("head").Remove()
	conv := converter.NewConverter(converter.WithPlugins(
		base.NewBasePlugin(),
		commonmark.NewCommonmarkPlugin(),
		table.NewTablePlugin(),
		strikethrough.NewStrikethroughPlugin(),
	))
	conv.Register.TagType("noscript", converter.TagTypeBlock, converter.PriorityEarly)
	markdown, err := conv.ConvertNode(doc.Nodes[0], converter.WithDomain(pageURL), converter.WithContext(ctx))
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return strings.TrimSpace(string(markdown)), nil
}

// FormatJSON formats JSON content with proper indentation.
func FormatJSON(content string) (string, error) {
	var data any
	if err := json.Unmarshal([]byte(content), &data); err != nil {
		return "", err
	}

	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(data); err != nil {
		return "", err
	}

	return buf.String(), nil
}
