package compatibility

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

type adapterFunc func(context.Context, Invocation) (Request, error)

func (f adapterFunc) Translate(ctx context.Context, invocation Invocation) (Request, error) {
	return f(ctx, invocation)
}

type runtimeFunc func(context.Context, Invocation, Request) error

func (f runtimeFunc) Execute(ctx context.Context, invocation Invocation, request Request) error {
	return f(ctx, invocation, request)
}

func TestRegistryDispatchesByExecutableBasename(t *testing.T) {
	t.Setenv(BypassEnvironment, "")
	registry := NewRegistry()
	var translated Invocation
	var executed Request
	err := registry.Register(Registration{
		Name: "codex",
		Adapter: adapterFunc(func(_ context.Context, invocation Invocation) (Request, error) {
			translated = invocation
			return Request{Style: ExecutionHeadless}, nil
		}),
		Runtime: runtimeFunc(func(_ context.Context, _ Invocation, request Request) error {
			require.Equal(t, "1", os.Getenv(BypassEnvironment))
			executed = request
			return nil
		}),
	})
	require.NoError(t, err)

	exitCode, handled := registry.Dispatch(t.Context(), Invocation{
		Executable: "/private/compat/bin/codex",
		Name:       "claude",
		Args:       []string{"exec", "hello"},
		WorkingDir: "/workspace",
		Env:        []string{"EXAMPLE=value"},
	})

	require.True(t, handled)
	require.Zero(t, exitCode)
	require.Equal(t, "codex", translated.Name)
	require.Equal(t, []string{"exec", "hello"}, translated.Args)
	require.Equal(t, "/workspace", translated.WorkingDir)
	require.Equal(t, "1", environmentValue(translated.Env, BypassEnvironment))
	require.Equal(t, "codex", executed.Source)
	require.Equal(t, "/workspace", executed.WorkingDir)
	require.Equal(t, ExecutionHeadless, executed.Style)
	require.Empty(t, os.Getenv(BypassEnvironment))
}

func TestRegistryLeavesUnregisteredInvocationUntouched(t *testing.T) {
	registry := NewRegistry()
	exitCode, handled := registry.Dispatch(t.Context(), Invocation{
		Executable: "/usr/local/bin/crux",
	})
	require.False(t, handled)
	require.Zero(t, exitCode)
}

func TestPublicRegistryHasNoBuiltInCompatibilityAliases(t *testing.T) {
	for _, name := range []string{"codex", "claude", "agy", "copilot"} {
		exitCode, handled := Dispatch(t.Context(), Invocation{Executable: "/private/compat/bin/" + name})
		require.False(t, handled, name)
		require.Zero(t, exitCode, name)
	}
}

func TestRegistryBypassPreservesNativeChildInvocation(t *testing.T) {
	registry := NewRegistry()
	called := false
	require.NoError(t, registry.Register(Registration{
		Name: "codex",
		Adapter: adapterFunc(func(_ context.Context, _ Invocation) (Request, error) {
			called = true
			return Request{}, nil
		}),
		Runtime: runtimeFunc(func(_ context.Context, _ Invocation, _ Request) error {
			called = true
			return nil
		}),
	}))

	exitCode, handled := registry.Dispatch(t.Context(), Invocation{
		Executable: "/private/compat/bin/codex",
		Env:        []string{BypassEnvironment + "=1"},
	})

	require.False(t, handled)
	require.Zero(t, exitCode)
	require.False(t, called)
}

func TestRegistryWritesTargetShapedExitError(t *testing.T) {
	registry := NewRegistry()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	require.NoError(t, registry.Register(Registration{
		Name: "agy",
		Adapter: adapterFunc(func(_ context.Context, _ Invocation) (Request, error) {
			return Request{}, &ExitError{Code: 2, Stdout: "output\n", Stderr: "invalid flag\n"}
		}),
		Runtime: runtimeFunc(func(_ context.Context, _ Invocation, _ Request) error {
			return errors.New("runtime should not execute")
		}),
	}))

	exitCode, handled := registry.Dispatch(t.Context(), Invocation{
		Executable: "/private/compat/bin/agy",
		WorkingDir: "/workspace",
		Stdout:     &stdout,
		Stderr:     &stderr,
	})

	require.True(t, handled)
	require.Equal(t, 2, exitCode)
	require.Equal(t, "output\n", stdout.String())
	require.Equal(t, "invalid flag\n", stderr.String())
}

func TestRegistryPreservesSuccessfulTargetExit(t *testing.T) {
	registry := NewRegistry()
	var stdout bytes.Buffer
	require.NoError(t, registry.Register(Registration{
		Name: "codex",
		Adapter: adapterFunc(func(_ context.Context, _ Invocation) (Request, error) {
			return Request{}, &ExitError{Code: 0, Stdout: "help\n"}
		}),
		Runtime: runtimeFunc(func(_ context.Context, _ Invocation, _ Request) error {
			return errors.New("runtime should not execute")
		}),
	}))
	exitCode, handled := registry.Dispatch(t.Context(), Invocation{
		Executable: "/private/compat/bin/codex",
		WorkingDir: "/workspace",
		Stdout:     &stdout,
	})
	require.True(t, handled)
	require.Zero(t, exitCode)
	require.Equal(t, "help\n", stdout.String())
}

func TestRegistryRejectsInvalidAndDuplicateRegistrations(t *testing.T) {
	registry := NewRegistry()
	adapter := adapterFunc(func(_ context.Context, _ Invocation) (Request, error) {
		return Request{}, nil
	})
	runtime := runtimeFunc(func(_ context.Context, _ Invocation, _ Request) error {
		return nil
	})

	require.EqualError(t, registry.Register(Registration{}), "compatibility registration name is required")
	require.EqualError(t, registry.Register(Registration{Name: "codex", Runtime: runtime}), "compatibility adapter for \"codex\" is required")
	require.EqualError(t, registry.Register(Registration{Name: "codex", Adapter: adapter}), "compatibility runtime for \"codex\" is required")
	require.NoError(t, registry.Register(Registration{Name: "codex", Adapter: adapter, Runtime: runtime}))
	require.EqualError(t, registry.Register(Registration{Name: "codex", Adapter: adapter, Runtime: runtime}), "compatibility adapter \"codex\" is already registered")
}
