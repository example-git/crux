//go:build embedded_mitmproxy && darwin && arm64

package trafficcapture

import _ "embed"

//go:embed assets/mitmproxy-runtime-darwin-arm64.tar.gz
var embeddedRuntimeArchive []byte

const (
	embeddedRuntimeTarget  = "darwin-arm64"
	embeddedRuntimeLibrary = "lib/libpython3.12.dylib"
)
