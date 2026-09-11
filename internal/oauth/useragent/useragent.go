// Package useragent builds the client identity strings presented to OAuth
// provider endpoints. Endpoints license by client identity, so each provider
// UA must match the official client. Versions are detected live where
// possible (installed CLI binaries, published release manifests), persisted
// on success, and fall back to the last persisted value and then a
// known-good static version. Detection is cached per process.
package useragent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/example-git/crux/internal/providertransport"
	"golang.org/x/mod/semver"
)

// Static fallback versions, used when live detection fails and no persisted
// value exists. These should be bumped when the corresponding clients
// meaningfully change.
const (
	staticGeminiVersion = "1.0.9"

	// staticCopilotCLIVersion is the GitHub Copilot CLI release advertised
	// when live detection fails.
	staticCopilotCLIVersion = "1.0.32"

	// staticCopilotExtensionVersion is the VS Code Copilot Chat extension
	// version advertised in vscode mode.
	staticCopilotExtensionVersion = "0.45.2026041705"

	// copilotVSCodeEditorVersion is the editor identity presented in vscode
	// mode.
	copilotVSCodeEditorVersion = "vscode/1.117.0-insider"
)

// antigravityManifestBase serves per-platform release manifests for the
// Antigravity CLI, mirroring the official install.sh
// (https://antigravity.google/cli/install.sh). Each manifest is JSON with a
// "version" field.
const antigravityManifestBase = "https://antigravity-cli-auto-updater-974169037036.us-central1.run.app/manifests"

const (
	probeTimeout             = 3 * time.Second
	codexReleaseProbeTimeout = 10 * time.Second
)

// versionRe extracts the first semver-like token from tool output,
// matching the reference implementation (`/v?(\d+\.\d+[\w.-]*)/`).
var versionRe = regexp.MustCompile(`v?(\d+\.\d+[\w.-]*)`)

// fullVersionRe validates that an entire string is a version, used before
// persisting or trusting cached values.
var fullVersionRe = regexp.MustCompile(`^\d+\.\d+[\w.-]*$`)

var (
	geminiOnce    sync.Once
	geminiVersion string
)

// Gemini returns the Antigravity CLI user agent, e.g.
// "antigravity/cli/1.0.9 darwin/arm64".
func Gemini() string {
	return fmt.Sprintf("antigravity/cli/%s %s/%s", GeminiVersion(), runtime.GOOS, runtime.GOARCH)
}

