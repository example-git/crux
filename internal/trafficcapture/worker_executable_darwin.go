package trafficcapture

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func bindWorkerExecutable(config *workerConfig, executable *os.File) error {
	path := filepath.Join(config.RuntimePath, "target-executable")
	copyFile, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return fmt.Errorf("create private target executable: %w", err)
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.Remove(path)
		}
	}()
	if _, err := executable.Seek(0, io.SeekStart); err != nil {
		_ = copyFile.Close()
		return fmt.Errorf("rewind approved target executable: %w", err)
	}
	if _, err := io.Copy(copyFile, executable); err != nil {
		_ = copyFile.Close()
		return fmt.Errorf("copy approved target executable into private runtime: %w", err)
	}
	if err := copyFile.Sync(); err != nil {
		_ = copyFile.Close()
		return fmt.Errorf("sync private target executable: %w", err)
	}
	if err := copyFile.Close(); err != nil {
		return fmt.Errorf("close private target executable: %w", err)
	}
	complete = true
	config.Command = append([]string{}, config.Command...)
	config.Command[0] = path
	config.PassFDs = nil
	return nil
}
