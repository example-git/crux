//go:build !unix

package webclient

import "os"

func openAttachment(root *os.Root, path string) (*os.File, error) {
	return root.Open(path)
}
