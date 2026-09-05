//go:build windows

package cookieutil

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

func platformBrowserProfilesFromEnvironment(getenv func(string) string) []browserProfile {
	localAppData := getenv("LOCALAPPDATA")
	appData := getenv("APPDATA")
	if localAppData == "" && appData == "" {
		return nil
	}
	browsers := []chromiumBrowser{
		windowsChromiumBrowser(localAppData, "Google", "Chrome", "User Data"),
		windowsChromiumBrowser(localAppData, "Google", "Chrome Beta", "User Data"),
		windowsChromiumBrowser(localAppData, "Google", "Chrome Dev", "User Data"),
		windowsChromiumBrowser(localAppData, "Chromium", "User Data"),
		windowsChromiumBrowser(localAppData, "BraveSoftware", "Brave-Browser", "User Data"),
		windowsChromiumBrowser(localAppData, "Microsoft", "Edge", "User Data"),
		windowsChromiumBrowser(localAppData, "Vivaldi", "User Data"),
	}
	return discoverBrowserProfiles(browsers, []string{filepath.Join(appData, "Mozilla", "Firefox", "Profiles")})
}

func windowsChromiumBrowser(root string, parts ...string) chromiumBrowser {
	path := filepath.Join(append([]string{root}, parts...)...)
	return chromiumBrowser{root: path, localStatePath: filepath.Join(path, "Local State")}
}

func chromiumCookieDecryptor(_ context.Context, profile browserProfile) (func([]byte) ([]byte, error), error) {
	data, err := os.ReadFile(profile.localStatePath)
	if err != nil {
		return nil, errors.New("browser encryption state is unavailable")
	}
	var state struct {
		OSCrypt struct {
			EncryptedKey string `json:"encrypted_key"`
		} `json:"os_crypt"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, errors.New("browser encryption state is invalid")
	}
	encryptedKey, err := base64.StdEncoding.DecodeString(state.OSCrypt.EncryptedKey)
	if err != nil || len(encryptedKey) <= len("DPAPI") || string(encryptedKey[:len("DPAPI")]) != "DPAPI" {
		return nil, errors.New("browser encryption key is invalid")
	}
	key, err := decryptDPAPI(encryptedKey[len("DPAPI"):])
	if err != nil {
		return nil, errors.New("browser encryption key is unavailable")
	}
	return func(encrypted []byte) ([]byte, error) {
		if len(encrypted) >= 3 && (string(encrypted[:3]) == "v10" || string(encrypted[:3]) == "v11") {
			if len(encrypted) < 3+12+16 {
				return nil, errors.New("invalid browser cookie ciphertext")
			}
			block, blockErr := aes.NewCipher(key)
			if blockErr != nil {
				return nil, blockErr
			}
			gcm, gcmErr := cipher.NewGCM(block)
			if gcmErr != nil {
				return nil, gcmErr
			}
			return gcm.Open(nil, encrypted[3:15], encrypted[15:], nil)
		}
		return decryptDPAPI(encrypted)
	}, nil
}

func decryptDPAPI(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, errors.New("empty DPAPI input")
	}
	input := windows.DataBlob{Size: uint32(len(data)), Data: &data[0]}
	var output windows.DataBlob
	if err := windows.CryptUnprotectData(&input, nil, nil, 0, nil, 0, &output); err != nil {
		return nil, err
	}
	if output.Data == nil || output.Size == 0 {
		return nil, errors.New("empty DPAPI output")
	}
	decrypted := append([]byte(nil), unsafe.Slice(output.Data, int(output.Size))...)
	_, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(output.Data)))
	return decrypted, nil
}
