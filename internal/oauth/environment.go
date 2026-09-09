package oauth

import (
	"context"
	"fmt"
	"os"
	"strings"
)

type environmentContextKey struct{}
type capturedEnvironment struct{ values map[string]string }

func (*capturedEnvironment) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[private OAuth environment]"))
}

// ContextWithEnvironment binds OAuth client registration settings to an
// immutable owner-side environment. An absent value in a bound environment
// remains absent; it cannot fall back to the executing process.
func ContextWithEnvironment(ctx context.Context, entries []string) context.Context {
	values := &capturedEnvironment{values: map[string]string{}}
	for _, entry := range entries {
		if key, value, ok := strings.Cut(entry, "="); ok {
			values.values[key] = value
		}
	}
	return context.WithValue(ctx, environmentContextKey{}, values)
}

func LookupEnvironment(ctx context.Context, name string) (string, bool) {
	if values, bound := ctx.Value(environmentContextKey{}).(*capturedEnvironment); bound {
		value, ok := values.values[name]
		return value, ok
	}
	return os.LookupEnv(name)
}
