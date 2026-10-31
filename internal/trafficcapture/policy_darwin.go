package trafficcapture

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/muesli/cancelreader"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

//go:embed macos/CaptureLauncher.swift
var captureLauncherSource string

//go:embed macos/PacketTunnelProvider.swift
var capturePacketProviderSource string

func buildCapturePolicyLauncher(ctx context.Context, directory, identifier string) (string, error) {
	app := filepath.Join(directory, "Capture.app")
	contents := filepath.Join(app, "Contents")
	provider := filepath.Join(contents, "PlugIns", "CaptureTunnel.appex")
	for _, path := range []string{filepath.Join(contents, "MacOS"), filepath.Join(provider, "Contents", "MacOS")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return "", err
		}
	}
	launcherSource := filepath.Join(directory, "CaptureLauncher.swift")
	providerSource := filepath.Join(directory, "CapturePacketTunnel.swift")
	if err := os.WriteFile(launcherSource, []byte(strings.ReplaceAll(captureLauncherSource, "@CAPTURE_ID@", identifier)), 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(providerSource, []byte(capturePacketProviderSource), 0o600); err != nil {
		return "", err
	}
	launcher := filepath.Join(contents, "MacOS", "CaptureLauncher")
	tunnel := filepath.Join(provider, "Contents", "MacOS", "CaptureTunnel")
	commands := [][]string{
		{"xcrun", "swiftc", launcherSource, "-o", launcher},
		{"xcrun", "swiftc", "-parse-as-library", "-emit-executable", "-module-name", "CruxCaptureTunnel", "-Xlinker", "-e", "-Xlinker", "_NSExtensionMain", providerSource, "-o", tunnel},
	}
	for _, arguments := range commands {
		output, err := exec.CommandContext(ctx, arguments[0], arguments[1:]...).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("build native capture helper: %w: %s", err, output)
		}
	}
	appInfo := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict>
<key>CFBundleIdentifier</key><string>%s</string><key>CFBundleExecutable</key><string>CaptureLauncher</string>
<key>CFBundlePackageType</key><string>APPL</string><key>CFBundleName</key><string>Crux Capture</string>
<key>CFBundleVersion</key><string>1</string><key>CFBundleShortVersionString</key><string>1.0</string><key>LSUIElement</key><true/>
</dict></plist>`, identifier)
	providerInfo := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict>
<key>CFBundleIdentifier</key><string>%s.packet-tunnel</string><key>CFBundleExecutable</key><string>CaptureTunnel</string>
<key>CFBundlePackageType</key><string>XPC!</string><key>CFBundleName</key><string>Crux Capture Tunnel</string>
<key>CFBundleVersion</key><string>1</string><key>CFBundleShortVersionString</key><string>1.0</string>
<key>NSExtension</key><dict><key>NSExtensionPointIdentifier</key><string>com.apple.networkextension.packet-tunnel</string>
<key>NSExtensionPrincipalClass</key><string>CapturePacketTunnelProvider</string></dict></dict></plist>`, identifier)
	appEntitlements := `<?xml version="1.0"?><plist version="1.0"><dict><key>com.apple.developer.networking.networkextension</key><array><string>packet-tunnel-provider</string></array></dict></plist>`
	providerEntitlements := `<?xml version="1.0"?><plist version="1.0"><dict><key>com.apple.developer.networking.networkextension</key><array><string>packet-tunnel-provider</string></array><key>com.apple.security.app-sandbox</key><true/><key>com.apple.security.network.client</key><true/><key>com.apple.security.network.server</key><true/></dict></plist>`
	files := map[string]string{
		filepath.Join(contents, "Info.plist"):             appInfo,
		filepath.Join(provider, "Contents", "Info.plist"): providerInfo,
		filepath.Join(directory, "launcher.entitlements"): appEntitlements,
		filepath.Join(directory, "provider.entitlements"): providerEntitlements,
	}
	for path, data := range files {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			return "", err
		}
	}
	for _, signing := range [][2]string{{provider, "provider.entitlements"}, {app, "launcher.entitlements"}} {
		output, err := exec.CommandContext(ctx, "codesign", "--force", "--sign", "-", "--entitlements", filepath.Join(directory, signing[1]), signing[0]).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("ad-hoc sign native capture helper: %w: %s", err, output)
		}
	}
	for _, arguments := range [][]string{
		{"/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister", "-f", app},
		{"/usr/bin/pluginkit", "-a", provider},
	} {
		output, err := exec.CommandContext(ctx, arguments[0], arguments[1:]...).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("register native capture helper: %w: %s", err, output)
		}
	}
	return launcher, nil
}

