package trafficcapture

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"
)

type wireGuardBridge struct {
	listener   net.Listener
	token      string
	settings   wireGuardSettings
	cancel     context.CancelFunc
	done       chan struct{}
	mu         sync.Mutex
	connection net.Conn
}

func startWireGuardBridge(configuration string) (*wireGuardBridge, error) {
	settings, err := parseWireGuardSettings(configuration)
	if err != nil {
		return nil, err
	}
	settings.AllowedIPs = wireGuardRoutePrefixes()
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		_ = listener.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	bridge := &wireGuardBridge{
		listener: listener, token: base64.RawURLEncoding.EncodeToString(secret), settings: settings,
		cancel: cancel, done: make(chan struct{}),
	}
	go bridge.serve(ctx)
	return bridge, nil
}

func (bridge *wireGuardBridge) serve(ctx context.Context) {
	defer close(bridge.done)
	for {
		connection, err := bridge.listener.Accept()
		if err != nil {
			return
		}
		_ = connection.SetReadDeadline(time.Now().Add(10 * time.Second))
		hello := make([]byte, len(bridge.token)+1)
		_, err = io.ReadFull(connection, hello)
		if err != nil || hello[len(hello)-1] != '\n' || subtle.ConstantTimeCompare(hello[:len(hello)-1], []byte(bridge.token)) != 1 {
			_ = connection.Close()
			continue
		}
		_ = connection.SetReadDeadline(time.Time{})
		bridge.mu.Lock()
		bridge.connection = connection
		bridge.mu.Unlock()
		if _, err := connection.Write([]byte{1}); err == nil {
			_ = bridgeWireGuardPackets(ctx, bridge.settings, connection)
		}
		_ = connection.Close()
		bridge.mu.Lock()
		bridge.connection = nil
		bridge.mu.Unlock()
	}
}

func (bridge *wireGuardBridge) providerConfiguration() (map[string]any, error) {
	configuration := map[string]any{
		"bridgePort":  bridge.listener.Addr().(*net.TCPAddr).Port,
		"bridgeToken": bridge.token,
		"dns":         []string{},
	}
	for _, prefix := range bridge.settings.Addresses {
		if prefix.Addr().Is4() {
			configuration["ipv4Address"] = prefix.Addr().String()
		} else {
			configuration["ipv6Address"] = prefix.Addr().String()
		}
	}
	if _, found := configuration["ipv4Address"]; !found {
		return nil, fmt.Errorf("mitmproxy WireGuard configuration did not supply an IPv4 address")
	}
	dns := make([]string, len(bridge.settings.DNS))
	for index, address := range bridge.settings.DNS {
		dns[index] = address.String()
	}
	configuration["dns"] = dns
	return configuration, nil
}

func (bridge *wireGuardBridge) close() error {
	bridge.cancel()
	err := bridge.listener.Close()
	bridge.mu.Lock()
	if bridge.connection != nil {
		_ = bridge.connection.Close()
	}
	bridge.mu.Unlock()
	<-bridge.done
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func wireGuardRoutePrefixes() []netip.Prefix {
	return []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")}
}
