package foundation

import (
	"context"
	"maps"
)

type runtimeControlsContextKey struct{}

func WithRuntimeControls(ctx context.Context, controls map[string]any) context.Context {
	if len(controls) == 0 {
		return ctx
	}
	return context.WithValue(ctx, runtimeControlsContextKey{}, maps.Clone(controls))
}

func RuntimeControlsFromContext(ctx context.Context) map[string]any {
	controls, _ := ctx.Value(runtimeControlsContextKey{}).(map[string]any)
	return maps.Clone(controls)
}
