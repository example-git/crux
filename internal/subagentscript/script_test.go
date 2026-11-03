package subagentscript

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type callerFunc func(context.Context, Request) (any, error)

func (f callerFunc) Call(ctx context.Context, request Request) (any, error) {
	return f(ctx, request)
}

func TestScriptStartsAllTasksBeforeWaiting(t *testing.T) {
	var actions []string
	service := callerFunc(func(_ context.Context, request Request) (any, error) {
		actions = append(actions, request.Action)
		if request.Action == "start" {
			return map[string]any{"id": request.Prompt}, nil
		}
		return map[string]any{"output": request.TaskID}, nil
	})
	result, err := Run(t.Context(), service, "parent", "", `
const tasks = ["one", "two"].map(prompt => subagents.start({prompt}));
print(env.CRUX_SESSION_ID);
return tasks.map(task => subagents.wait(task.id));
`)
	require.NoError(t, err)
	require.Equal(t, []string{"start", "start", "wait", "wait"}, actions)
	require.Len(t, result.Tasks, 2)
	require.Equal(t, []any{"parent"}, result.Output)
	require.Len(t, result.Result, 2)
}

func TestScriptRetainsStartedTasksOnFailure(t *testing.T) {
	service := callerFunc(func(context.Context, Request) (any, error) {
		return map[string]any{"id": "a12345678"}, nil
	})
	result, err := Run(t.Context(), service, "parent", "", `subagents.start({prompt:"work"}); throw Error("failed orchestration");`)
	require.ErrorContains(t, err, "failed orchestration")
	require.Len(t, result.Tasks, 1)
}

func TestScriptRejectsSessionOverrideBeforeLaunch(t *testing.T) {
	called := false
	service := callerFunc(func(context.Context, Request) (any, error) {
		called = true
		return nil, nil
	})
	_, err := Run(t.Context(), service, "parent", "", `subagents.start({prompt:"work", parent_session_id:"other"});`)
	require.ErrorContains(t, err, `unknown field "parent_session_id"`)
	require.False(t, called)
}

func TestScriptCancellationInterruptsJavaScript(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	_, err := Run(ctx, callerFunc(func(context.Context, Request) (any, error) { return nil, nil }), "parent", "", `while (true) {}`)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestBridgeRejectsChangedSessionAndToken(t *testing.T) {
	service := callerFunc(func(_ context.Context, request Request) (any, error) {
		return []string{request.Action}, nil
	})
	bridge, err := NewBridge(service, "parent", "")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bridge.Close()) })
	environment := map[string]string{}
	for _, entry := range bridge.Environment() {
		key, value, _ := strings.Cut(entry, "=")
		environment[key] = value
	}
	for _, field := range []string{"", SessionEnv, TokenEnv} {
		t.Run(field, func(t *testing.T) {
			client, err := ClientFromEnvironment(func(key string) string {
				if key == field {
					return "different"
				}
				return environment[key]
			})
			require.NoError(t, err)
			defer client.Close()
			_, err = client.Call(t.Context(), Request{Action: "list"})
			if field == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "invalid session binding")
			}
		})
	}
}

func TestEnvironmentReplacesInheritedSessionBinding(t *testing.T) {
	ctx := WithEnvironment(t.Context(), []string{SessionEnv + "=child", ParentEnv + "=parent", URLEnv + "=new-endpoint", TokenEnv + "=new-token"})
	base := []string{"PATH=/bin", SessionEnv + "=other", TokenEnv + "=old-token", URLEnv + "=old-endpoint"}
	result := Environment(ctx, base)
	require.Equal(t, []string{"PATH=/bin", SessionEnv + "=child", ParentEnv + "=parent", URLEnv + "=new-endpoint", TokenEnv + "=new-token"}, result)
	require.Equal(t, []string{"PATH=/bin"}, Environment(t.Context(), base))
}

func TestRequestRejectsInvalidExplicitValues(t *testing.T) {
	negative := int64(-1)
	for _, request := range []Request{
		{Action: "unknown"},
		{Action: "start"},
		{Action: "output"},
		{Action: "wait", TaskID: "a12345678", TimeoutMS: &negative},
		{Action: "list", TaskID: "a12345678"},
		{Action: "stop", TaskID: "a12345678", Prompt: "ignored"},
	} {
		require.Error(t, request.Validate())
	}
	_, err := ClientFromEnvironment(func(string) string { return "" })
	require.ErrorContains(t, err, "session environment")
	var request Request
	require.Error(t, Decode([]byte(`{"action":"list","parent_session_id":"other"}`), &request))
	require.Error(t, Decode([]byte(`{"action":"list"} {}`), &request))
	service := callerFunc(func(context.Context, Request) (any, error) { return nil, errors.New("service unavailable") })
	_, err = Run(t.Context(), service, "parent", "", `return subagents.list();`)
	require.ErrorContains(t, err, "service unavailable")
}
