// Package callbackrelay receives a code on the user's computer for exchange by
// the selected workspace owner. It never exchanges or persists a credential.
package callbackrelay

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/example-git/crux/internal/oauth"
)

const InputLimit = 16 << 10

type Relay struct{ state *relayState }

type relayState struct {
	mu        sync.Mutex
	port      uint16
	server    *http.Server
	done      chan struct{}
	served    chan struct{}
	cancel    context.CancelFunc
	closeOnce sync.Once
	input     string
	err       error
	received  bool
	closed    bool
}

func (Relay) Format(s fmt.State, _ rune) { _, _ = s.Write([]byte("[private OAuth callback relay]")) }
func (Relay) MarshalJSON() ([]byte, error) {
	return nil, errors.New("OAuth callback relays are private")
}

// Start binds before the caller requests an authorization URL from the owner.
// A fixed-port conflict is a visible failure; it never selects another port.
func Start(ctx context.Context, requirement oauth.CallbackRequirement) (*Relay, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := requirement.Validate(); err != nil {
		return nil, err
	}
	if requirement.Mode != "loopback-fixed" && requirement.Mode != "loopback-dynamic" {
		return nil, errors.New("OAuth callback requires a loopback declaration")
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("localhost", strconv.Itoa(int(requirement.Port))))
	if err != nil {
		return nil, fmt.Errorf("bind OAuth callback: %w", err)
	}
	lifetime, cancel := context.WithCancel(ctx)
	r := &relayState{port: uint16(listener.Addr().(*net.TCPAddr).Port), done: make(chan struct{}), served: make(chan struct{}), cancel: cancel}
	r.server = &http.Server{ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: InputLimit + 4096, Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if req.URL.EscapedPath() != requirement.Path {
			http.NotFound(w, req)
			return
		}
		if req.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "Use GET for this callback.", http.StatusMethodNotAllowed)
			return
		}
		input := req.URL.RawQuery
		if len(input) > InputLimit {
			http.Error(w, "Authorization response is too large.", http.StatusRequestEntityTooLarge)
			return
		}
		if input == "" || !utf8.ValidString(input) || strings.ContainsRune(input, 0) {
			http.Error(w, "Authorization response is invalid.", http.StatusBadRequest)
			return
		}
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			http.Error(w, "This login has closed.", http.StatusGone)
			return
		}
		if r.received {
			r.mu.Unlock()
			http.Error(w, "An authorization response was already received.", http.StatusConflict)
			return
		}
		r.input, r.received = input, true
		close(r.done)
		r.mu.Unlock()
		_, _ = w.Write([]byte("Authorization response received. Return to Crux to finish login.\n"))
	})}
	go func() {
		defer close(r.served)
		err := r.server.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			r.mu.Lock()
			if !r.received && !r.closed {
				r.err = errors.New("OAuth callback listener stopped")
				r.closed = true
				close(r.done)
			}
			r.mu.Unlock()
		}
	}()
	context.AfterFunc(lifetime, func() { _ = r.close() })
	if err := ctx.Err(); err != nil {
		_ = r.close()
		return nil, err
	}
	return &Relay{state: r}, nil
}

func (r *Relay) Port() uint16 { return r.state.port }

// Wait cancellation stops only the wait. A received query is retained across
// caller retries and Close so its original submission can be reconciled.
func (r *Relay) Wait(ctx context.Context) (string, error) { return r.state.wait(ctx) }

func (r *relayState) wait(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-r.done:
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.received {
		return r.input, nil
	}
	if r.err != nil {
		return "", r.err
	}
	return "", context.Canceled
}

// Close cancels pending reads and waits until this listener has stopped. It
// never affects another relay or the owner's admitted exchange/commit receipt.
func (r *Relay) Close() error {
	if r == nil {
		return nil
	}
	return r.state.close()
}

func (r *relayState) close() error {
	r.closeOnce.Do(func() {
		r.cancel()
		r.mu.Lock()
		if !r.received && !r.closed {
			close(r.done)
		}
		r.closed = true
		r.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := r.server.Shutdown(ctx); err != nil {
			_ = r.server.Close()
		}
		<-r.served
	})
	return nil
}
