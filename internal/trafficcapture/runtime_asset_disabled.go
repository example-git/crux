//go:build !embedded_mitmproxy

package trafficcapture

var embeddedRuntimeArchive []byte

const (
	embeddedRuntimeTarget  = ""
	embeddedRuntimeLibrary = ""
)
