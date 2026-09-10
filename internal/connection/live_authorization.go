package connection

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"sync"
	"time"
)

type principalRequests struct {
	principal string
	grantID   string
	ctx       context.Context
	cancel    context.CancelFunc
	requests  sync.WaitGroup
	revoked   bool
	drained   chan struct{}
	err       error
}

type observedAuthorizationUse struct {
	grantID string
	at      time.Time
}

type liveAuthorization struct {
	authorization *ClientAuthorization
	ctx           context.Context
	cancel        context.CancelFunc
	mu            sync.Mutex
	current       map[string]*principalRequests
	retired       []*principalRequests
	uses          map[string]observedAuthorizationUse
	admit         func(string) error
	revoke        func(context.Context, string) error
	control       *authorizationControl
	closeOnce     sync.Once
	closed        chan struct{}
	closeErr      error
}

// StartLive binds one daemon to the captured store. The callbacks perform real
// backend admission and cancellation/join; a notification-only callback is not
// sufficient to acknowledge revocation.
func (a *ClientAuthorization) StartLive(ctx context.Context, admit func(string) error, revoke func(context.Context, string) error) error {
	if a == nil || admit == nil || revoke == nil {
		return ErrClientAuthorization
	}
	a.liveMu.Lock()
	defer a.liveMu.Unlock()
	if a.live != nil {
		return errors.New("live authorization is already enabled")
	}
	liveCtx, cancel := context.WithCancel(ctx)
	l := &liveAuthorization{authorization: a, ctx: liveCtx, cancel: cancel, admit: admit, revoke: revoke, current: map[string]*principalRequests{}, uses: map[string]observedAuthorizationUse{}, closed: make(chan struct{})}
	control, err := startAuthorizationControl(l)
	if err != nil {
		cancel()
		return err
	}
	l.control = control
	a.live = l
	go l.watch()
	return nil
}