func GeminiForContext(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := providertransport.ValidateContextOwner(ctx); err != nil {
		return "", err
	}
	if identity, ok := ctx.Value(geminiIdentityKey{}).(NativeIdentity); ok {
		return identity.UserAgent, nil
	}
	version, err := GeminiVersionForContext(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("antigravity/cli/%s %s/%s", version, runtime.GOOS, runtime.GOARCH), nil
}

// GeminiVersion resolves the Antigravity CLI version: env override, then the
// official release manifest, then a live probe of installed binaries, then
// the last persisted answer, then the static fallback.
func GeminiVersion() string {
	if v := os.Getenv("ANTIGRAVITY_CLI_VERSION"); v != "" {
		return v
	}
	geminiOnce.Do(func() {
		geminiVersion = resolve("gemini", staticGeminiVersion, func() string {
			if v := fetchAntigravityManifestVersion(); v != "" {
				return v
			}
			for _, tool := range []string{"antigravity", "agy", "gemini"} {
				if v := runToolVersion(tool); v != "" {
					return v
				}
			}
			return ""
		})
	})
	return geminiVersion
}

func GeminiVersionForContext(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := providertransport.ValidateContextOwner(ctx); err != nil {
		return "", err
	}
	if value := environmentOrForContext(ctx, "ANTIGRAVITY_CLI_VERSION", ""); value != "" {
		return value, nil
	}
	return resolveForContext(ctx, "gemini", staticGeminiVersion, func(ctx context.Context) string {
		if value := fetchAntigravityManifestVersionForContext(ctx); value != "" {
			return value
		}
		for _, tool := range []string{"antigravity", "agy", "gemini"} {
			if value := runToolVersionForContext(ctx, tool); value != "" {
				return value
			}
		}
		return ""
	})
}

// antigravityPlatform reproduces the platform detection in the official
// install.sh: "<os>_<arch>" with a "_musl" suffix on musl-based Linux.
func antigravityPlatform() string {
	os_ := runtime.GOOS
	arch := runtime.GOARCH
	if os_ != "darwin" && os_ != "linux" && os_ != "windows" {
		return ""
	}
	if arch != "amd64" && arch != "arm64" {
		return ""
	}
	if os_ == "linux" && isMuslLinux() {
		return fmt.Sprintf("linux_%s_musl", arch)
	}
	return fmt.Sprintf("%s_%s", os_, arch)
}

// isMuslLinux mirrors install.sh's musl libc detection.
func isMuslLinux() bool {
	for _, p := range []string{"/lib/libc.musl-x86_64.so.1", "/lib/libc.musl-aarch64.so.1"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	out, err := exec.CommandContext(context.Background(), "ldd", "/bin/ls").CombinedOutput()
	return err == nil && strings.Contains(string(out), "musl")
}

func antigravityPlatformForContext(ctx context.Context) string {
	if runtime.GOOS != "linux" {
		return antigravityPlatform()
	}
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		return ""
	}
	platform := "linux_" + runtime.GOARCH
	if isMuslLinuxForContext(ctx) {
		platform += "_musl"
	}
	return platform
}

func isMuslLinuxForContext(ctx context.Context) bool {
	for _, path := range []string{"/lib/libc.musl-x86_64.so.1", "/lib/libc.musl-aarch64.so.1"} {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	output, err := commandCombinedOutputForContext(ctx, "ldd", "/bin/ls")
	return err == nil && strings.Contains(string(output), "musl")
}

// fetchAntigravityManifestVersion queries the release manifest for the
// current platform and returns its "version" field. Returns "" on any
// failure.
func fetchAntigravityManifestVersion() string {
	return fetchAntigravityManifestVersionForContext(context.Background())
}

func fetchAntigravityManifestVersionForContext(ctx context.Context) string {
	platform := antigravityPlatformForContext(ctx)
	if platform == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, antigravityManifestBase+"/"+platform+".json", nil)
	if err != nil {
		return ""
	}
	resp, err := providertransport.ClientWithContextOwnerValidator(ctx, http.DefaultClient).Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return ""
	}
	var manifest struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return ""
	}
	return strings.TrimSpace(manifest.Version)
}

// resolve runs the live detector and persists a valid answer; otherwise it
// returns the last persisted valid answer, then the static fallback.
func resolve(key, fallback string, detect func() string) string {
	if value := detect(); fullVersionRe.MatchString(value) {
		persist(key, value)
		return value
	}
	if value := persisted(key); value != "" {
		return value
	}
	return fallback
}

func newestVersion(detected, cached string) string {
	if !fullVersionRe.MatchString(detected) || !semver.IsValid("v"+detected) {
		detected = ""
	}
	if !fullVersionRe.MatchString(cached) || !semver.IsValid("v"+cached) {
		cached = ""
	}
	if detected == "" || cached != "" && semver.Compare("v"+cached, "v"+detected) > 0 {
		return cached
	}
	return detected
}

func resolveNewest(key, fallback string, detect func() string) string {
	detected := detect()
	cached := persisted(key)
	selected := newestVersion(detected, cached)
	if selected == "" {
		return fallback
	}
	if selected == detected && selected != cached {
		persist(key, selected)
	}
	return selected
}

