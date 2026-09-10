package notification

import tea "github.com/example-git/crux/foundation/bubbletea"

// NoopBackend is a no-op notification backend that does nothing.
// This is the default backend used when notifications are not supported.
type NoopBackend struct{}

// Send does nothing and returns nil.
func (NoopBackend) Send(_ Notification) tea.Cmd {
	return nil
}