// AdmitRequest owns the lifetime of the whole HTTP handler, including streams.
// Its completion must be called even when writing the response fails.
func (a *ClientAuthorization) AdmitRequest(ctx context.Context, state tls.ConnectionState, abort func()) (context.Context, func(), error) {
	if a == nil {
		return ctx, nil, ErrClientAuthorization
	}
	a.liveMu.Lock()
	l := a.live
	a.liveMu.Unlock()
	if l == nil {
		return ctx, nil, ErrClientAuthorization
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ctx.Err() != nil || a.Authorize(ctx, state) != nil {
		return ctx, nil, ErrClientAuthorization
	}
	data, err := readClientAuthorization(ctx, a.path)
	if err != nil {
		return ctx, nil, ErrClientAuthorization
	}
	grants, err := a.grants(data)
	if err != nil {
		return ctx, nil, err
	}
	l.reconcileLocked(grants)
	principal := certificateFingerprint(state.PeerCertificates[0])
	grantID, ok := grants[principal]
	if !ok {
		return ctx, nil, ErrClientAuthorization
	}
	entry := l.current[principal]
	if entry != nil && entry.revoked {
		select {
		case <-entry.drained:
			if entry.err != nil {
				return ctx, nil, ErrClientAuthorization
			}
			entry = nil
		default:
			return ctx, nil, ErrClientAuthorization
		}
	}
	if entry == nil {
		if err := l.admit(principal); err != nil {
			return ctx, nil, ErrClientAuthorization
		}
		lifetime, cancel := context.WithCancel(l.ctx)
		entry = &principalRequests{principal: principal, grantID: grantID, ctx: lifetime, cancel: cancel, drained: make(chan struct{})}
		l.current[principal] = entry
	}
	entry.requests.Add(1)
	requestCtx, cancel := context.WithCancel(ctx)
	abortDone := make(chan struct{})
	stop := context.AfterFunc(entry.ctx, func() {
		defer close(abortDone)
		cancel()
		if abort != nil {
			abort()
		}
	})
	if entry.ctx.Err() != nil {
		cancel()
	}
	l.uses[principal] = observedAuthorizationUse{grantID: grantID, at: time.Now().UTC()}
	var once sync.Once
	return requestCtx, func() {
		once.Do(func() {
			if !stop() {
				<-abortDone
			}
			cancel()
			entry.requests.Done()
		})
	}, nil
}

func (a *ClientAuthorization) grants(data *store) (map[string]string, error) {
	if _, err := a.roots(data); err != nil {
		return nil, err
	}
	grants := map[string]string{}
	for _, code := range data.AuthorizedClients {
		cert, err := parseCertificate(code, x509.ExtKeyUsageClientAuth)
		if err != nil {
			return nil, ErrClientAuthorization
		}
		principal := certificateFingerprint(cert)
		grants[principal] = data.AuthorizationRecords[principal].GrantID
	}
	return grants, nil
}

func (l *liveAuthorization) reconcileLocked(grants map[string]string) {
	// Unreadable authority cancels active work below but does not erase an
	// observation. Only a successful snapshot establishes grant replacement.
	if grants != nil {
		for principal, use := range l.uses {
			if grant, ok := grants[principal]; !ok || grant != use.grantID {
				delete(l.uses, principal)
			}
		}
	}
	for principal, entry := range l.current {
		grant, ok := grants[principal]
		if !ok || grant != entry.grantID {
			l.revokeLocked(entry)
		}
	}
	l.sweepDrainedLocked()
}

// A successfully joined old lifetime no longer needs heap retention. A receipt
// can still verify that no matching live/retired work remains. Errors and work
// still draining remain retained and cannot be reported as successful cleanup.
func (l *liveAuthorization) sweepDrainedLocked() {
	kept := l.retired[:0]
	for _, entry := range l.retired {
		finished := false
		select {
		case <-entry.drained:
			finished = entry.err == nil
		default:
		}
		if !finished {
			kept = append(kept, entry)
			continue
		}
		if l.current[entry.principal] == entry {
			delete(l.current, entry.principal)
		}
	}
	clear(l.retired[len(kept):])
	l.retired = kept
}

func (l *liveAuthorization) revokeLocked(entry *principalRequests) {
	if entry.revoked {
		return
	}
	entry.revoked = true
	entry.cancel()
	l.retired = append(l.retired, entry)
	go func() {
		// Cleanup is retained independently of one administrative HTTP wait.
		entry.err = l.revoke(context.Background(), entry.principal)
		entry.requests.Wait()
		close(entry.drained)
	}()
}

func (l *liveAuthorization) reconcileReceipt(ctx context.Context, receipt RevocationRecord) error {
	l.mu.Lock()
	data, err := readClientAuthorization(ctx, l.authorization.path)
	if err == nil {
		stored, ok := data.Revocations[receipt.OperationID]
		if !ok || stored != receipt {
			err = ErrClientAuthorization
		}
	}
	var grants map[string]string
	if err == nil {
		grants, err = l.authorization.grants(data)
	}
	if err != nil {
		l.mu.Unlock()
		return err
	}
	l.reconcileLocked(grants)
	var waiting []*principalRequests
	for _, entry := range l.retired {
		if entry.principal == receipt.Principal && entry.grantID == receipt.GrantID {
			waiting = append(waiting, entry)
		}
	}
	l.mu.Unlock()
	for _, entry := range waiting {
		select {
		case <-entry.drained:
			if entry.err != nil {
				return entry.err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (l *liveAuthorization) watch() {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-l.ctx.Done():
			l.beginClose()
			return
		case <-ticker.C:
			l.mu.Lock()
			var grants map[string]string
			if data, err := readClientAuthorization(l.ctx, l.authorization.path); err == nil {
				grants, _ = l.authorization.grants(data) // Nil grants on any failure.
			}
			l.reconcileLocked(grants) // Unreadable or replaced authority fails closed.
			l.mu.Unlock()
		}
	}
}

func (l *liveAuthorization) beginClose() {
	l.closeOnce.Do(func() {
		l.cancel()
		l.mu.Lock()
		l.reconcileLocked(nil)
		retired := append([]*principalRequests(nil), l.retired...)
		l.mu.Unlock()
		go func() {
			_ = l.control.server.Close()
			for _, entry := range retired {
				<-entry.drained
				l.closeErr = errors.Join(l.closeErr, entry.err)
			}
			if l.closeErr == nil {
				l.control.remove()
			}
			close(l.closed)
		}()
	})
}

func (a *ClientAuthorization) CloseLive(ctx context.Context) error {
	if a == nil {
		return nil
	}
	a.liveMu.Lock()
	l := a.live
	a.liveMu.Unlock()
	if l == nil {
		return nil
	}
	l.beginClose()
	select {
	case <-l.closed:
		return l.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}
