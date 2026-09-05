package clientidentity

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providertransport"
)

var cacheMu sync.Mutex

var isolatedVersions = struct {
	sync.Mutex
	values map[string]string
}{values: map[string]string{}}

func ResolveWithEnvironment(ctx context.Context, identity *manifest.ResolvedClientIdentity, environment []string) (string, string, error) {
	if identity == nil {
		return "", "", nil
	}
	if err := providertransport.ValidateContextOwner(ctx); err != nil {
		return "", "", err
	}
	pattern, err := regexp.Compile(identity.VersionPattern)
	if err != nil {
		return "", "", err
	}
	version := ""
	for _, entry := range environment {
		key, candidate, ok := strings.Cut(entry, "=")
		if ok && key == identity.Environment && pattern.MatchString(candidate) {
			version = candidate
		}
	}
	if version != "" {
		if err := ctx.Err(); err != nil {
			return "", "", err
		}
		return version, expand(identity.UserAgentFormat, version), nil
	}
	keyBytes, err := json.Marshal(identity)
	if err != nil {
		return "", "", err
	}
	key := string(keyBytes)
	if version == "" {
		version = fetchVersion(ctx, identity, pattern)
	}
	if err := providertransport.ValidateContextOwner(ctx); err != nil {
		return "", "", err
	}
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	isolatedVersions.Lock()
	defer isolatedVersions.Unlock()
	if version == "" {
		version = isolatedVersions.values[key]
	}
	if version == "" {
		version = identity.FallbackVersion
	}
	if !pattern.MatchString(version) {
		return "", "", fmt.Errorf("resolved client version is invalid")
	}
	if len(isolatedVersions.values) >= 128 {
		isolatedVersions.values = map[string]string{}
	}
	isolatedVersions.values[key] = version
	return version, expand(identity.UserAgentFormat, version), nil
}

func Resolve(identity *manifest.ResolvedClientIdentity) (string, string, error) {
	return resolve(context.Background(), identity, true)
}

func ResolveForContext(ctx context.Context, identity *manifest.ResolvedClientIdentity) (string, string, error) {
	return resolve(ctx, identity, providertransport.OwnerValidatorFromContext(ctx) == nil)
}

func resolve(ctx context.Context, identity *manifest.ResolvedClientIdentity, persistResolved bool) (string, string, error) {
	if identity == nil {
		return "", "", nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := providertransport.ValidateContextOwner(ctx); err != nil {
		return "", "", err
	}
	pattern, err := regexp.Compile(identity.VersionPattern)
	if err != nil {
		return "", "", err
	}
	version := ""
	if candidate := strings.TrimSpace(os.Getenv(identity.Environment)); pattern.MatchString(candidate) {
		version = candidate
	}
	if version == "" {
		version = fetchVersion(ctx, identity, pattern)
	}
	if err := providertransport.ValidateContextOwner(ctx); err != nil {
		return "", "", err
	}
	if version == "" {
		version = cachedVersion(identity.CacheKey, pattern)
	}
	if version == "" {
		version = identity.FallbackVersion
	}
	if !pattern.MatchString(version) {
		return "", "", fmt.Errorf("resolved client version is invalid")
	}
	if err := providertransport.ValidateContextOwner(ctx); err != nil {
		return "", "", err
	}
	if persistResolved {
		persistVersion(identity.CacheKey, version)
	}
	return version, expand(identity.UserAgentFormat, version), nil
}

func fetchVersion(ctx context.Context, identity *manifest.ResolvedClientIdentity, pattern *regexp.Regexp) string {
	if identity.LatestURL == "" {
		return ""
	}
	client := &http.Client{
		Timeout: time.Duration(identity.ProbeTimeoutMS) * time.Millisecond,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, expand(identity.LatestURL, ""), nil)
	if err != nil {
		return ""
	}
	response, err := providertransport.ClientWithContextOwnerValidator(ctx, client).Do(request)
	if err != nil {
		return ""
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ""
	}
	data, err := readBounded(response.Body, identity.ProbeMaxBytes)
	if err != nil {
		return ""
	}
	candidate := strings.TrimSpace(string(data))
	if identity.VersionPointer != "" {
		var document any
		if json.Unmarshal(data, &document) != nil {
			return ""
		}
		candidate, _ = providertransport.JSONPointer(document, identity.VersionPointer).(string)
		candidate = strings.TrimSpace(candidate)
	}
	if pattern.MatchString(candidate) {
		return candidate
	}
	return ""
}

func expand(value, version string) string {
	return strings.NewReplacer(
		"{version}", version,
		"{os}", runtime.GOOS,
		"{arch}", runtime.GOARCH,
	).Replace(value)
}

func cachePath() string {
	if directory := strings.TrimSpace(os.Getenv("AI_CLI_DIR")); directory != "" {
		return filepath.Join(directory, "provider-client-versions.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".ai-cli", "provider-client-versions.json")
}

func cachedVersion(key string, pattern *regexp.Regexp) string {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	values := readCache()
	if pattern.MatchString(values[key]) {
		return values[key]
	}
	return ""
}

func persistVersion(key, value string) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	values := readCache()
	values[key] = value
	path := cachePath()
	if path == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	data, _ := json.Marshal(values)
	temporary := path + ".tmp"
	if os.WriteFile(temporary, data, 0o600) == nil {
		_ = os.Rename(temporary, path)
	}
}

func readCache() map[string]string {
	result := map[string]string{}
	path := cachePath()
	if path == "" {
		return result
	}
	data, err := os.ReadFile(path)
	if err == nil {
		_ = json.Unmarshal(data, &result)
	}
	return result
}

func readBounded(reader io.Reader, maximum int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, fmt.Errorf("client identity response exceeds %d bytes", maximum)
	}
	return data, nil
}