func resolveForContext(ctx context.Context, key, fallback string, detect func(context.Context) string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := providertransport.ValidateContextOwner(ctx); err != nil {
		return "", err
	}
	if value := detect(ctx); fullVersionRe.MatchString(value) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if err := providertransport.ValidateContextOwner(ctx); err != nil {
			return "", err
		}
		return value, nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := providertransport.ValidateContextOwner(ctx); err != nil {
		return "", err
	}
	value := persistedForContext(ctx, key)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := providertransport.ValidateContextOwner(ctx); err != nil {
		return "", err
	}
	if value != "" {
		return value, nil
	}
	return fallback, nil
}

func resolveNewestForContext(ctx context.Context, key, fallback string, detect func(context.Context) string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := providertransport.ValidateContextOwner(ctx); err != nil {
		return "", err
	}
	detected := detect(ctx)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := providertransport.ValidateContextOwner(ctx); err != nil {
		return "", err
	}
	cached := persistedForContext(ctx, key)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := providertransport.ValidateContextOwner(ctx); err != nil {
		return "", err
	}
	if selected := newestVersion(detected, cached); selected != "" {
		return selected, nil
	}
	return fallback, nil
}

// runToolVersion runs `tool --version` and parses the first semver-like
// token. Returns "" on any failure.
func runToolVersion(tool string) string {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, tool, "--version").Output()
	if err != nil {
		return ""
	}
	if m := versionRe.FindStringSubmatch(string(out)); m != nil {
		return m[1]
	}
	return ""
}

func runToolVersionForContext(ctx context.Context, tool string) string {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out, err := commandOutputForContext(ctx, tool, "--version")
	if err != nil {
		return ""
	}
	if match := versionRe.FindStringSubmatch(string(out)); match != nil {
		return match[1]
	}
	return ""
}

// ---------------------------------------------------------------------------
// Persistence
// ---------------------------------------------------------------------------

var persistMu sync.Mutex

// cachePath returns the on-disk location of the persisted version cache,
// alongside the other ai-cli config files.
func cachePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".ai-cli", "useragent-versions.json"), nil
}

// persisted returns the last stored valid version for key, or "".
func persisted(key string) string {
	persistMu.Lock()
	defer persistMu.Unlock()
	versions := readCache()
	v := versions[key]
	if !fullVersionRe.MatchString(v) {
		return ""
	}
	return v
}

