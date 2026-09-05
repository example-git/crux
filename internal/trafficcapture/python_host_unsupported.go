//go:build !darwin && !linux

package trafficcapture

func runEmbeddedPython(string) error {
	return embeddedRuntimeUnavailableError()
}
