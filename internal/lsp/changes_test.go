package lsp

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestChangeQueueCoalescesAndRejectsStaleTimers(t *testing.T) {
	queue := changeQueue{running: true}
	defer queue.stop()
	var calls atomic.Int32
	notify := func(context.Context, string) { calls.Add(1) }
	queue.enqueue("same.go", notify)
	queue.enqueue("same.go", notify)
	queue.enqueue("other.go", notify)
	queue.running = false
	queue.generation = 2
	queue.flush(1, notify)
	require.Zero(t, calls.Load())
	queue.flush(2, notify)
	require.EqualValues(t, 2, calls.Load())
	queue.stop()
	queue.enqueue("stopped.go", notify)
	require.Empty(t, queue.pending)
}

func TestChangeQueueStopCancelsAndJoins(t *testing.T) {
	var queue changeQueue
	defer queue.stop()
	entered := make(chan struct{})
	exited := make(chan struct{})
	notify := func(ctx context.Context, _ string) {
		close(entered)
		<-ctx.Done()
		close(exited)
	}
	queue.enqueue("file.go", notify)
	generation := queue.generation
	go queue.flush(generation, notify)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("notification did not start")
	}
	queue.stop()
	select {
	case <-exited:
	default:
		t.Fatal("stop did not join notification")
	}
}