// persist stores a valid version for key. Failures are silent: persistence
// is an optimization, never a requirement.
func persist(key, version string) {
	if !fullVersionRe.MatchString(version) {
		return
	}
	persistMu.Lock()
	defer persistMu.Unlock()
	versions := readCache()
	if versions[key] == version {
		return
	}
	versions[key] = version
	path, err := cachePath()
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	data, err := json.MarshalIndent(versions, "", "  ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// readCache loads the persisted version map; missing or corrupt files yield
// an empty map. Callers must hold persistMu.
func readCache() map[string]string {
	versions := map[string]string{}
	path, err := cachePath()
	if err != nil {
		return versions
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return versions
	}
	_ = json.Unmarshal(data, &versions)
	return versions
}

// ---------------------------------------------------------------------------
// Copilot
// ---------------------------------------------------------------------------

// CopilotMode selects which client identity the Copilot provider presents:
// the Copilot CLI or the VS Code Copilot Chat extension.
type CopilotMode string

const (
	CopilotModeCLI    CopilotMode = "copilot-cli"
	CopilotModeVSCode CopilotMode = "vscode"
)

// CopilotIdentity is the full set of identity values a Copilot request needs.
type CopilotIdentity struct {
	Mode          CopilotMode
	IntegrationID string
	UserAgent     string
	EditorVersion string
	// EditorPluginVersion is only set in vscode mode.
	EditorPluginVersion string
}

var (
	copilotOnce    sync.Once
	copilotVersion string

	copilotExtOnce    sync.Once
	copilotExtVersion string
)

// CopilotAdvertisementMode returns the configured identity mode. Defaults to
// vscode, matching Crux's existing behavior; set COPILOT_ADVERTISE_MODE to
// "copilot-cli" (or "cli") to present the Copilot CLI identity instead.
func CopilotAdvertisementMode() CopilotMode {
	switch strings.ToLower(os.Getenv("COPILOT_ADVERTISE_MODE")) {
	case "cli", "copilot-cli":
		return CopilotModeCLI
	case "vscode":
		return CopilotModeVSCode
	}
	return CopilotModeVSCode
}

// Copilot returns the identity for the configured advertisement mode.
func Copilot() CopilotIdentity {
	if CopilotAdvertisementMode() == CopilotModeVSCode {
		ext := CopilotExtensionVersion()
		ua := "GitHubCopilotChat/" + ext
		return CopilotIdentity{
			Mode:                CopilotModeVSCode,
			IntegrationID:       envOr("COPILOT_VSCODE_INTEGRATION_ID", "vscode-chat"),
			UserAgent:           ua,
			EditorVersion:       envOr("COPILOT_VSCODE_EDITOR_VERSION", copilotVSCodeEditorVersion),
			EditorPluginVersion: envOr("COPILOT_VSCODE_EDITOR_PLUGIN_VERSION", "copilot-chat/"+ext),
		}
	}
	term := os.Getenv("TERM_PROGRAM")
	if term == "" {
		term = os.Getenv("TERM")
	}
	if term == "" {
		term = "terminal"
	}
	return CopilotIdentity{
		Mode:          CopilotModeCLI,
		IntegrationID: "copilot-developer-cli",
		UserAgent: fmt.Sprintf("copilot/%s (%s v%s) term/%s",
			CopilotCLIVersion(), runtime.GOOS, osVersion(), term),
	}
}

func CopilotForContext(ctx context.Context) (CopilotIdentity, error) {
	if err := ctx.Err(); err != nil {
		return CopilotIdentity{}, err
	}
	if copilotAdvertisementModeForContext(ctx) == CopilotModeVSCode {
		extension, err := CopilotExtensionVersionForContext(ctx)
		if err != nil {
			return CopilotIdentity{}, err
		}
		return CopilotIdentity{
			Mode:                CopilotModeVSCode,
			IntegrationID:       environmentOrForContext(ctx, "COPILOT_VSCODE_INTEGRATION_ID", "vscode-chat"),
			UserAgent:           "GitHubCopilotChat/" + extension,
			EditorVersion:       environmentOrForContext(ctx, "COPILOT_VSCODE_EDITOR_VERSION", copilotVSCodeEditorVersion),
			EditorPluginVersion: environmentOrForContext(ctx, "COPILOT_VSCODE_EDITOR_PLUGIN_VERSION", "copilot-chat/"+extension),
		}, nil
	}
	version, err := CopilotCLIVersionForContext(ctx)
	if err != nil {
		return CopilotIdentity{}, err
	}
	osRelease, err := osVersionForContext(ctx)
	if err != nil {
		return CopilotIdentity{}, err
	}
	terminal := environmentOrForContext(ctx, "TERM_PROGRAM", "")
	if terminal == "" {
		terminal = environmentOrForContext(ctx, "TERM", "")
	}
	if terminal == "" {
		terminal = "terminal"
	}
	return CopilotIdentity{
		Mode:          CopilotModeCLI,
		IntegrationID: "copilot-developer-cli",
		UserAgent: fmt.Sprintf("copilot/%s (%s v%s) term/%s",
			version, runtime.GOOS, osRelease, terminal),
	}, nil
}

// CopilotCLIVersion resolves the Copilot CLI version: env override, then
// the GitHub release feed for the native CLI, then the versions.json GitHub
// Copilot config, then a live probe of installed binaries, then the last
// persisted answer, then the static fallback.
func CopilotCLIVersion() string {
	if v := os.Getenv("COPILOT_CLI_VERSION"); v != "" {
		return v
	}
	copilotOnce.Do(func() {
		copilotVersion = resolve("copilot-cli", staticCopilotCLIVersion, func() string {
			if v := fetchCopilotCLILatest(); v != "" {
				return v
			}
			if v := readCopilotVersionsJSON(); v != "" {
				return v
			}
			for _, tool := range []string{"github-copilot-cli", "copilot"} {
				if v := runToolVersion(tool); v != "" {
					return v
				}
			}
			return ""
		})
	})
	return copilotVersion
}

func CopilotCLIVersionForContext(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := providertransport.ValidateContextOwner(ctx); err != nil {
		return "", err
	}
	if value := environmentOrForContext(ctx, "COPILOT_CLI_VERSION", ""); value != "" {
		return value, nil
	}
	return resolveForContext(ctx, "copilot-cli", staticCopilotCLIVersion, func(ctx context.Context) string {
		if value := fetchCopilotCLILatestForContext(ctx); value != "" {
			return value
		}
		if value := readCopilotVersionsJSONForContext(ctx); value != "" {
			return value
		}
		for _, tool := range []string{"github-copilot-cli", "copilot"} {
			if value := runToolVersionForContext(ctx, tool); value != "" {
				return value
			}
		}
		return ""
	})
}

// CopilotExtensionVersion resolves the VS Code Copilot Chat extension
// version presented in vscode mode: env override, then the VS Code
// Marketplace, then versions.json, then the last persisted answer, then the
// static fallback.
func CopilotExtensionVersion() string {
	if v := os.Getenv("COPILOT_VSCODE_EXTENSION_VERSION"); v != "" {
		return v
	}
	copilotExtOnce.Do(func() {
		copilotExtVersion = resolve("copilot-extension", staticCopilotExtensionVersion, func() string {
			if v := fetchCopilotChatMarketplaceVersion(); v != "" {
				return v
			}
			return readCopilotVersionsJSON()
		})
	})
	return copilotExtVersion
}

func CopilotExtensionVersionForContext(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := providertransport.ValidateContextOwner(ctx); err != nil {
		return "", err
	}
	if value := environmentOrForContext(ctx, "COPILOT_VSCODE_EXTENSION_VERSION", ""); value != "" {
		return value, nil
	}
	return resolveForContext(ctx, "copilot-extension", staticCopilotExtensionVersion, func(ctx context.Context) string {
		if value := fetchCopilotChatMarketplaceVersionForContext(ctx); value != "" {
			return value
		}
		return readCopilotVersionsJSONForContext(ctx)
	})
}

// fetchCopilotCLILatest reads the latest native Copilot CLI release from the
// github/copilot-cli release feed. Returns "" on any failure.
func fetchCopilotCLILatest() string {
	return fetchCopilotCLILatestForContext(context.Background())
}

func fetchCopilotCLILatestForContext(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.github.com/repos/github/copilot-cli/releases/latest", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := providertransport.ClientWithContextOwnerValidator(ctx, http.DefaultClient).Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ""
	}
	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(data, &release); err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(release.TagName), "v")
}

