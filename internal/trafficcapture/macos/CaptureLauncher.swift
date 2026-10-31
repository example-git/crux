import CoreServices
import Foundation
import NetworkExtension
import Security

private let captureIdentity = "@CAPTURE_ID@"

struct CaptureLaunchConfiguration: Decodable {
    let command: [String]
    let environment: [String: String]
    let workingDirectory: String
    let providerConfiguration: [String: JSONValue]
    let statusPath: String
    let terminalPath: String
}

indirect enum JSONValue: Decodable {
    case string(String)
    case number(Int)
    case array([JSONValue])

    init(from decoder: Decoder) throws {
        let container = try decoder.singleValueContainer()
        if let value = try? container.decode(String.self) { self = .string(value) }
        else if let value = try? container.decode(Int.self) { self = .number(value) }
        else { self = .array(try container.decode([JSONValue].self)) }
    }

    var value: Any {
        switch self {
        case .string(let value): return value
        case .number(let value): return value
        case .array(let values): return values.map { $0.value }
        }
    }
}

func saveStatus(_ path: String, _ value: [String: Any]) throws {
    let data = try JSONSerialization.data(withJSONObject: value, options: [.sortedKeys])
    try data.write(to: URL(fileURLWithPath: path), options: [.atomic])
    try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: path)
}

func captureRequirement() throws -> (String, String) {
    let application = Bundle.main.bundleURL
    let status = LSRegisterURL(application as CFURL, true)
    guard status == noErr else { throw NSError(domain: NSOSStatusErrorDomain, code: Int(status)) }
    var code: SecStaticCode?
    let created = SecStaticCodeCreateWithPath(application as CFURL, [], &code)
    guard created == errSecSuccess, let code else { throw NSError(domain: NSOSStatusErrorDomain, code: Int(created)) }
    var requirement: SecRequirement?
    let copied = SecCodeCopyDesignatedRequirement(code, [], &requirement)
    guard copied == errSecSuccess, let requirement else { throw NSError(domain: NSOSStatusErrorDomain, code: Int(copied)) }
    var requirementText: CFString?
    let rendered = SecRequirementCopyString(requirement, [], &requirementText)
    guard rendered == errSecSuccess, let requirementText else {
        throw NSError(domain: NSOSStatusErrorDomain, code: Int(rendered))
    }
    return (captureIdentity, requirementText as String)
}

func runCapture() async -> Int32 {
    guard CommandLine.arguments.count == 2 else {
        fputs("Capture launcher requires one configuration file\n", stderr)
        return 2
    }
    let path = CommandLine.arguments[1]
    var statusPath: String?
    var savedManager: NETunnelProviderManager?
    var child: Process?
    var result: Int32 = 1
    do {
        let configuration = try JSONDecoder().decode(CaptureLaunchConfiguration.self, from: Data(contentsOf: URL(fileURLWithPath: path)))
        try FileManager.default.removeItem(atPath: path)
        statusPath = configuration.statusPath
        guard !configuration.command.isEmpty else {
            throw NSError(domain: "CruxCapture", code: 1, userInfo: [NSLocalizedDescriptionKey: "Empty capture manager command"])
        }
        let (identifier, requirement) = try captureRequirement()
        let rule = NEAppRule(signingIdentifier: identifier, designatedRequirement: requirement)
        rule.matchPath = Bundle.main.executableURL?.resolvingSymlinksInPath().path
        rule.matchTools = nil
        let manager = NETunnelProviderManager.forPerAppVPN()
        let tunnel = NETunnelProviderProtocol()
        tunnel.providerBundleIdentifier = identifier + ".packet-tunnel"
        tunnel.providerConfiguration = configuration.providerConfiguration.mapValues { $0.value }
        tunnel.serverAddress = "127.0.0.1"
        tunnel.includeAllNetworks = true
        tunnel.enforceRoutes = true
        tunnel.excludeLocalNetworks = false
        tunnel.excludeAPNs = false
        manager.protocolConfiguration = tunnel
        manager.appRules = [rule]
        manager.localizedDescription = "Crux capture " + identifier
        manager.isEnabled = true
        let demand = NEOnDemandRuleConnect()
        demand.interfaceTypeMatch = .any
        manager.onDemandRules = [demand]
        manager.isOnDemandEnabled = true
        try await manager.saveToPreferences()
        savedManager = manager
        try await manager.loadFromPreferences()
        try manager.connection.startVPNTunnel()
        let deadline = Date().addingTimeInterval(60)
        while manager.connection.status != .connected && Date() < deadline {
            try await Task.sleep(nanoseconds: 100_000_000)
        }
        guard manager.connection.status == .connected else {
            try await manager.connection.fetchLastDisconnectError()
            throw NSError(domain: "CruxCapture", code: 2, userInfo: [NSLocalizedDescriptionKey: "Capture packet tunnel did not activate; no target was launched"])
        }
        try saveStatus(configuration.statusPath, ["state": "connected", "bundle_identifier": identifier, "helpers_included": true])
        let process = Process()
        process.executableURL = URL(fileURLWithPath: configuration.command[0])
        process.arguments = Array(configuration.command.dropFirst())
        process.environment = configuration.environment
        process.currentDirectoryURL = URL(fileURLWithPath: configuration.workingDirectory)
        let terminal = try FileHandle(forUpdating: URL(fileURLWithPath: configuration.terminalPath))
        defer { try? terminal.close() }
        process.standardInput = terminal
        process.standardOutput = terminal
        process.standardError = terminal
        child = process
        result = try await withCheckedThrowingContinuation { continuation in
            process.terminationHandler = { completed in continuation.resume(returning: completed.terminationStatus) }
            do { try process.run() } catch { continuation.resume(throwing: error) }
        }
    } catch {
        if let statusPath {
            try? saveStatus(statusPath, ["state": "failed", "error": error.localizedDescription])
        }
        fputs("Capture policy failed: \(error.localizedDescription)\n", stderr)
    }
    if let child, child.isRunning {
        child.terminate()
        child.waitUntilExit()
    }
    if let manager = savedManager {
        manager.connection.stopVPNTunnel()
        do { try await manager.removeFromPreferences() }
        catch {
            if let statusPath { try? saveStatus(statusPath, ["state": "cleanup_failed", "error": error.localizedDescription]) }
            fputs("Capture policy cleanup failed: \(error.localizedDescription)\n", stderr)
            result = 1
        }
    }
    return result
}

signal(SIGINT, SIG_IGN)
Task { exit(await runCapture()) }
dispatchMain()
