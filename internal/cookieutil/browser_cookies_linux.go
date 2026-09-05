//go:build linux

package cookieutil

import (
	"context"
	"crypto/sha1"
	"errors"
	"path/filepath"
	"time"

	"github.com/godbus/dbus/v5"
	"golang.org/x/crypto/pbkdf2"
)

func platformBrowserProfilesFromEnvironment(getenv func(string) string) []browserProfile {
	home := getenv("HOME")
	if !filepath.IsAbs(home) {
		return nil
	}
	config := filepath.Join(home, ".config")
	browsers := []chromiumBrowser{
		{root: filepath.Join(config, "google-chrome"), keyApplication: "chrome"},
		{root: filepath.Join(config, "google-chrome-beta"), keyApplication: "chrome"},
		{root: filepath.Join(config, "google-chrome-unstable"), keyApplication: "chrome"},
		{root: filepath.Join(config, "chromium"), keyApplication: "chromium"},
		{root: filepath.Join(config, "BraveSoftware", "Brave-Browser"), keyApplication: "brave"},
		{root: filepath.Join(config, "microsoft-edge"), keyApplication: "microsoft-edge"},
		{root: filepath.Join(config, "vivaldi"), keyApplication: "vivaldi"},
	}
	return discoverBrowserProfiles(browsers, []string{filepath.Join(home, ".mozilla", "firefox")})
}

func chromiumCookieDecryptor(ctx context.Context, profile browserProfile) (func([]byte) ([]byte, error), error) {
	v10Key := pbkdf2.Key([]byte("peanuts"), []byte("saltysalt"), 1, 16, sha1.New)
	var v11Key []byte
	if profile.keyApplication != "" {
		if password, err := linuxSecretServicePassword(ctx, profile.keyApplication); err == nil {
			v11Key = pbkdf2.Key(password, []byte("saltysalt"), 1, 16, sha1.New)
		}
	}
	return func(encrypted []byte) ([]byte, error) {
		if len(encrypted) < 3 {
			return nil, errors.New("unsupported browser cookie encryption")
		}
		switch string(encrypted[:3]) {
		case "v10":
			return decryptChromiumCBC(encrypted, v10Key)
		case "v11":
			if len(v11Key) == 0 {
				return nil, errors.New("browser cookie key is unavailable")
			}
			return decryptChromiumCBC(encrypted, v11Key)
		default:
			return nil, errors.New("unsupported browser cookie encryption")
		}
	}, nil
}

type secretServiceValue struct {
	Session     dbus.ObjectPath
	Parameters  []byte
	Value       []byte
	ContentType string
}

func linuxSecretServicePassword(ctx context.Context, application string) ([]byte, error) {
	connection, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, errors.New("browser secret service is unavailable")
	}
	defer connection.Close()
	service := connection.Object("org.freedesktop.secrets", dbus.ObjectPath("/org/freedesktop/secrets"))
	var output dbus.Variant
	var session dbus.ObjectPath
	if err := service.CallWithContext(ctx, "org.freedesktop.Secret.Service.OpenSession", 0, "plain", dbus.MakeVariant("")).Store(&output, &session); err != nil {
		return nil, errors.New("open browser secret service session failed")
	}
	defer serviceSessionClose(connection, session)
	var unlocked []dbus.ObjectPath
	var locked []dbus.ObjectPath
	if err := service.CallWithContext(ctx, "org.freedesktop.Secret.Service.SearchItems", 0, map[string]string{"application": application}).Store(&unlocked, &locked); err != nil {
		return nil, errors.New("search browser secret service failed")
	}
	if len(unlocked) == 0 {
		if len(locked) > 0 {
			return nil, errors.New("browser secret service item is locked")
		}
		return nil, errors.New("browser secret service item was not found")
	}
	for _, path := range unlocked {
		var secret secretServiceValue
		if err := connection.Object("org.freedesktop.secrets", path).CallWithContext(ctx, "org.freedesktop.Secret.Item.GetSecret", 0, session).Store(&secret); err != nil {
			continue
		}
		if len(secret.Value) > 0 {
			return append([]byte(nil), secret.Value...), nil
		}
	}
	return nil, errors.New("browser secret service returned no password")
}

func serviceSessionClose(connection *dbus.Conn, session dbus.ObjectPath) {
	if session.IsValid() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = connection.Object("org.freedesktop.secrets", session).CallWithContext(ctx, "org.freedesktop.Secret.Session.Close", 0).Err
	}
}
