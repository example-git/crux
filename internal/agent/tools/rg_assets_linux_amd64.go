//go:build linux && amd64

package tools

import _ "embed"

const (
	embeddedRipgrepOS   = "linux"
	embeddedRipgrepArch = "amd64"
)

//go:embed ripgrep/rg-linux-amd64
var embeddedRipgrepBinary []byte
