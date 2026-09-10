package task

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestForegroundWaitSessionIsolation(t *testing.T) {
	var waits ForegroundWaits
	first := waits.Register("first")
	second := waits.Register("second")
	removed := waits.Register("first")
	waits.Remove(removed)
	require.Equal(t, 1, waits.Count("first"))
	require.Equal(t, 1, waits.Detach("first"))
	require.Zero(t, waits.Detach("first"))
	select {
	case <-first.Detached:
	default:
		t.Fatal("first wait was not detached")
	}
	select {
	case <-second.Detached:
		t.Fatal("other session detached")
	default:
	}
	select {
	case <-removed.Detached:
		t.Fatal("removed wait detached")
	default:
	}
	waits.Remove(first)
	waits.Remove(second)
	require.Zero(t, waits.Count("second"))
}
