package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/example-git/crux/internal/subagentscript"
	"github.com/stretchr/testify/require"
)

type subagentCommandService struct {
	mu       sync.Mutex
	requests []subagentscript.Request
}

func (s *subagentCommandService) Call(_ context.Context, request subagentscript.Request) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, request)
	return map[string]string{"id": "a12345678"}, nil
}

func TestSubagentCommandsUseBoundSessionAPI(t *testing.T) {
	service := &subagentCommandService{}
	bridge, err := subagentscript.NewBridge(service, "parent", "")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bridge.Close()) })
	for _, entry := range bridge.Environment() {
		key, value, _ := strings.Cut(entry, "=")
		t.Setenv(key, value)
	}
	for _, test := range []struct {
		args  []string
		input string
	}{
		{args: []string{"start", "--type", "worker", "--prompt-file", "-"}, input: "line one\nline two"},
		{args: []string{"wait", "a12345678", "--timeout", "10ms"}},
		{args: []string{"call"}, input: `{"action":"continue","task_id":"a12345678","prompt":"next"}`},
		{args: []string{"script", "-"}, input: `return subagents.start({prompt:"scripted"});`},
	} {
		command := newSubagentCommand()
		var output bytes.Buffer
		command.SetOut(&output)
		command.SetIn(strings.NewReader(test.input))
		command.SetArgs(test.args)
		require.NoError(t, command.ExecuteContext(t.Context()))
		require.True(t, json.Valid(output.Bytes()))
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	require.Len(t, service.requests, 4)
	require.Equal(t, "line one\nline two", service.requests[0].Prompt)
	require.Equal(t, "worker", service.requests[0].SubagentType)
	require.EqualValues(t, 10, *service.requests[1].TimeoutMS)
	require.Equal(t, "continue", service.requests[2].Action)
	require.Equal(t, "scripted", service.requests[3].Prompt)
}

func TestSubagentCommandRejectsSessionOverride(t *testing.T) {
	command := newSubagentCommand()
	command.SetArgs([]string{"start", "--session", "other", "work"})
	require.ErrorContains(t, command.ExecuteContext(t.Context()), "unknown flag: --session")
}
