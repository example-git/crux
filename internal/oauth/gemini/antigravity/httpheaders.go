package antigravity

import (
	"fmt"
	"strings"
)

// The helpers below mirror the integrated foundation HTTP header behavior,
// which cannot be imported from outside the fantasy module. They are inlined
// here so this provider stands alone.

// defaultUserAgent returns the default User-Agent string for the SDK.
func defaultUserAgent(version string) string {
	return fmt.Sprintf("Crux-Foundation/%s", version)
}

// resolveHeaders returns a new header map with a User-Agent field.
//
// An explicit user agent takes precedence, followed by one supplied through
// the header map, falling back to the default. The input map is never
// mutated.
func resolveHeaders(headers map[string]string, explicitUA, defaultUA string) map[string]string {
	out := make(map[string]string, len(headers)+1)
	var uaKeys []string

	for k, v := range headers {
		out[k] = v
		if strings.EqualFold(k, "User-Agent") {
			uaKeys = append(uaKeys, k)
		}
	}

	switch {
	case explicitUA != "":
		for _, k := range uaKeys {
			delete(out, k)
		}
		out["User-Agent"] = explicitUA
	case len(uaKeys) > 0:
		val := out[uaKeys[0]]
		for _, k := range uaKeys {
			delete(out, k)
		}
		out["User-Agent"] = val
	default:
		out["User-Agent"] = defaultUA
	}

	return out
}
