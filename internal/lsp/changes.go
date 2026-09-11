package lsp

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"time"
)

const changeDebounce = 300 * time.Millisecond

type changeQueue struct {
	mu         sync.Mutex
	pending    map[string]struct{}
	timer      *time.Timer
	running    bool
	stopped    bool
	cancel     context.CancelFunc
	done       chan struct{}
	generation uint64
}

func (q *changeQueue) enqueue(path string, notify func(context.Context, string)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.stopped {
		return
	}
	if q.pending == nil {
		q.pending = make(map[string]struct{})
	}
	q.pending[path] = struct{}{}
	if !q.running {
		if q.timer != nil {
			q.timer.Stop()
		}
		q.generation++
		generation := q.generation
		q.timer = time.AfterFunc(changeDebounce, func() { q.flush(generation, notify) })
	}
}

func (q *changeQueue) flush(generation uint64, notify func(context.Context, string)) {
	q.mu.Lock()
	if q.stopped || q.running || generation != q.generation || len(q.pending) == 0 {
		q.mu.Unlock()
		return
	}
	q.running = true
	paths := q.pending
	q.pending = nil
	ctx, cancel := context.WithCancel(context.Background())
	q.cancel = cancel
	done := make(chan struct{})
	q.done = done
	q.mu.Unlock()
	defer cancel()
	for path := range paths {
		if ctx.Err() != nil {
			break
		}
		bounded, release := context.WithTimeout(ctx, 5*time.Second)
		notify(bounded, path)
		release()
	}
	q.mu.Lock()
	q.running = false
	q.cancel = nil
	q.done = nil
	close(done)
	if !q.stopped && len(q.pending) > 0 {
		q.generation++
		generation := q.generation
		q.timer = time.AfterFunc(changeDebounce, func() { q.flush(generation, notify) })
	}
	q.mu.Unlock()
}

func (q *changeQueue) stop() {
	q.mu.Lock()
	q.stopped = true
	q.pending = nil
	if q.timer != nil {
		q.timer.Stop()
	}
	if q.cancel != nil {
		q.cancel()
	}
	done := q.done
	q.mu.Unlock()
	if done != nil {
		<-done
	}
}

func (q *changeQueue) resume() {
	q.mu.Lock()
	q.stopped = false
	q.mu.Unlock()
}

func (s *Manager) QueueChange(path string) {
	if s == nil || path == "" {
		return
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return
	}
	for client := range s.clients.Seq() {
		if client.HandlesFile(absolute) {
			s.changes.enqueue(absolute, s.notifyChangedFile)
			return
		}
	}
}

func (s *Manager) notifyChangedFile(ctx context.Context, path string) {
	for name, client := range s.clients.Seq2() {
		if !client.HandlesFile(path) {
			continue
		}
		var err error
		if client.IsFileOpen(path) {
			err = client.NotifyChange(ctx, path)
		} else {
			err = client.OpenFileOnDemand(ctx, path)
		}
		if err != nil && ctx.Err() == nil {
			slog.WarnContext(ctx, "Failed to notify language server of file change", "server", name, "error", err)
		}
	}
}
