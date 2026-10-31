package trafficcapture

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"
)

type wireGuardSettings struct {
	PrivateKey    [32]byte
	PeerPublicKey [32]byte
	Endpoint      netip.AddrPort
	AllowedIPs    []netip.Prefix
	Addresses     []netip.Prefix
	DNS           []netip.Addr
	MTU           int
}

func parseWireGuardSettings(configuration string) (wireGuardSettings, error) {
	settings := wireGuardSettings{MTU: 1280}
	section := ""
	seen := make(map[string]bool)
	for _, line := range strings.Split(configuration, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line[1 : len(line)-1]
			if section != "Interface" && section != "Peer" {
				return settings, fmt.Errorf("unsupported WireGuard section: %s", section)
			}
			if seen[section] {
				return settings, fmt.Errorf("duplicate WireGuard section: %s", section)
			}
			seen[section] = true
			continue
		}
		name, value, found := strings.Cut(line, "=")
		if !found {
			return settings, fmt.Errorf("invalid WireGuard configuration line")
		}
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		key := section + "." + name
		if seen[key] {
			return settings, fmt.Errorf("duplicate WireGuard setting: %s", key)
		}
		seen[key] = true
		switch key {
		case "Interface.PrivateKey", "Peer.PublicKey":
			decoded, err := base64.StdEncoding.DecodeString(value)
			if err != nil || len(decoded) != 32 {
				return settings, fmt.Errorf("invalid WireGuard key in %s", key)
			}
			if key == "Interface.PrivateKey" {
				copy(settings.PrivateKey[:], decoded)
			} else {
				copy(settings.PeerPublicKey[:], decoded)
			}
		case "Interface.Address", "Peer.AllowedIPs":
			for _, item := range strings.Split(value, ",") {
				prefix, err := netip.ParsePrefix(strings.TrimSpace(item))
				if err != nil {
					return settings, fmt.Errorf("invalid WireGuard prefix in %s", key)
				}
				if key == "Interface.Address" {
					settings.Addresses = append(settings.Addresses, prefix)
				} else {
					settings.AllowedIPs = append(settings.AllowedIPs, prefix.Masked())
				}
			}
		case "Interface.DNS":
			for _, item := range strings.Split(value, ",") {
				address, err := netip.ParseAddr(strings.TrimSpace(item))
				if err != nil {
					return settings, fmt.Errorf("invalid WireGuard DNS address")
				}
				settings.DNS = append(settings.DNS, address)
			}
		case "Peer.Endpoint":
			endpoint, err := netip.ParseAddrPort(value)
			if err != nil || endpoint.Port() == 0 {
				return settings, fmt.Errorf("WireGuard endpoint must be an IP address and nonzero port")
			}
			settings.Endpoint = endpoint
		default:
			return settings, fmt.Errorf("unsupported WireGuard setting: %s", key)
		}
	}
	for _, key := range []string{"Interface.PrivateKey", "Interface.Address", "Peer.PublicKey", "Peer.Endpoint", "Peer.AllowedIPs"} {
		if !seen[key] {
			return settings, fmt.Errorf("missing WireGuard setting: %s", key)
		}
	}
	return settings, nil
}

type wireGuardPacketDevice struct {
	mtu      int
	incoming chan []byte
	outgoing chan []byte
	events   chan tun.Event
	closed   chan struct{}
	once     sync.Once
}

func newWireGuardPacketDevice(mtu int) *wireGuardPacketDevice {
	return &wireGuardPacketDevice{
		mtu: mtu, incoming: make(chan []byte, 64), outgoing: make(chan []byte, 64),
		events: make(chan tun.Event), closed: make(chan struct{}),
	}
}

func (packets *wireGuardPacketDevice) File() *os.File           { return nil }
func (packets *wireGuardPacketDevice) MTU() (int, error)        { return packets.mtu, nil }
func (packets *wireGuardPacketDevice) Name() (string, error)    { return "crux-capture", nil }
func (packets *wireGuardPacketDevice) Events() <-chan tun.Event { return packets.events }
func (packets *wireGuardPacketDevice) BatchSize() int           { return 1 }

func (packets *wireGuardPacketDevice) Read(buffers [][]byte, sizes []int, offset int) (int, error) {
	if len(buffers) == 0 || len(sizes) == 0 || offset < 0 || offset > len(buffers[0]) {
		return 0, io.ErrShortBuffer
	}
	select {
	case <-packets.closed:
		return 0, net.ErrClosed
	case packet := <-packets.incoming:
		if len(packet) > len(buffers[0])-offset {
			return 0, io.ErrShortBuffer
		}
		sizes[0] = copy(buffers[0][offset:], packet)
		return 1, nil
	}
}

