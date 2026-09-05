//go:build embedded_mitmproxy && !(darwin && arm64) && !(linux && amd64) && !(linux && arm64)

package trafficcapture

var embeddedRuntimeArchive []byte

const (
	embeddedRuntimeTarget  = ""
	embeddedRuntimeLibrary = ""
)
