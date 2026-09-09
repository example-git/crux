package shell

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExpandValueWithoutCommandsMatchesVariableSemantics(t *testing.T) {
	environment := []string{"KEY=captured value", "EMPTY=", "NUMBER=12"}
	for _, value := range []string{
		"literal key", "$KEY", "${KEY}", "${EMPTY:-fallback}", "${KEY:+alternate}",
		"${MISSING:-${KEY}}", "${MISSING}", `\$KEY`, `"$KEY"`, `'quoted'`,
		"prefix\n$KEY\nsuffix", "$((NUMBER+3))", "${KEY:0:8}", "${KEY/value/result}",
	} {
		t.Run(value, func(t *testing.T) {
			want, err := ExpandValue(t.Context(), value, environment)
			require.NoError(t, err)
			got, err := ExpandValueWithoutCommands(t.Context(), value, environment, false)
			require.NoError(t, err)
			require.Equal(t, want, got)
		})
	}
	t.Setenv("PRIVATE_ENV", "live-must-not-be-read")
	got, err := ExpandValueWithoutCommands(t.Context(), "$PRIVATE_ENV", nil, false)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestExpandValueWithoutCommandsRejectsHiddenCommands(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "must-not-exist")
	command := "$(printf x >> '" + marker + "')"
	for _, value := range []string{command, "${SET:-" + command + "}", "${ABSENT:+" + command + "}",
		"${SET:-${ABSENT:-" + command + "}}", "`printf x >> '" + marker + "'`", "$(< '" + marker + "')"} {
		_, err := ExpandValueWithoutCommands(t.Context(), value, []string{"SET=present"}, false)
		require.ErrorContains(t, err, "command substitution")
		require.NotContains(t, err.Error(), marker)
	}
	_, err := os.Stat(marker)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestExpandValueWithoutCommandsCapturedPolicyAndPrivateErrors(t *testing.T) {
	prior := NoUnset.Load()
	NoUnset.Store(true)
	t.Cleanup(func() { NoUnset.Store(prior) })
	got, err := ExpandValueWithoutCommands(t.Context(), "$MISSING", nil, false)
	require.NoError(t, err)
	require.Empty(t, got)
	NoUnset.Store(false)
	for _, value := range []string{"$MISSING", "${MISSING:?synthetic-private-error}", "${synthetic-private-template"} {
		_, err := ExpandValueWithoutCommands(t.Context(), value, nil, true)
		require.Error(t, err)
		require.NotContains(t, err.Error(), "synthetic-private")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = ExpandValueWithoutCommands(ctx, "literal", nil, false)
	require.ErrorIs(t, err, context.Canceled)
}