func (packets *wireGuardPacketDevice) Write(buffers [][]byte, offset int) (int, error) {
	for index, buffer := range buffers {
		if offset < 0 || offset > len(buffer) {
			return index, io.ErrShortBuffer
		}
		packet := append([]byte{}, buffer[offset:]...)
		select {
		case <-packets.closed:
			return index, net.ErrClosed
		case packets.outgoing <- packet:
		}
	}
	return len(buffers), nil
}

func (packets *wireGuardPacketDevice) Close() error {
	packets.once.Do(func() {
		close(packets.closed)
		close(packets.events)
	})
	return nil
}

type wireGuardTunnel struct {
	device  *device.Device
	packets *wireGuardPacketDevice
}

func newWireGuardTunnel(settings wireGuardSettings) (*wireGuardTunnel, error) {
	if !settings.Endpoint.IsValid() || settings.Endpoint.Port() == 0 {
		return nil, fmt.Errorf("a WireGuard endpoint is required")
	}
	if settings.PrivateKey == [32]byte{} || settings.PeerPublicKey == [32]byte{} {
		return nil, fmt.Errorf("WireGuard keys must not be empty")
	}
	if settings.MTU < 1280 || settings.MTU > 65535 {
		return nil, fmt.Errorf("WireGuard MTU must be between 1280 and 65535")
	}
	if len(settings.AllowedIPs) == 0 {
		return nil, fmt.Errorf("WireGuard allowed IPs are required")
	}
	configuration := "private_key=" + hex.EncodeToString(settings.PrivateKey[:]) + "\nreplace_peers=true\npublic_key=" + hex.EncodeToString(settings.PeerPublicKey[:]) + "\nendpoint=" + settings.Endpoint.String() + "\n"
	for _, prefix := range settings.AllowedIPs {
		if !prefix.IsValid() {
			return nil, fmt.Errorf("invalid WireGuard allowed IP prefix")
		}
		configuration += "allowed_ip=" + prefix.Masked().String() + "\n"
	}
	packets := newWireGuardPacketDevice(settings.MTU)
	engine := device.NewDevice(packets, conn.NewDefaultBind(), device.NewLogger(device.LogLevelSilent, ""))
	if err := engine.IpcSet(configuration); err != nil {
		engine.Close()
		return nil, fmt.Errorf("configure capture WireGuard device: %w", err)
	}
	if err := engine.Up(); err != nil {
		engine.Close()
		return nil, fmt.Errorf("start capture WireGuard device: %w", err)
	}
	return &wireGuardTunnel{device: engine, packets: packets}, nil
}

func (tunnel *wireGuardTunnel) sendPacket(ctx context.Context, packet []byte) error {
	if len(packet) == 0 || len(packet) > tunnel.packets.mtu {
		return fmt.Errorf("packet length must be between 1 and %d", tunnel.packets.mtu)
	}
	select {
	case <-tunnel.packets.closed:
		return net.ErrClosed
	default:
	}
	copyOfPacket := append([]byte{}, packet...)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-tunnel.packets.closed:
		return net.ErrClosed
	case tunnel.packets.incoming <- copyOfPacket:
		return nil
	}
}

func (tunnel *wireGuardTunnel) receivePacket(ctx context.Context) ([]byte, error) {
	select {
	case <-tunnel.packets.closed:
		return nil, net.ErrClosed
	default:
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-tunnel.packets.closed:
		return nil, net.ErrClosed
	case packet := <-tunnel.packets.outgoing:
		return packet, nil
	}
}

func (tunnel *wireGuardTunnel) close() {
	tunnel.device.Close()
}

func bridgeWireGuardPackets(ctx context.Context, settings wireGuardSettings, connection net.Conn) error {
	tunnel, err := newWireGuardTunnel(settings)
	if err != nil {
		_ = connection.Close()
		return err
	}
	defer tunnel.close()
	defer connection.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, func() {
		_ = connection.Close()
		tunnel.close()
	})
	defer stop()
	done := make(chan error, 2)
	go func() {
		for {
			var size uint32
			if err := binary.Read(connection, binary.BigEndian, &size); err != nil {
				done <- err
				return
			}
			if size == 0 || size > uint32(settings.MTU) {
				done <- fmt.Errorf("invalid WireGuard packet frame length: %d", size)
				return
			}
			packet := make([]byte, size)
			if _, err := io.ReadFull(connection, packet); err != nil {
				done <- err
				return
			}
			if err := tunnel.sendPacket(ctx, packet); err != nil {
				done <- err
				return
			}
		}
	}()
	go func() {
		for {
			packet, err := tunnel.receivePacket(ctx)
			if err != nil {
				done <- err
				return
			}
			if err := binary.Write(connection, binary.BigEndian, uint32(len(packet))); err != nil {
				done <- err
				return
			}
			if _, err := io.Copy(connection, bytes.NewReader(packet)); err != nil {
				done <- err
				return
			}
		}
	}()
	result := <-done
	cancel()
	<-done
	return result
}
