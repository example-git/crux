package providertransport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/example-git/crux/internal/providerplugin/manifest"
)

type ImageWorkflowHost struct {
	budgetMu      sync.Mutex
	budgetErr     error
	requestCount  int
	stepCount     int
	responseBytes int64
	Manifest      manifest.ImageManifest
	Client        *http.Client
	ValidateOwner func() error
	Credentials   map[string]any
	CookieJars    map[string]http.CookieJar
}

func (h *ImageWorkflowHost) charge(requests, steps int, bytes int64) error {
	h.budgetMu.Lock()
	defer h.budgetMu.Unlock()
	if h.budgetErr != nil {
		return h.budgetErr
	}
	requestLimit, stepLimit, byteLimit := h.Manifest.Limits.Requests, h.Manifest.Limits.Steps, h.Manifest.Limits.TotalResponseBytes
	if requestLimit == 0 {
		requestLimit = 512
	}
	if stepLimit == 0 {
		stepLimit = 4096
	}
	if byteLimit == 0 {
		byteLimit = 1 << 30
	}
	if requests > requestLimit-h.requestCount || steps > stepLimit-h.stepCount || bytes > byteLimit-h.responseBytes {
		h.budgetErr = &ImageWorkflowError{Phase: "validation", Cause: errors.New("image workflow aggregate budget exceeded")}
		return h.budgetErr
	}
	h.requestCount += requests
	h.stepCount += steps
	h.responseBytes += bytes
	return nil
}

type imageBudgetBody struct {
	io.ReadCloser
	host *ImageWorkflowHost
}

func (b *imageBudgetBody) Read(data []byte) (int, error) {
	if len(data) > 32*1024 {
		data = data[:32*1024]
	}
	n, err := b.ReadCloser.Read(data)
	if budgetErr := b.host.charge(0, 0, int64(n)); budgetErr != nil {
		return 0, budgetErr
	}
	return n, err
}

type ImageWorkflowError struct {
	Phase  string
	Status int
	Cause  error
}

func (e *ImageWorkflowError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("image %s request failed (HTTP %d)", e.Phase, e.Status)
	}
	return "image " + e.Phase + " failed: " + e.Cause.Error()
}
func (e *ImageWorkflowError) Unwrap() error { return e.Cause }

func (h *ImageWorkflowHost) Execute(ctx context.Context, id string, values map[string]any) (any, error) {
	if h.ValidateOwner == nil {
		return nil, errors.New("image plugin owner validator is required")
	}
	if err := h.ValidateOwner(); err != nil {
		return nil, err
	}
	values, err := h.credentialValues(values)
	if err != nil {
		return nil, err
	}
	return h.execute(ctx, id, values, 0)
}

func (h *ImageWorkflowHost) execute(ctx context.Context, id string, values map[string]any, depth int) (any, error) {
	if depth > 32 {
		return nil, errors.New("image workflow nesting exceeds limit")
	}
	workflow, ok := h.Manifest.Workflows[id]
	if !ok {
		return nil, errors.New("image workflow is unavailable")
	}
	values = imageCloneContext(values)
	steps := map[string]any{}
	values["steps"] = steps
	for index, step := range workflow.Steps {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := h.charge(0, 1, 0); err != nil {
			return nil, err
		}
		var result any
		var err error
		switch {
		case step.Request != nil:
			result, err = h.request(ctx, *step.Request, values)
		case step.Value != nil:
			result, err = EvaluateImageValue(*step.Value, values)
		case step.Assert != nil:
			result, err = EvaluateImageValue(*step.Assert, values)
			if err == nil && ImageWorkflowValue(result) != true {
				err = errors.New("image response assertion failed")
			}
		case step.Call != "":
			bindings := map[string]any{}
			for name, binding := range step.Bindings {
				bindings[name], err = EvaluateImageValue(binding, values)
				if err != nil {
					break
				}
			}
			if err == nil {
				child := imageCloneContext(values)
				child["args"] = bindings
				result, err = h.execute(ctx, step.Call, child, depth+1)
			}
		default:
			err = errors.New("image workflow contains an unsupported step")
		}
		if err != nil {
			return nil, fmt.Errorf("image workflow depth %d step %d: %w", depth, index+1, err)
		}
		steps[step.ID] = result
	}
	result, err := EvaluateImageValue(workflow.Result, values)
	if err != nil {
		return nil, fmt.Errorf("image workflow depth %d result: %w", depth, err)
	}
	return result, nil
}

