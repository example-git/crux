package providertransport

import (
	"context"
	"time"
)

type OperationDiagnostic struct {
	ID         string
	Kind       string
	StatusCode int
	Duration   time.Duration
	Failed     bool
}

type operationDiagnosticContextKey struct{}

func ContextWithOperationDiagnostics(ctx context.Context, record func(OperationDiagnostic)) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, operationDiagnosticContextKey{}, record)
}

func RecordOperationDiagnostic(ctx context.Context, diagnostic OperationDiagnostic) {
	if ctx == nil {
		return
	}
	if record, ok := ctx.Value(operationDiagnosticContextKey{}).(func(OperationDiagnostic)); ok && record != nil {
		record(diagnostic)
	}
}
