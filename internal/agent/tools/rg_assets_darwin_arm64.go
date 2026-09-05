//go:build darwin && arm64

package tools

import _ "embed"

const (
	embeddedRipgrepOS   = "darwin"
	embeddedRipgrepArch = "arm64"
)

//go:embed ripgrep/rg-darwin-arm64
var embeddedRipgrepBinary []byte