func (h *ImageWorkflowHost) allowedURL(target *url.URL) bool {
	if target.Scheme != "https" || target.User != nil || target.Hostname() == "" {
		return false
	}
	for _, origin := range h.Manifest.Origins {
		base, err := url.Parse(origin.URL)
		if err != nil {
			continue
		}
		if target.Port() != base.Port() {
			continue
		}
		if strings.EqualFold(target.Hostname(), base.Hostname()) || origin.Subdomains && strings.HasSuffix(strings.ToLower(target.Hostname()), "."+strings.ToLower(base.Hostname())) {
			return true
		}
	}
	return false
}

type imageBoundaryError struct{ cause error }

func (e *imageBoundaryError) Error() string { return "image plugin execution boundary rejected" }
func (e *imageBoundaryError) Unwrap() error { return e.cause }

type imageOwnerTransport struct {
	base        http.RoundTripper
	host        *ImageWorkflowHost
	credentials map[string]bool
}

func (t imageOwnerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if !t.host.allowedURL(request.URL) {
		return nil, &imageBoundaryError{cause: errors.New("image request origin is not permitted by the plugin")}
	}
	if err := t.host.checkCredentials(request.URL, t.credentials); err != nil {
		return nil, err
	}
	if err := t.host.ValidateOwner(); err != nil {
		return nil, &imageBoundaryError{cause: err}
	}
	if err := t.host.charge(1, 0, 0); err != nil {
		return nil, &imageBoundaryError{cause: err}
	}
	request = request.Clone(request.Context())
	cookies := t.host.scopedCookies()
	cookies.Add(request, t.credentials)
	response, err := t.base.RoundTrip(request)
	if err == nil && response != nil {
		if ownerErr := t.host.ValidateOwner(); ownerErr != nil {
			if response.Body != nil {
				response.Body.Close()
			}
			return nil, &imageBoundaryError{cause: ownerErr}
		}
		cookies.Store(request.URL, response.Cookies())
		if response.Body != nil {
			response.Body = &imageBudgetBody{ReadCloser: response.Body, host: t.host}
		}
	}
	return response, err
}

