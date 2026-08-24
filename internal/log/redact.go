package log

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/example-git/crux/internal/redact"
)

type redactingHandler struct {
	handler slog.Handler
}

func (h redactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.handler.Enabled(ctx, level)
}

func (h redactingHandler) Handle(ctx context.Context, record slog.Record) error {
	clean := slog.NewRecord(record.Time, record.Level, redact.String(record.Message), record.PC)
	record.Attrs(func(attr slog.Attr) bool {
		clean.AddAttrs(redactAttr(attr))
		return true
	})
	return h.handler.Handle(ctx, clean)
}

func (h redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, len(attrs))
	for i, attr := range attrs {
		clean[i] = redactAttr(attr)
	}
	return redactingHandler{handler: h.handler.WithAttrs(clean)}
}

func (h redactingHandler) WithGroup(name string) slog.Handler {
	return redactingHandler{handler: h.handler.WithGroup(name)}
}

func redactAttr(attr slog.Attr) slog.Attr {
	value := attr.Value.Resolve()
	switch value.Kind() {
	case slog.KindString:
		attr.Value = slog.StringValue(redact.String(value.String()))
	case slog.KindAny:
		if err, ok := value.Any().(error); ok {
			attr.Value = slog.StringValue(redact.String(err.Error()))
		} else if stringer, ok := value.Any().(fmt.Stringer); ok {
			attr.Value = slog.StringValue(redact.String(stringer.String()))
		}
	case slog.KindGroup:
		attrs := value.Group()
		clean := make([]slog.Attr, len(attrs))
		for i, child := range attrs {
			clean[i] = redactAttr(child)
		}
		attr.Value = slog.GroupValue(clean...)
	}
	return attr
}
