package task

import "sync"

type ForegroundWait struct {
	SessionID string
	Detached  chan struct{}
}

type ForegroundWaits struct {
	mu    sync.Mutex
	waits map[*ForegroundWait]struct{}
}

func (w *ForegroundWaits) Register(sessionID string) *ForegroundWait {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.waits == nil {
		w.waits = make(map[*ForegroundWait]struct{})
	}
	wait := &ForegroundWait{SessionID: sessionID, Detached: make(chan struct{})}
	w.waits[wait] = struct{}{}
	return wait
}

func (w *ForegroundWaits) Remove(wait *ForegroundWait) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.waits, wait)
	select {
	case <-wait.Detached:
		return true
	default:
		return false
	}
}

func (w *ForegroundWaits) Count(sessionID string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	count := 0
	for wait := range w.waits {
		if wait.SessionID == sessionID {
			count++
		}
	}
	return count
}

func (w *ForegroundWaits) Detach(sessionID string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	count := 0
	for wait := range w.waits {
		if wait.SessionID == sessionID {
			delete(w.waits, wait)
			close(wait.Detached)
			count++
		}
	}
	return count
}