func (h *ImageWorkflowHost) request(ctx context.Context, declaration manifest.ImageRequest, values map[string]any) (any, error) {
	evaluation := imageEvaluation{remaining: 100000, credentials: map[string]bool{}}
	resolved, err := evaluation.value(declaration.URL, values)
	if err != nil {
		return nil, err
	}
	address, err := imageString(resolved)
	if err != nil {
		return nil, err
	}
	target, err := url.Parse(address)
	if err != nil || !h.allowedURL(target) {
		return nil, errors.New("image request origin is not permitted by the plugin")
	}
	query := target.Query()
	for key, expression := range declaration.Query {
		value, err := evaluation.value(expression, values)
		if err != nil {
			return nil, err
		}
		if imageValueOmitted(value) {
			continue
		}
		text, err := imageString(value)
		if err != nil {
			return nil, err
		}
		query.Set(key, text)
	}
	target.RawQuery = query.Encode()
	var body []byte
	contentType := ""
	if declaration.Body != nil {
		value, err := evaluation.value(*declaration.Body, values)
		if err != nil {
			return nil, err
		}
		body, contentType, err = encodeImageBody(declaration.Encoding, value)
		if err != nil {
			return nil, err
		}
	}
	if int64(len(body)) > h.Manifest.Limits.ResponseBytes {
		return nil, errors.New("image request exceeds declared byte limit")
	}
	client := http.Client{}
	if h.Client != nil {
		client = *h.Client
	}
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	client.Transport = imageOwnerTransport{base: transport, host: h, credentials: evaluation.credentials}
	previousRedirect := client.CheckRedirect
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if !h.allowedURL(request.URL) {
			return &imageBoundaryError{cause: errors.New("image redirect origin is not permitted by the plugin")}
		}
		if len(via) >= 5 {
			return &imageBoundaryError{cause: errors.New("image redirect limit exceeded")}
		}
		if err := h.ValidateOwner(); err != nil {
			return &imageBoundaryError{cause: err}
		}
		if previousRedirect != nil {
			return previousRedirect(request, via)
		}
		return nil
	}
	attempts := 1
	if declaration.Retry != nil {
		attempts = declaration.Retry.Attempts
	}
	for attempt := 0; attempt < attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		requestCtx, cancel := context.WithTimeout(ctx, time.Duration(declaration.TimeoutSeconds)*time.Second)
		request, err := http.NewRequestWithContext(requestCtx, declaration.Method, target.String(), bytes.NewReader(body))
		if err != nil {
			cancel()
			return nil, errors.New("invalid image request")
		}
		if contentType != "" {
			request.Header.Set("Content-Type", contentType)
		}
		for key, expression := range declaration.Headers {
			value, evalErr := evaluation.value(expression, values)
			if evalErr != nil {
				cancel()
				return nil, evalErr
			}
			if imageValueOmitted(value) {
				continue
			}
			text, textErr := imageString(value)
			if textErr != nil {
				cancel()
				return nil, textErr
			}
			request.Header.Set(key, text)
		}
		response, err := client.Do(request)
		if err != nil {
			requestErr := requestCtx.Err()
			cancel()
			var boundary *imageBoundaryError
			if errors.As(err, &boundary) {
				return nil, boundary
			}
			if requestErr != nil {
				return nil, requestErr
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, &ImageWorkflowError{Phase: declaration.Phase, Cause: errors.New("network request failed")}
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 && len(declaration.AcceptedMediaTypes) > 0 {
			media, _, parseErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
			if parseErr != nil || !slices.Contains(declaration.AcceptedMediaTypes, media) {
				response.Body.Close()
				cancel()
				return nil, &ImageWorkflowError{Phase: "validation", Cause: errors.New("response media type is not accepted")}
			}
		}
		data, readErr := io.ReadAll(io.LimitReader(response.Body, declaration.MaxBytes+1))
		closeErr := response.Body.Close()
		requestErr := requestCtx.Err()
		cancel()
		if requestErr != nil {
			return nil, requestErr
		}
		var budgetErr *ImageWorkflowError
		if errors.As(readErr, &budgetErr) {
			return nil, budgetErr
		}
		if readErr != nil || closeErr != nil {
			return nil, &ImageWorkflowError{Phase: "validation", Cause: errors.New("response read failed")}
		}
		if int64(len(data)) > declaration.MaxBytes {
			return nil, &ImageWorkflowError{Phase: "validation", Cause: errors.New("response exceeds declared byte limit")}
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			if declaration.Retry != nil && attempt+1 < attempts && slices.Contains(declaration.Retry.Statuses, response.StatusCode) {
				timer := time.NewTimer(time.Duration(declaration.Retry.DelayMS) * time.Millisecond)
				select {
				case <-ctx.Done():
					timer.Stop()
					return nil, ctx.Err()
				case <-timer.C:
					continue
				}
			}
			return nil, &ImageWorkflowError{Phase: declaration.Phase, Status: response.StatusCode, Cause: errors.New("HTTP request rejected")}
		}
		var decoded any
		switch declaration.Response {
		case "text":
			decoded = string(data)
		case "binary":
			decoded = data
		case "json":
			decoder := json.NewDecoder(bytes.NewReader(data))
			decoder.UseNumber()
			if err := decoder.Decode(&decoded); err != nil {
				return nil, &ImageWorkflowError{Phase: "validation", Cause: errors.New("invalid JSON response")}
			}
			var extra any
			if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
				return nil, &ImageWorkflowError{Phase: "validation", Cause: errors.New("trailing JSON response")}
			}
		case "framed-json":
			decoded, err = decodeImageFrames(data, declaration.FramePrefix)
			if err != nil {
				return nil, &ImageWorkflowError{Phase: "validation", Cause: err}
			}
		default:
			return nil, errors.New("unsupported image response encoding")
		}
		headers := map[string]any{}
		for name, entries := range response.Header {
			if len(entries) > 0 {
				headers[strings.ToLower(name)] = entries[0]
			}
		}
		return evaluation.scoped(map[string]any{"body": decoded, "headers": headers, "status": response.StatusCode}), nil
	}
	return nil, errors.New("image request attempt limit exhausted")
}

