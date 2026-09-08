package list

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type joinedAlignmentItem struct {
	*trackedItem
	joined bool
}

func (i *joinedAlignmentItem) JoinPrevious() bool {
	return i.joined
}

func TestJoinedItemSpacingGeometry(t *testing.T) {
	for _, joined := range []bool{false, true} {
		first := newTrackedItem("tool", "body\ntab", true)
		thinking := &joinedAlignmentItem{trackedItem: newTrackedItem("thinking", "branch", true), joined: joined}
		last := newTrackedItem("answer", "answer", true)
		l := NewList(first, thinking, last)
		l.SetSize(40, 10)
		l.SetGap(1)
		gap := 1
		if joined {
			gap = 0
		}
		require.Equal(t, gap, l.GapAfter(0))
		require.Equal(t, 1, l.GapAfter(1))
		require.Equal(t, 5+gap, l.TotalHeight())
		lines := strings.Split(l.Render(), "\n")
		require.Contains(t, lines[2+gap], "branch")
		index, row := l.findItemAtY(0, 2+gap)
		require.Equal(t, 1, index)
		require.Zero(t, row)
		if !joined {
			index, _ = l.findItemAtY(0, 2)
			require.Equal(t, -1, index)
		}
		l.SetSize(40, 2)
		l.ScrollBy(2 + gap)
		require.Equal(t, 2+gap, l.Offset())
		require.Contains(t, strings.Split(l.Render(), "\n")[0], "branch")
		index, row = l.findItemAtY(0, 0)
		require.Equal(t, 1, index)
		require.Zero(t, row)
		l.ScrollBy(-(2 + gap))
		require.Zero(t, l.Offset())
		l.SetGap(2)
		wantHeight := 8
		if joined {
			wantHeight = 6
		}
		require.Equal(t, wantHeight, l.TotalHeight())
	}
}
