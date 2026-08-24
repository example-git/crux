// Package sse provides bounded standards-compliant Server-Sent Events framing
// for provider-neutral public protocol transports.
package sse

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const DefaultMaxEventBytes = int64(1 << 20)

var ErrMissingTerminal = errors.New("SSE stream ended without a terminal event")

type Options struct {
	MaxEventBytes   int64
	DoneMarker      string
	RequireTerminal bool
	IsTerminal      func(json.RawMessage) bool
}

// Parse parses standard text/event-stream framing. It deliberately does not
// recover concatenated or malformed data fields; private dialect recovery is
// consumer policy rather than a public protocol primitive.
func Parse(ctx context.Context, reader io.Reader, options Options, yield func(json.RawMessage) error) error {
	if reader == nil {
		return fmt.Errorf("SSE reader is nil")
	}
	if yield == nil {
		return fmt.Errorf("SSE yield callback is nil")
	}
	limit := options.MaxEventBytes
	if limit <= 0 {
		limit = DefaultMaxEventBytes
	}
	doneMarker := options.DoneMarker
	if doneMarker == "" {
		doneMarker = "[DONE]"
	}

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), int(limit)+1)
	var data bytes.Buffer
	terminal := false
	dispatch := func() error {
		if data.Len() == 0 {
			return nil
		}
		payload := bytes.TrimSuffix(data.Bytes(), []byte("\n"))
		data.Reset()
		if string(payload) == doneMarker {
			terminal = true
			return nil
		}
		if int64(len(payload)) > limit {
			return fmt.Errorf("SSE event exceeds %d bytes", limit)
		}
		if !json.Valid(payload) {
			return fmt.Errorf("SSE data is not valid JSON")
		}
		raw := json.RawMessage(bytes.Clone(payload))
		if err := yield(raw); err != nil {
			return err
		}
		if options.IsTerminal != nil && options.IsTerminal(raw) {
			terminal = true
		}
		return nil
	}

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := scanner.Text()
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			field, value = line, ""
		} else if strings.HasPrefix(value, " ") {
			value = value[1:]
		}
		if field != "data" {
			continue
		}
		if int64(data.Len()+len(value)+1) > limit {
			return fmt.Errorf("SSE event exceeds %d bytes", limit)
		}
		data.WriteString(value)
		data.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read SSE stream: %w", err)
	}
	if err := dispatch(); err != nil {
		return err
	}
	if options.RequireTerminal && !terminal {
		return ErrMissingTerminal
	}
	return nil
}
