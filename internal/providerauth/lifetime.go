package providerauth

import "context"

func (s *Service) operationContext(ctx context.Context) (context.Context, func()) {
	bound, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.lifetime, cancel)
	if s.lifetime.Err() != nil {
		cancel()
	}
	return bound, func() { stop(); cancel() }
}

// Close cancels this incarnation, joins its admitted work and releases retained
// preparations. No later operation can reopen the service. The workspace owner
// calls Close before reporting its own teardown complete.
func (s *Service) Close() {
	s.closeOnce.Do(func() {
		s.cancel()
		// All worker admission occurs under gate. Cross it after cancellation
		// before Wait, so no worker can be added concurrently with the wait.
		s.gate <- struct{}{}
		for _, login := range s.logins {
			login.cancel()
		}
		<-s.gate
		s.workers.Wait()
		s.gate <- struct{}{}
		for _, login := range s.logins {
			login.clearPrivate()
		}
		s.logins, s.loginIDs = nil, nil
		s.keyChecks, s.keyCheckIDs = nil, nil
		s.receipts, s.receiptIDs = nil, nil
		s.reloads, s.reloadIDs = nil, nil
		<-s.gate
	})
}