func encodeImageBody(encoding string, value any) ([]byte, string, error) {
	switch encoding {
	case "json":
		data, err := json.Marshal(value)
		return data, "application/json", err
	case "binary":
		if data, ok := value.([]byte); ok {
			return data, "application/octet-stream", nil
		}
		text, err := imageString(value)
		return []byte(text), "application/octet-stream", err
	case "form":
		fields, ok := value.(map[string]any)
		if !ok {
			return nil, "", errors.New("image form requires an object")
		}
		form := url.Values{}
		for name, item := range fields {
			text, err := imageString(item)
			if err != nil {
				return nil, "", err
			}
			form.Set(name, text)
		}
		return []byte(form.Encode()), "application/x-www-form-urlencoded", nil
	case "multipart":
		fields, ok := value.([]any)
		if !ok {
			return nil, "", errors.New("image multipart requires a part array")
		}
		var buffer bytes.Buffer
		writer := multipart.NewWriter(&buffer)
		for _, item := range fields {
			part, ok := item.(map[string]any)
			if !ok {
				return nil, "", errors.New("invalid image multipart part")
			}
			name, ok := part["name"].(string)
			if !ok || strings.ContainsAny(name, "\r\n") {
				return nil, "", errors.New("invalid image multipart name")
			}
			filename, _ := part["filename"].(string)
			if filename == "" {
				text, err := imageString(part["value"])
				if err != nil {
					return nil, "", err
				}
				if err := writer.WriteField(name, text); err != nil {
					return nil, "", err
				}
			} else {
				mediaType, ok := part["media_type"].(string)
				if !ok || strings.ContainsAny(filename+mediaType, "\r\n") {
					return nil, "", errors.New("invalid image multipart file")
				}
				data, ok := part["data"].([]byte)
				if !ok {
					return nil, "", errors.New("image multipart file requires binary data")
				}
				header := textproto.MIMEHeader{}
				header.Set("Content-Disposition", fmt.Sprintf("form-data; name=%q; filename=%q", name, filename))
				header.Set("Content-Type", mediaType)
				out, err := writer.CreatePart(header)
				if err != nil {
					return nil, "", err
				}
				if _, err := out.Write(data); err != nil {
					return nil, "", err
				}
			}
			if buffer.Len() > imageValueLimit {
				return nil, "", errors.New("image multipart exceeds byte limit")
			}
		}
		if err := writer.Close(); err != nil {
			return nil, "", err
		}
		return buffer.Bytes(), writer.FormDataContentType(), nil
	default:
		return nil, "", errors.New("unsupported image request encoding")
	}
}

func decodeImageFrames(data []byte, prefix string) ([]any, error) {
	text := string(data)
	if prefix != "" {
		if !strings.HasPrefix(text, prefix) {
			return nil, errors.New("image response framing prefix is missing")
		}
		text = strings.TrimPrefix(text, prefix)
	}
	var frames []any
	for len(strings.TrimSpace(text)) > 0 {
		text = strings.TrimLeft(text, "\r\n ")
		line, rest, ok := strings.Cut(text, "\n")
		if !ok {
			return nil, errors.New("image response frame length is missing")
		}
		length, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil || length < 1 || length > len(rest) {
			return nil, errors.New("image response frame length is invalid")
		}
		var frame any
		decoder := json.NewDecoder(strings.NewReader(rest[:length]))
		decoder.UseNumber()
		if err := decoder.Decode(&frame); err != nil {
			return nil, errors.New("invalid image response frame")
		}
		var extra any
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			return nil, errors.New("trailing image response frame data")
		}
		frames = append(frames, frame)
		if len(frames) > 4096 {
			return nil, errors.New("image response frame count exceeds limit")
		}
		text = rest[length:]
	}
	if len(frames) == 0 {
		return nil, errors.New("image response contains no frames")
	}
	return frames, nil
}
