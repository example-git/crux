//go:build linux && arm64

package tools

import _ "embed"

const (
	embeddedRipgrepOS   = "linux"
	embeddedRipgrepArch = "arm64"
)

//go:embed ripgrep/rg-linux-arm64
var embeddedRipgrepBinary []byte
