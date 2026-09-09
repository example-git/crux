package mcp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	mcpoauth "github.com/example-git/crux/internal/oauth/mcp"
	"github.com/example-git/crux/internal/pubsub"
)

var ErrClosed = errors.New("workspace MCP runtime is closed")

// Manager owns one workspace's connections, authentication, caches and events.
// It is installed once on ConfigStore and remains closed after workspace teardown.
type Manager struct {
	store         *config.ConfigStore
	ctx           context.Context
	cancel        context.CancelFunc
	lifecycle     sync.Mutex
	closed        bool
	operations    sync.WaitGroup
	closeOnce     sync.Once
	closeDone     chan struct{}
	sessions      *csync.Map[string, *ClientSession]
	states        *csync.Map[string, ClientInfo]
	authURLs      *csync.Map[string, *mcpoauth.Handler]
	broker        *pubsub.Broker[Event]
	initOnce      sync.Once
	initDone      chan struct{}
	initMu        sync.Mutex
	initStarted   bool
	initErr       error
	renewMusMu    sync.Mutex
	renewMus      map[string]*sync.Mutex
	gens          *csync.Map[string, uint64]
	suppressMus   *csync.Map[string, *sync.Mutex]
	newSession    func(context.Context, *config.ConfigStore, string, config.MCPConfig, config.VariableResolver, bool) (*ClientSession, error)
	reinitMu      sync.Mutex
	reinitRunning bool
	reinitDirty   bool
	allTools      *csync.Map[string, []*Tool]
	allPrompts    *csync.Map[string, []*Prompt]
	allResources  *csync.Map[string, []*Resource]
}

// For returns only the supplied store's MCP runtime. There is no global fallback.
func For(cfg *config.ConfigStore) *Manager {
	if cfg == nil {
		panic("MCP runtime requires a ConfigStore")
	}
	return cfg.MCPRuntime(func() config.MCPRuntime { r := newManager(); r.store = cfg; return r }).(*Manager)
}

func newManager() *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	r := &Manager{ctx: ctx, cancel: cancel, closeDone: make(chan struct{}), initDone: make(chan struct{}),
		sessions: csync.NewMap[string, *ClientSession](), states: csync.NewMap[string, ClientInfo](),
		authURLs: csync.NewMap[string, *mcpoauth.Handler](), broker: pubsub.NewBroker[Event](),
		renewMus: map[string]*sync.Mutex{}, gens: csync.NewMap[string, uint64](), suppressMus: csync.NewMap[string, *sync.Mutex](),
		allTools: csync.NewMap[string, []*Tool](), allPrompts: csync.NewMap[string, []*Prompt](), allResources: csync.NewMap[string, []*Resource]()}
	r.newSession = r.createSession
	return r
}

func (*Manager) Format(s fmt.State, verb rune) { fmt.Fprint(s, "[private workspace MCP runtime]") }
func (*Manager) MarshalJSON() ([]byte, error)  { return nil, errors.New("MCP runtime is private") }

// admit binds work to the workspace and pairs Add with shutdown under one lock.
func (r *Manager) admit(ctx context.Context) (context.Context, func(), error) {
	r.lifecycle.Lock()
	if r.closed {
		r.lifecycle.Unlock()
		return nil, nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		r.lifecycle.Unlock()
		return nil, nil, err
	}
	r.operations.Add(1)
	r.lifecycle.Unlock()
	work, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(r.ctx, cancel)
	if r.ctx.Err() != nil {
		cancel()
	}
	return work, func() { stop(); cancel(); r.operations.Done() }, nil
}

// serverOperation serializes changes to one server while allowing other servers
// and workspaces to connect independently. Waiting is canceled by shutdown.
func (r *Manager) serverOperation(ctx context.Context, name string) (context.Context, func(), error) {
	work, done, err := r.admit(ctx)
	if err != nil {
		return nil, nil, err
	}
	mu := r.renewLock(name)
	for !mu.TryLock() {
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-work.Done():
			timer.Stop()
			done()
			return nil, nil, work.Err()
		case <-timer.C:
		}
	}
	if err := work.Err(); err != nil {
		mu.Unlock()
		done()
		return nil, nil, err
	}
	return work, func() { mu.Unlock(); done() }, nil
}

// Close cancels this workspace only. Cleanup continues after a caller's wait
// deadline; repeated calls wait on the same cleanup and can never reopen it.
func (r *Manager) Close(ctx context.Context) error {
	r.closeOnce.Do(func() {
		r.lifecycle.Lock()
		r.closed = true
		r.cancel()
		r.lifecycle.Unlock()
		r.finishInit(ErrClosed)
		go func() {
			r.operations.Wait()
			var wg sync.WaitGroup
			for name, s := range r.sessions.Seq2() {
				r.sessions.Del(name)
				wg.Go(func() { closeSession(name, s) })
			}
			for name, h := range r.authURLs.Seq2() {
				r.authURLs.Del(name)
				wg.Go(h.Close)
			}
			wg.Wait()
			for name := range r.states.Seq2() {
				r.states.Del(name)
			}
			for name := range r.allTools.Seq2() {
				r.allTools.Del(name)
			}
			for name := range r.allPrompts.Seq2() {
				r.allPrompts.Del(name)
			}
			for name := range r.allResources.Seq2() {
				r.allResources.Del(name)
			}
			r.broker.Shutdown()
			close(r.closeDone)
		}()
	})
	select {
	case <-r.closeDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// requireStore rejects accidental mixing of one workspace's connection owner
// with another workspace's configuration. Unbound managers exist only in tests.
func (r *Manager) requireStore(cfg *config.ConfigStore) error {
	if r.store != nil && r.store != cfg {
		return errors.New("MCP runtime belongs to a different workspace")
	}
	return nil
}
