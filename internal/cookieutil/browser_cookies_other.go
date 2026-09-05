//go:build !darwin && !linux && !windows

package cookieutil

import (
	"context"
	"errors"
)

func platformBrowserProfilesFromEnvironment(func(string) string) []browserProfile {
	return nil
}

func chromiumCookieDecryptor(context.Context, browserProfile) (func([]byte) ([]byte, error), error) {
	return nil, errors.New("browser cookie import is unsupported on this platform")
}
