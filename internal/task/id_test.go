package task

import (
	"context"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOwnershipContext(t *testing.T) {
	t.Parallel()

	require.Equal(t, Ownership{}, OwnershipFromContext(context.Background()))
	ownership := Ownership{
		WorkspaceID:      "workspace",
		ParentSessionID:  "parent",
		OwnerAgentTaskID: "a12345678",
		OriginToolCallID: "call",
	}
	require.Equal(t, ownership, OwnershipFromContext(WithOwnership(context.Background(), ownership)))
}

func TestNewID(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		taskType Type
		pattern  string
	}{
		{taskType: TypeShell, pattern: `^b[0-9a-z]{8}$`},
		{taskType: TypeAgent, pattern: `^a[0-9a-z]{8}$`},
	} {
		id, err := NewID(test.taskType)
		require.NoError(t, err)
		require.Regexp(t, regexp.MustCompile(test.pattern), id)
		parsed, err := ParseID(id)
		require.NoError(t, err)
		require.Equal(t, test.taskType, parsed)
	}
}

func TestNewIDRejectsUnknownType(t *testing.T) {
	t.Parallel()

	_, err := NewID(Type("unknown"))
	require.EqualError(t, err, `invalid task type "unknown"`)
}

func TestParseIDRejectsInvalidValues(t *testing.T) {
	t.Parallel()

	for _, id := range []string{"", "001", "x12345678", "b1234567", "b123456789", "b1234567A", "a1234567-"} {
		_, err := ParseID(id)
		require.Error(t, err, id)
	}
}
