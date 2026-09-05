//go:build (!darwin || !arm64) && (!linux || !amd64) && (!linux || !arm64)

package tools

const (
	embeddedRipgrepOS   = ""
	embeddedRipgrepArch = ""
)

var embeddedRipgrepBinary []byte