// fetchCopilotChatMarketplaceVersion reads the latest GitHub.copilot-chat
// extension version from the VS Code Marketplace gallery API. Returns "" on
// any failure.
func fetchCopilotChatMarketplaceVersion() string {
	return fetchCopilotChatMarketplaceVersionForContext(context.Background())
}

func fetchCopilotChatMarketplaceVersionForContext(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	body := `{"filters":[{"criteria":[{"filterType":7,"value":"GitHub.copilot-chat"}]}],"flags":914}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://marketplace.visualstudio.com/_apis/public/gallery/extensionquery",
		strings.NewReader(body))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json;api-version=3.0-preview.1")
	resp, err := providertransport.ClientWithContextOwnerValidator(ctx, http.DefaultClient).Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ""
	}
	var out struct {
		Results []struct {
			Extensions []struct {
				Versions []struct {
					Version string `json:"version"`
				} `json:"versions"`
			} `json:"extensions"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return ""
	}
	if len(out.Results) == 0 || len(out.Results[0].Extensions) == 0 || len(out.Results[0].Extensions[0].Versions) == 0 {
		return ""
	}
	return strings.TrimSpace(out.Results[0].Extensions[0].Versions[0].Version)
}

// readCopilotVersionsJSON reads the version advertised by the GitHub Copilot
// config at ~/.config/github-copilot/versions.json. Returns "" on failure.
func readCopilotVersionsJSON() string {
	return readCopilotVersionsJSONForContext(context.Background())
}