func (manager *captureManager) initializeWireGuard(environment map[string]string, configuration string) (captureSessionStatus, error) {
	bridge, err := startWireGuardBridge(configuration)
	if err != nil {
		return captureSessionStatus{}, err
	}
	manager.packetBridge = bridge
	provider, err := bridge.providerConfiguration()
	if err != nil {
		return captureSessionStatus{}, err
	}
	identifier := "org.crux.capture." + strings.ToLower(manager.config.Session)
	launcher, err := buildCapturePolicyLauncher(context.Background(), manager.config.RuntimePath, identifier)
	if err != nil {
		return captureSessionStatus{}, err
	}
	childConfig := manager.config
	childConfig.Managed = true
	childConfig.DetachedTerminal = true
	childConfig.ControlSocket = filepath.Join(manager.socketDir, "managed.sock")
	manager.externalAddress = childConfig.ControlSocket
	configPath := filepath.Join(manager.config.RuntimePath, "manager.json")
	if err := writePrivateJSON(configPath, childConfig); err != nil {
		return captureSessionStatus{}, err
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		return captureSessionStatus{}, err
	}
	terminal, slave, err := openCaptureTerminal()
	if err != nil {
		return captureSessionStatus{}, err
	}
	defer slave.Close()
	terminalPath, err := captureTerminalPath(terminal)
	if err != nil {
		_ = terminal.Close()
		return captureSessionStatus{}, err
	}
	restore, err := relayCaptureTerminal(terminal)
	if err != nil {
		_ = terminal.Close()
		return captureSessionStatus{}, err
	}
	manager.restoreTerminal = restore
	policyPath := filepath.Join(manager.config.RuntimePath, "policy.json")
	policyStatus := filepath.Join(manager.config.RuntimePath, "policy-status.json")
	if err := writePrivateJSON(policyPath, map[string]any{
		"command":     []string{manager.executable, "__traffic-capture-manager", configPath},
		"environment": environmentMap(os.Environ()), "workingDirectory": workingDirectory,
		"providerConfiguration": provider, "statusPath": policyStatus, "terminalPath": terminalPath,
	}); err != nil {
		return captureSessionStatus{}, err
	}
	appPath := filepath.Dir(filepath.Dir(filepath.Dir(launcher)))
	manager.launcher = exec.CommandContext(context.Background(), "/usr/bin/open", "-W", "-n", appPath, "--args", policyPath)
	manager.launcher.Stdin, manager.launcher.Stdout, manager.launcher.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := manager.launcher.Start(); err != nil {
		manager.launcher = nil
		return captureSessionStatus{}, err
	}
	manager.launcherDone = make(chan struct{})
	go func() { _ = manager.launcher.Wait(); close(manager.launcherDone) }()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-manager.launcherDone:
			data, _ := os.ReadFile(policyStatus)
			return captureSessionStatus{}, fmt.Errorf("capture policy launcher exited before target startup: %s", data)
		default:
		}
		if _, err := os.Stat(childConfig.ControlSocket); err == nil {
			data, err := callCaptureControl(context.Background(), childConfig.ControlSocket, captureControlRequest{Action: "initialize", Environment: environment})
			if err != nil {
				return captureSessionStatus{}, err
			}
			var result captureSessionStatus
			if err := json.Unmarshal(data, &result); err != nil {
				return captureSessionStatus{}, err
			}
			manager.lastSession = result
			return result, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return captureSessionStatus{}, fmt.Errorf("capture app policy did not activate; no selected target was launched")
}

func relayCaptureTerminal(terminal *os.File) (func(), error) {
	state, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return nil, err
	}
	input, err := cancelreader.NewReader(os.Stdin)
	if err != nil {
		_ = term.Restore(int(os.Stdin.Fd()), state)
		return nil, err
	}
	inputDone := make(chan struct{})
	resize := func() {
		if size, err := unix.IoctlGetWinsize(int(os.Stdin.Fd()), unix.TIOCGWINSZ); err == nil {
			_ = unix.IoctlSetWinsize(int(terminal.Fd()), unix.TIOCSWINSZ, size)
		}
	}
	resize()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGWINCH)
	done := make(chan struct{})
	go func() { defer close(inputDone); _, _ = io.Copy(terminal, input) }()
	go func() { _, _ = io.Copy(os.Stdout, terminal) }()
	go func() {
		for {
			select {
			case <-signals:
				resize()
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(signals)
		close(done)
		input.Cancel()
		_ = terminal.Close()
		<-inputDone
		_ = input.Close()
		_ = term.Restore(int(os.Stdin.Fd()), state)
	}, nil
}
