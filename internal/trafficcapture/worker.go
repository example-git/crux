package trafficcapture

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

//go:embed worker.py
var embeddedWorker string

const paneLogLimit = 1024 * 1024

type workerSignalConfig struct {
	StopPath string `json:"stop_path"`
}

func RunWorker(configPath string) error {
	if !EmbeddedRuntimeAvailable() {
		return embeddedRuntimeUnavailableError()
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read traffic capture worker config: %w", err)
	}
	var config workerConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("decode traffic capture worker config: %w", err)
	}
	if config.StopPath == "" {
		return fmt.Errorf("traffic capture worker config is missing stop_path")
	}
	if len(config.Command) == 0 {
		return fmt.Errorf("traffic capture worker config is missing command")
	}

	output, err := openVerifiedCapturePath(config.Output, config.OutputIdentity, os.O_RDWR)
	if err != nil {
		return fmt.Errorf("open approved capture output: %w", err)
	}
	defer output.Close()
	executable, err := openVerifiedCapturePath(config.Command[0], config.ExecutableIdentity, os.O_RDONLY)
	if err != nil {
		return fmt.Errorf("open approved target executable: %w", err)
	}
	defer executable.Close()
	workingDirectory, err := openVerifiedCapturePath(config.WorkingDir, config.WorkingDirIdentity, os.O_RDONLY)
	if err != nil {
		return fmt.Errorf("open approved target working directory: %w", err)
	}
	defer workingDirectory.Close()

	if err := workingDirectory.Chdir(); err != nil {
		return fmt.Errorf("enter approved target working directory: %w", err)
	}
	config.Output = fmt.Sprintf("/dev/fd/%d", output.Fd())
	if err := bindWorkerExecutable(&config, executable); err != nil {
		return err
	}
	config.WorkingDir = ""
	if err := writePrivateJSON(configPath, config); err != nil {
		return fmt.Errorf("bind traffic capture worker config: %w", err)
	}
	if err := os.Setenv("CRUX_TRAFFIC_CAPTURE_CONFIG", configPath); err != nil {
		return fmt.Errorf("set traffic capture worker config: %w", err)
	}
	defer os.Unsetenv("CRUX_TRAFFIC_CAPTURE_CONFIG")
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-signals:
			_ = os.WriteFile(config.StopPath, nil, 0o600)
		case <-done:
		}
	}()
	return runEmbeddedPython(embeddedWorker)
}

func openVerifiedCapturePath(path string, expected pathIdentity, flag int) (*os.File, error) {
	file, err := os.OpenFile(path, flag, 0)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	actual, err := identityFromFileInfo(info)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if actual != expected {
		_ = file.Close()
		return nil, fmt.Errorf("filesystem object changed after approval: %s", path)
	}
	return file, nil
}

func WritePaneLog(path string, input io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create traffic capture pane log directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open traffic capture pane log: %w", err)
	}
	defer file.Close()
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("seek traffic capture pane log: %w", err)
	}
	buffer := make([]byte, 32*1024)
	var size int64
	if info, statErr := file.Stat(); statErr == nil {
		size = info.Size()
	}
	for {
		count, readErr := input.Read(buffer)
		if count > 0 {
			if _, err := file.Write(buffer[:count]); err != nil {
				return fmt.Errorf("write traffic capture pane log: %w", err)
			}
			size += int64(count)
			if size > 2*paneLogLimit {
				if err := compactPaneLog(file); err != nil {
					return err
				}
				size = paneLogLimit
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return nil
			}
			return fmt.Errorf("read traffic capture pane output: %w", readErr)
		}
	}
}

func compactPaneLog(file *os.File) error {
	if _, err := file.Seek(-paneLogLimit, io.SeekEnd); err != nil {
		return fmt.Errorf("seek traffic capture pane log: %w", err)
	}
	data := make([]byte, paneLogLimit)
	count, err := io.ReadFull(file, data)
	if err != nil && err != io.ErrUnexpectedEOF {
		return fmt.Errorf("read traffic capture pane log tail: %w", err)
	}
	if err := file.Truncate(0); err != nil {
		return fmt.Errorf("truncate traffic capture pane log: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind traffic capture pane log: %w", err)
	}
	if _, err := file.Write(data[:count]); err != nil {
		return fmt.Errorf("rewrite traffic capture pane log: %w", err)
	}
	return nil
}
