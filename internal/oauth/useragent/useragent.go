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

const probeTimeout = 3 * time.Second

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

// fetchAntigravityManifestVersion queries the release manifest for the
// current platform and returns its "version" field. Returns "" on any
// failure.
func fetchAntigravityManifestVersion() string {
	platform := antigravityPlatform()
	if platform == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, antigravityManifestBase+"/"+platform+".json", nil)
	if err != nil {
		return ""
	}
	resp, err := http.DefaultClient.Do(req)
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
	if v := detect(); fullVersionRe.MatchString(v) {
		persist(key, v)
		return v
	}
	if v := persisted(key); v != "" {
		return v
	}
	return fallback
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

// fetchCopilotCLILatest reads the latest native Copilot CLI release from the
// github/copilot-cli release feed. Returns "" on any failure.
func fetchCopilotCLILatest() string {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.github.com/repos/github/copilot-cli/releases/latest", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
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
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
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
	resp, err := http.DefaultClient.Do(req)
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
	home, err := os.UserHomeDir()
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

// CodexOriginator returns the originator value presented in the UA and the
// "originator" header, honoring the same override env var as the Codex CLI.
func CodexOriginator() string {
	return envOr("CODEX_INTERNAL_ORIGINATOR_OVERRIDE", "codex_cli_rs")
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
		codexVersion = resolve("codex", staticCodexVersion, func() string {
			if v := fetchCodexLatest(); v != "" {
				return v
			}
			return runToolVersion("codex")
		})
	})
	return codexVersion
}

// fetchCodexLatest reads the newest non-prerelease tag from the openai/codex
// release feed (tags look like "rust-v0.148.0"). Returns "" on any failure.
func fetchCodexLatest() string {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.github.com/repos/openai/codex/releases?per_page=10", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return ""
	}
	var releases []struct {
		TagName    string `json:"tag_name"`
		Prerelease bool   `json:"prerelease"`
		Draft      bool   `json:"draft"`
	}
	if err := json.Unmarshal(data, &releases); err != nil {
		return ""
	}
	for _, r := range releases {
		if r.Prerelease || r.Draft {
			continue
		}
		return strings.TrimPrefix(strings.TrimSpace(r.TagName), "rust-v")
	}
	return ""
}