func readCopilotVersionsJSONForContext(ctx context.Context) string {
	home, err := userHomeForContext(ctx)
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".config", "github-copilot", "versions.json"))
	if err != nil {
		return ""
	}
	var parsed struct {
		Version      string `json:"version"`
		BuildVersion string `json:"buildVersion"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return ""
	}
	if parsed.Version != "" {
		return parsed.Version
	}
	return parsed.BuildVersion
}

// osVersion returns the host OS product version on macOS ("14.0" fallback),
// matching the reference CLI identity.
func osVersion() string {
	if runtime.GOOS != "darwin" {
		return "14.0"
	}
	out, err := exec.CommandContext(context.Background(), "sw_vers", "-productVersion").Output()
	if err != nil {
		return "14.0"
	}
	if v := strings.TrimSpace(string(out)); v != "" {
		return v
	}
	return "14.0"
}

func osVersionForContext(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := providertransport.ValidateContextOwner(ctx); err != nil {
		return "", err
	}
	if runtime.GOOS != "darwin" {
		return "14.0", nil
	}
	output, err := commandOutputForContext(ctx, "sw_vers", "-productVersion")
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := providertransport.ValidateContextOwner(ctx); err != nil {
		return "", err
	}
	if err != nil {
		return "14.0", nil
	}
	if value := strings.TrimSpace(string(output)); value != "" {
		return value, nil
	}
	return "14.0", nil
}

// envOr returns the environment value for key, or fallback when unset.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// CopilotGitHubUserAgent returns the User-Agent presented to github.com
// OAuth endpoints: the extension UA in vscode mode, the CLI integration id
// in cli mode, matching the reference implementation.
func CopilotGitHubUserAgent() string {
	id := Copilot()
	if id.Mode == CopilotModeVSCode {
		return id.UserAgent
	}
	return id.IntegrationID
}

func CopilotGitHubUserAgentForContext(ctx context.Context) (string, error) {
	identity, err := CopilotForContext(ctx)
	if err != nil {
		return "", err
	}
	if identity.Mode == CopilotModeVSCode {
		return identity.UserAgent, nil
	}
	return identity.IntegrationID, nil
}

// ---------------------------------------------------------------------------
// Codex
// ---------------------------------------------------------------------------

// staticCodexVersion is the Codex CLI release advertised when live detection
// fails and no persisted value exists.
const staticCodexVersion = "0.146.0"

// codexOSType mirrors the os_info crate's os_type() display names used by
// the Codex CLI UA.
func codexOSType() string {
	switch runtime.GOOS {
	case "darwin":
		return "Mac OS"
	case "linux":
		return "Linux"
	case "windows":
		return "Windows"
	}
	return runtime.GOOS
}

// codexTerminalToken reproduces codex-rs terminal-detection's
// user_agent_token(): TERM_PROGRAM (with /TERM_PROGRAM_VERSION when set),
// else TERM, else "unknown".
func codexTerminalToken() string {
	if program := os.Getenv("TERM_PROGRAM"); program != "" {
		if version := os.Getenv("TERM_PROGRAM_VERSION"); version != "" {
			return program + "/" + version
		}
		return program
	}
	if term := os.Getenv("TERM"); term != "" {
		return term
	}
	return "unknown"
}

// Codex returns the Codex CLI user agent, matching
// codex-rs/login get_codex_user_agent():
// "codex_cli_rs/<version> (<os> <ver>; <arch>) <terminal>", e.g.
// "codex_cli_rs/0.148.0 (Mac OS 26.5; arm64) iTerm.app/3.7.0".
func Codex() string {
	return fmt.Sprintf("%s/%s (%s %s; %s) %s",
		CodexOriginator(), CodexVersion(), codexOSType(), osVersion(),
		runtime.GOARCH, codexTerminalToken())
}

func CodexForContext(ctx context.Context) (string, error) {
	identity, err := ResolveCodexIdentity(ctx)
	return identity.UserAgent, err
}

// CodexOriginator returns the originator value presented in the UA and the
// "originator" header, honoring the same override env var as the Codex CLI.
func CodexOriginator() string {
	return envOr("CODEX_INTERNAL_ORIGINATOR_OVERRIDE", "codex_cli_rs")
}

// CodexOriginatorForContext matches the originator used by CodexForContext,
// including captured absence, so a request's header and User-Agent agree.
func CodexOriginatorForContext(ctx context.Context) string {
	if identity, ok := ctx.Value(codexIdentityKey{}).(NativeIdentity); ok {
		return identity.Originator
	}
	return environmentOrForContext(ctx, "CODEX_INTERNAL_ORIGINATOR_OVERRIDE", "codex_cli_rs")
}

func codexTerminalTokenForContext(ctx context.Context) string {
	if program := environmentOrForContext(ctx, "TERM_PROGRAM", ""); program != "" {
		if version := environmentOrForContext(ctx, "TERM_PROGRAM_VERSION", ""); version != "" {
			return program + "/" + version
		}
		return program
	}
	return environmentOrForContext(ctx, "TERM", "unknown")
}

var (
	codexOnce    sync.Once
	codexVersion string
)

// CodexVersion resolves the Codex CLI version: env override, then the latest
// stable tag on the openai/codex release feed, then a live probe of the
// installed binary, then the last persisted answer, then the static fallback.
func CodexVersion() string {
	if v := os.Getenv("CODEX_VERSION"); v != "" {
		return v
	}
	codexOnce.Do(func() {
		codexVersion = resolveNewest("codex", staticCodexVersion, func() string {
			if v := fetchCodexLatest(); v != "" {
				return v
			}
			return runToolVersion("codex")
		})
	})
	return codexVersion
}

func CodexVersionForContext(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := providertransport.ValidateContextOwner(ctx); err != nil {
		return "", err
	}
	if identity, ok := ctx.Value(codexIdentityKey{}).(NativeIdentity); ok {
		return identity.Version, nil
	}
	if value := environmentOrForContext(ctx, "CODEX_VERSION", ""); value != "" {
		return value, nil
	}
	return resolveNewestForContext(ctx, "codex", staticCodexVersion, func(ctx context.Context) string {
		if value := fetchCodexLatestForContext(ctx); value != "" {
			return value
		}
		return runToolVersionForContext(ctx, "codex")
	})
}

// fetchCodexLatest reads the newest stable tag from the openai/codex latest
// release redirect (tags look like "rust-v0.148.0"). Returns "" on any failure.
func fetchCodexLatest() string {
	return fetchCodexLatestForContext(context.Background())
}

func fetchCodexLatestForContext(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, codexReleaseProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead,
		"https://github.com/openai/codex/releases/latest", nil)
	if err != nil {
		return ""
	}
	baseClient := providertransport.ClientWithContextOwnerValidator(ctx, http.DefaultClient)
	client := *baseClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusMultipleChoices || resp.StatusCode >= http.StatusBadRequest {
		return ""
	}
	target, err := req.URL.Parse(resp.Header.Get("Location"))
	if err != nil || target.Scheme != "https" || target.Hostname() != "github.com" {
		return ""
	}
	const tagPrefix = "/openai/codex/releases/tag/rust-v"
	version, found := strings.CutPrefix(target.Path, tagPrefix)
	if !found || !fullVersionRe.MatchString(version) {
		return ""
	}
	return version
}
