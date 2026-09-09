//go:build !unix

package connection

import "os"

func openClientAuthorization(path string) (*os.File, error) {
	return os.Open(path)
}
