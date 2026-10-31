import Foundation
import Network
import NetworkExtension

@objc(CapturePacketTunnelProvider)
final class CapturePacketTunnelProvider: NEPacketTunnelProvider {
    private let queue = DispatchQueue(label: "org.crux.capture.packet-tunnel")
    private var connection: NWConnection?
    private var stopping = false
    private var pendingStart: ((Error?) -> Void)?
    private var mtu = 1280

    override func startTunnel(options: [String: NSObject]?, completionHandler: @escaping (Error?) -> Void) {
        guard let configuration = (protocolConfiguration as? NETunnelProviderProtocol)?.providerConfiguration,
              let portNumber = configuration["bridgePort"] as? Int,
              let portValue = UInt16(exactly: portNumber),
              let port = NWEndpoint.Port(rawValue: portValue),
              let token = configuration["bridgeToken"] as? String,
              let ipv4 = configuration["ipv4Address"] as? String,
              let dns = configuration["dns"] as? [String] else {
            completionHandler(NSError(domain: "CruxCapture", code: 1, userInfo: [NSLocalizedDescriptionKey: "Missing capture packet bridge configuration"]))
            return
        }
        pendingStart = completionHandler
        let settings = NEPacketTunnelNetworkSettings(tunnelRemoteAddress: "127.0.0.1")
        let ipv4Settings = NEIPv4Settings(addresses: [ipv4], subnetMasks: ["255.255.255.255"])
        ipv4Settings.includedRoutes = [.default()]
        settings.ipv4Settings = ipv4Settings
        let ipv6 = configuration["ipv6Address"] as? String ?? "fd00:6372:7578::1"
        let ipv6Settings = NEIPv6Settings(addresses: [ipv6], networkPrefixLengths: [128])
        ipv6Settings.includedRoutes = [.default()]
        settings.ipv6Settings = ipv6Settings
        settings.dnsSettings = NEDNSSettings(servers: dns)
        settings.dnsSettings?.matchDomains = [""]
        settings.mtu = NSNumber(value: mtu)
        setTunnelNetworkSettings(settings) { error in
            self.queue.async {
                if let error {
                    self.fail(error)
                    return
                }
                let connection = NWConnection(host: "127.0.0.1", port: port, using: .tcp)
                self.connection = connection
                connection.stateUpdateHandler = { state in
                    switch state {
                    case .ready:
                        var hello = Data(token.utf8)
                        hello.append(10)
                        connection.send(content: hello, completion: .contentProcessed { error in
                            self.queue.async {
                                if let error {
                                    self.fail(error)
                                    return
                                }
                                self.readExactly(1) { acknowledgement in
                                    guard acknowledgement == Data([1]) else {
                                        self.fail(NSError(domain: "CruxCapture", code: 2, userInfo: [NSLocalizedDescriptionKey: "Packet bridge authentication failed"]))
                                        return
                                    }
                                    self.pendingStart?(nil)
                                    self.pendingStart = nil
                                    self.readPackets()
                                    self.readFrame()
                                }
                            }
                        })
                    case .failed(let error):
                        self.fail(error)
                    case .cancelled:
                        if !self.stopping {
                            self.fail(NSError(domain: "CruxCapture", code: 3, userInfo: [NSLocalizedDescriptionKey: "Packet bridge disconnected"]))
                        }
                    default:
                        break
                    }
                }
                connection.start(queue: self.queue)
            }
        }
    }

    private func fail(_ error: Error) {
        guard !stopping else { return }
        stopping = true
        connection?.cancel()
        if let callback = pendingStart {
            pendingStart = nil
            callback(error)
        } else {
            cancelTunnelWithError(error)
        }
    }

    private func readPackets() {
        guard !stopping else { return }
        packetFlow.readPackets { packets, _ in
            self.queue.async {
                guard !self.stopping else { return }
                for packet in packets {
                    guard packet.count <= self.mtu else {
                        self.fail(NSError(domain: "CruxCapture", code: 4, userInfo: [NSLocalizedDescriptionKey: "Packet exceeds capture MTU"]))
                        return
                    }
                    var length = UInt32(packet.count).bigEndian
                    var frame = withUnsafeBytes(of: &length) { Data($0) }
                    frame.append(packet)
                    self.connection?.send(content: frame, completion: .contentProcessed { error in
                        if let error {
                            self.queue.async { self.fail(error) }
                        }
                    })
                }
                self.readPackets()
            }
        }
    }

    private func readExactly(_ count: Int, accumulated: Data = Data(), completion: @escaping (Data) -> Void) {
        guard !stopping, let connection else { return }
        let remaining = count - accumulated.count
        connection.receive(minimumIncompleteLength: remaining, maximumLength: remaining) { data, _, complete, error in
            self.queue.async {
                if let error {
                    self.fail(error)
                    return
                }
                var result = accumulated
                if let data { result.append(data) }
                if result.count == count {
                    completion(result)
                } else if complete {
                    self.fail(NSError(domain: "CruxCapture", code: 5, userInfo: [NSLocalizedDescriptionKey: "Packet bridge closed"]))
                } else {
                    self.readExactly(count, accumulated: result, completion: completion)
                }
            }
        }
    }

    private func readFrame() {
        readExactly(4) { header in
            let count = header.reduce(UInt32(0)) { ($0 << 8) | UInt32($1) }
            guard count > 0 && count <= self.mtu else {
                self.fail(NSError(domain: "CruxCapture", code: 6, userInfo: [NSLocalizedDescriptionKey: "Invalid packet bridge frame"]))
                return
            }
            self.readExactly(Int(count)) { packet in
                let version = packet[0] >> 4
                guard version == 4 || version == 6 else {
                    self.fail(NSError(domain: "CruxCapture", code: 7, userInfo: [NSLocalizedDescriptionKey: "Unknown packet IP version"]))
                    return
                }
                self.packetFlow.writePackets([packet], withProtocols: [NSNumber(value: version == 4 ? AF_INET : AF_INET6)])
                self.readFrame()
            }
        }
    }

    override func stopTunnel(with reason: NEProviderStopReason, completionHandler: @escaping () -> Void) {
        queue.async {
            self.stopping = true
            self.connection?.cancel()
            completionHandler()
        }
    }
}
