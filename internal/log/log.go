package log

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/example-git/crux/internal/redact"
	"gopkg.in/natefinch/lumberjack.v2"
)

var (
	initOnce    sync.Once
	initialized atomic.Bool
)

func Setup(logFile string, debug bool, ws ...io.Writer) func() error {
	cleanup := func() error { return nil }
	initOnce.Do(func() {
		logRotator := &lumberjack.Logger{
			Filename:   logFile,
			MaxSize:    10,    // Max size in MB
			MaxBackups: 0,     // Number of backups
			MaxAge:     30,    // Days
			Compress:   false, // Enable compression
		}

		level := slog.LevelInfo
		if debug {
			level = slog.LevelDebug
		}

		opts := &slog.HandlerOptions{
			Level:     level,
			AddSource: true,
		}

		writer := &ownedLogWriter{writer: logRotator}
		var handlers []slog.Handler
		handlers = append(handlers, redactingHandler{handler: slog.NewJSONHandler(writer, opts)})

		for _, w := range ws {
			if w == nil {
				continue
			}
			if f, ok := w.(term.File); ok && term.IsTerminal(f.Fd()) {
				handlers = append(handlers, redactingHandler{handler: slog.NewTextHandler(w, opts)})
			} else {
				handlers = append(handlers, redactingHandler{handler: slog.NewJSONHandler(w, opts)})
			}
		}

		previous := slog.Default()
		logger := slog.New(slog.NewMultiHandler(handlers...))
		slog.SetDefault(logger)
		cleanup = sync.OnceValue(func() error {
			if slog.Default() == logger {
				slog.SetDefault(previous)
			}
			return writer.Close()
		})
		initialized.Store(true)
	})
	return cleanup
}

type ownedLogWriter struct {
	mu     sync.Mutex
	writer io.WriteCloser
	closed bool
}

func (w *ownedLogWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, os.ErrClosed
	}
	return w.writer.Write(data)
}

func (w *ownedLogWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	return w.writer.Close()
}

func Initialized() bool {
	return initialized.Load()
}

func RecoverPanic(name string, cleanup func()) {
	if r := recover(); r != nil {
		// Create a timestamped panic log file
		timestamp := time.Now().Format("20060102-150405")
		filename := fmt.Sprintf("crux-panic-%s-%s.log", name, timestamp)

		file, err := os.Create(filename)
		if err == nil {
			defer file.Close()

			// Write panic information and stack trace
			fmt.Fprintf(file, "Panic in %s: %s\n\n", redact.String(name), redact.String(fmt.Sprint(r)))
			fmt.Fprintf(file, "Time: %s\n\n", time.Now().Format(time.RFC3339))
			fmt.Fprintf(file, "Stack Trace:\n%s\n", redact.Bytes(debug.Stack()))

			// Execute cleanup function if provided
			if cleanup != nil {
				cleanup()
			}
		}
	}
}
