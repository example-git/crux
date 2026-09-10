package workspace

import "github.com/example-git/crux/internal/config"

// AuthorityViewProvider exposes only the accepted, redacted identity already
// cached by a workspace. Presentation must not collect credentials or perform
// network requests to determine which authority is in use.
type AuthorityViewProvider interface {
	AcceptedAuthority() *config.RemoteAuthority
}

// RemoteAddressProvider exposes the connection endpoint already held locally.
type RemoteAddressProvider interface {
	RemoteAddress() string
}

func (w *ClientWorkspace) RemoteAddress() string {
	return w.client.RemoteAddress()
}

func (w *ClientWorkspace) AcceptedAuthority() *config.RemoteAuthority {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.ws.Authority == nil {
		return nil
	}
	result := *w.ws.Authority
	result.Accounts = append([]config.RemoteAccountIdentity(nil), result.Accounts...)
	return &result
}
