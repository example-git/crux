//go:build !unix

package config

import "os"

func openProviderContextFile(path string) (*os.File, error) {
	return os.Open(path)
}
