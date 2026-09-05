//go:build embedded_mitmproxy && linux && arm64

package trafficcapture

import _ "embed"

//go:embed assets/mitmproxy-runtime-linux-arm64.tar.gz
var embeddedRuntimeArchive []byte

const (
	embeddedRuntimeTarget  = "linux-arm64"
	embeddedRuntimeLibrary = "lib/libpython3.12.so.1.0"
)
