package trafficcapture

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"

	"github.com/stretchr/testify/require"
)

func wireGuardTestKeys(t *testing.T) ([32]byte, [32]byte) {
	t.Helper()
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	require.NoError(t, err)
	return [32]byte(key.Bytes()), [32]byte(key.PublicKey().Bytes())
}

func wireGuardTestPeer(t *testing.T, privateKey, peerKey [32]byte) (*wireGuardPacketDevice, netip.AddrPort) {
	t.Helper()
	packets := newWireGuardPacketDevice(1280)
	engine := device.NewDevice(packets, conn.NewDefaultBind(), device.NewLogger(device.LogLevelSilent, ""))
	t.Cleanup(engine.Close)
	require.NoError(t, engine.IpcSet("private_key="+hex.EncodeToString(privateKey[:])+"\npublic_key="+hex.EncodeToString(peerKey[:])+"\nallowed_ip=0.0.0.0/0\nallowed_ip=::/0\n"))
	require.NoError(t, engine.Up())
	state, err := engine.IpcGet()
	require.NoError(t, err)
	var port uint64
	for _, line := range strings.Split(state, "\n") {
		if value, found := strings.CutPrefix(line, "listen_port="); found {
			port, err = strconv.ParseUint(value, 10, 16)
			require.NoError(t, err)
		}
	}
	require.NotZero(t, port)
	return packets, netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(port))
}

func wireGuardTestRelay(t *testing.T, server netip.AddrPort) (netip.AddrPort, <-chan []byte) {
	t.Helper()
	relay, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	require.NoError(t, err)
	done := make(chan struct{})
	observed := make(chan []byte, 128)
	go func() {
		defer close(done)
		var client netip.AddrPort
		buffer := make([]byte, 65535)
		for {
			count, source, err := relay.ReadFromUDPAddrPort(buffer)
			if err != nil {
				return
			}
			select {
			case observed <- bytes.Clone(buffer[:count]):
			default:
			}
			destination := server
			if source == server {
				destination = client
			} else {
				client = source
			}
			if destination.IsValid() {
				_, _ = relay.WriteToUDPAddrPort(buffer[:count], destination)
			}
		}
	}()
	t.Cleanup(func() { _ = relay.Close(); <-done })
	return relay.LocalAddr().(*net.UDPAddr).AddrPort(), observed
}

func wireGuardTestPacket(version int, reverse bool, payload string) []byte {
	if version == 6 {
		packet := make([]byte, 40+len(payload))
		packet[0] = 0x60
		binary.BigEndian.PutUint16(packet[4:6], uint16(len(payload)))
		packet[6], packet[7] = 253, 64
		first, second := netip.MustParseAddr("fd00::1").As16(), netip.MustParseAddr("fd00::2").As16()
		if reverse {
			first, second = second, first
		}
		copy(packet[8:24], first[:])
		copy(packet[24:40], second[:])
		copy(packet[40:], payload)
		return packet
	}
	packet := make([]byte, 20+len(payload))
	packet[0], packet[8], packet[9] = 0x45, 64, 253
	binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
	first, second := []byte{10, 0, 0, 1}, []byte{10, 0, 0, 2}
	if reverse {
		first, second = second, first
	}
	copy(packet[12:16], first)
	copy(packet[16:20], second)
	var sum uint32
	for offset := 0; offset < 20; offset += 2 {
		sum += uint32(binary.BigEndian.Uint16(packet[offset : offset+2]))
	}
	for sum > 65535 {
		sum = sum&65535 + sum>>16
	}
	binary.BigEndian.PutUint16(packet[10:12], ^uint16(sum))
	copy(packet[20:], payload)
	return packet
}

