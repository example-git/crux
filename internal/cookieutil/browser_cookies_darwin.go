//go:build darwin

package cookieutil

import (
	"context"
	"crypto/sha1"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/pbkdf2"
)

func platformBrowserProfilesFromEnvironment(getenv func(string) string) []browserProfile {
	home := getenv("HOME")
	if !filepath.IsAbs(home) {
		return nil
	}
	applicationSupport := filepath.Join(home, "Library", "Application Support")
	browsers := []chromiumBrowser{
		{root: filepath.Join(applicationSupport, "Google", "Chrome"), keyService: "Chrome Safe Storage", keyAccount: "Chrome"},
		{root: filepath.Join(applicationSupport, "Google", "Chrome Beta"), keyService: "Chrome Safe Storage", keyAccount: "Chrome"},
		{root: filepath.Join(applicationSupport, "Google", "Chrome Dev"), keyService: "Chrome Safe Storage", keyAccount: "Chrome"},
		{root: filepath.Join(applicationSupport, "Chromium"), keyService: "Chromium Safe Storage", keyAccount: "Chromium"},
		{root: filepath.Join(applicationSupport, "BraveSoftware", "Brave-Browser"), keyService: "Brave Safe Storage", keyAccount: "Brave"},
		{root: filepath.Join(applicationSupport, "Microsoft Edge"), keyService: "Microsoft Edge Safe Storage", keyAccount: "Microsoft Edge"},
		{root: filepath.Join(applicationSupport, "Vivaldi"), keyService: "Vivaldi Safe Storage", keyAccount: "Vivaldi"},
		{root: filepath.Join(applicationSupport, "Arc", "User Data"), keyService: "Arc Safe Storage", keyAccount: "Arc"},
	}
	return discoverBrowserProfiles(browsers, []string{filepath.Join(applicationSupport, "Firefox", "Profiles")})
}

func chromiumCookieDecryptor(ctx context.Context, profile browserProfile) (func([]byte) ([]byte, error), error) {
	if profile.keyService == "" || profile.keyAccount == "" {
		return nil, errors.New("browser credential identity is unavailable")
	}
	output, err := exec.CommandContext(ctx, "/usr/bin/security", "-q", "find-generic-password", "-w", "-a", profile.keyAccount, "-s", profile.keyService).Output()
	if err != nil {
		return nil, errors.New("browser cookie key is unavailable")
	}
	password := strings.TrimSpace(string(output))
	if password == "" {
		return nil, errors.New("browser cookie key is empty")
	}
	key := pbkdf2.Key([]byte(password), []byte("saltysalt"), 1003, 16, sha1.New)
	return browserCookieDecryptorFromKey(key), nil
}
