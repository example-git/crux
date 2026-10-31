//go:build embedded_mitmproxy && (darwin || linux)

package trafficcapture

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func init() {
	captureTestCommands["__capture-wireguard-interop-server"] = func(arguments []string) int {
		_ = os.Setenv("CRUX_WG_TEST_READY", arguments[0])
		_ = os.Setenv("CRUX_WG_TEST_RELEASE", arguments[1])
		if err := runEmbeddedPython(`import asyncio, json, os
from pathlib import Path
from mitmproxy_rs import wireguard
async def echo(stream):
    data = await stream.read(65535)
    stream.write(b"reply:" + data)
    await stream.drain()
async def run():
    server_key, client_key = wireguard.genkey(), wireguard.genkey()
    server = await wireguard.start_wireguard_server("127.0.0.1", 0, server_key, [wireguard.pubkey(client_key)], echo, echo)
    host, port = server.getsockname()
    configuration = f"[Interface]\nPrivateKey = {client_key}\nAddress = 10.0.0.1/32\nDNS = 10.0.0.53\n[Peer]\nPublicKey = {wireguard.pubkey(server_key)}\nAllowedIPs = 0.0.0.0/0\nEndpoint = {host}:{port}\n"
    path = Path(os.environ["CRUX_WG_TEST_READY"])
    with path.open("w") as output:
        path.chmod(0o600)
        json.dump({"configuration": configuration}, output)
    while not Path(os.environ["CRUX_WG_TEST_RELEASE"]).exists():
        await asyncio.sleep(0.05)
    server.close()
    await server.wait_closed()
asyncio.run(run())
`); err != nil {
			return 1
		}
		return 0
	}
}

func TestEmbeddedWireGuardProtocolInterop(t *testing.T) {
	directory := t.TempDir()
	ready, release := filepath.Join(directory, "ready.json"), filepath.Join(directory, "release")
	executable, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	server := exec.CommandContext(ctx, executable, "__capture-wireguard-interop-server", ready, release)
	require.NoError(t, server.Start())
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o600); require.NoError(t, server.Wait()) })
	var state struct {
		Configuration string `json:"configuration"`
	}
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(ready)
		return err == nil && json.Unmarshal(data, &state) == nil
	}, 10*time.Second, 50*time.Millisecond)
	settings, err := parseWireGuardSettings(state.Configuration)
	require.NoError(t, err)
	tunnel, err := newWireGuardTunnel(settings)
	require.NoError(t, err)
	defer tunnel.close()
	payload := []byte("crux-udp-interop")
	udp := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint16(udp[:2], 23456)
	binary.BigEndian.PutUint16(udp[2:4], 9999)
	binary.BigEndian.PutUint16(udp[4:6], uint16(len(udp)))
	copy(udp[8:], payload)
	packet := wireGuardTestPacket(4, false, string(udp))
	packet[9] = 17
	packet[10], packet[11] = 0, 0
	var sum uint32
	for offset := 0; offset < 20; offset += 2 {
		sum += uint32(binary.BigEndian.Uint16(packet[offset : offset+2]))
	}
	for sum > 65535 {
		sum = sum&65535 + sum>>16
	}
	binary.BigEndian.PutUint16(packet[10:12], ^uint16(sum))
	require.NoError(t, tunnel.sendPacket(ctx, packet))
	response, err := tunnel.receivePacket(ctx)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(response), 28)
	require.Equal(t, "reply:"+string(payload), string(response[28:]))
}
