//go:build embedded_mitmproxy && linux && amd64

package trafficcapture

import _ "embed"

//go:embed assets/mitmproxy-runtime-linux-amd64.tar.gz
var embeddedRuntimeArchive []byte

const (
	embeddedRuntimeTarget  = "linux-amd64"
	embeddedRuntimeLibrary = "lib/libpython3.12.so.1.0"
)