func TestWireGuardEncryptedPacketExchange(t *testing.T) {
	clientPrivate, clientPublic := wireGuardTestKeys(t)
	serverPrivate, serverPublic := wireGuardTestKeys(t)
	server, endpoint := wireGuardTestPeer(t, serverPrivate, clientPublic)
	relay, observed := wireGuardTestRelay(t, endpoint)
	tunnel, err := newWireGuardTunnel(wireGuardSettings{
		PrivateKey: clientPrivate, PeerPublicKey: serverPublic, Endpoint: relay, MTU: 1280,
		AllowedIPs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")},
	})
	require.NoError(t, err)
	t.Cleanup(tunnel.close)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	marker := "crux-wireguard-private-payload"
	for _, version := range []int{4, 6} {
		outgoing := wireGuardTestPacket(version, false, marker)
		require.NoError(t, tunnel.sendPacket(ctx, outgoing))
		select {
		case received := <-server.outgoing:
			require.Equal(t, outgoing, received)
		case <-ctx.Done():
			t.Fatal("WireGuard peer did not decrypt the outgoing packet")
		}
		incoming := wireGuardTestPacket(version, true, marker+"-reply")
		server.incoming <- incoming
		received, err := tunnel.receivePacket(ctx)
		require.NoError(t, err)
		require.Equal(t, incoming, received)
	}
	var count int
	for len(observed) > 0 {
		count++
		datagram := <-observed
		require.NotContains(t, string(datagram), marker)
	}
	require.GreaterOrEqual(t, count, 6)
	tunnel.close()
	require.ErrorIs(t, tunnel.sendPacket(ctx, wireGuardTestPacket(4, false, marker)), net.ErrClosed)
	_, err = tunnel.receivePacket(ctx)
	require.ErrorIs(t, err, net.ErrClosed)
}

func TestWireGuardConfigurationParsing(t *testing.T) {
	privateKey, publicKey := wireGuardTestKeys(t)
	configuration := fmt.Sprintf("[Interface]\nPrivateKey = %s\nAddress = 10.0.0.1/32\nDNS = 10.0.0.53\n\n[Peer]\nPublicKey = %s\nAllowedIPs = 0.0.0.0/0, ::/0\nEndpoint = 127.0.0.1:51820\n", base64.StdEncoding.EncodeToString(privateKey[:]), base64.StdEncoding.EncodeToString(publicKey[:]))
	settings, err := parseWireGuardSettings(configuration)
	require.NoError(t, err)
	require.Equal(t, privateKey, settings.PrivateKey)
	require.Equal(t, publicKey, settings.PeerPublicKey)
	require.Len(t, settings.AllowedIPs, 2)
	require.Equal(t, netip.MustParseAddr("10.0.0.53"), settings.DNS[0])
	for _, invalid := range []string{
		strings.Replace(configuration, "127.0.0.1:51820", "unresolved.example:51820", 1),
		strings.Replace(configuration, "PrivateKey = "+base64.StdEncoding.EncodeToString(privateKey[:]), "PrivateKey = invalid-secret", 1),
		configuration + "Endpoint = 127.0.0.1:1\n",
		configuration + "Typo = value\n",
		"[Interface]\nAddress = 10.0.0.1/32\n",
	} {
		_, err := parseWireGuardSettings(invalid)
		require.Error(t, err)
		require.NotContains(t, err.Error(), "invalid-secret")
		require.NotContains(t, err.Error(), base64.StdEncoding.EncodeToString(privateKey[:]))
	}
}

func TestWireGuardBridgeStopsOnInvalidFrameAndCancellation(t *testing.T) {
	privateKey, publicKey := wireGuardTestKeys(t)
	settings := wireGuardSettings{
		PrivateKey: privateKey, PeerPublicKey: publicKey, Endpoint: netip.MustParseAddrPort("127.0.0.1:1"), MTU: 1280,
		AllowedIPs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")},
	}
	for _, cancelOnly := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		local, remote := net.Pipe()
		done := make(chan error, 1)
		go func() { done <- bridgeWireGuardPackets(ctx, settings, local) }()
		if cancelOnly {
			cancel()
		} else {
			require.NoError(t, binary.Write(remote, binary.BigEndian, uint32(65536)))
		}
		select {
		case err := <-done:
			require.Error(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("WireGuard packet bridge did not terminate")
		}
		cancel()
		_ = remote.Close()
	}
}
