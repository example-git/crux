package trafficcapture

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

const captureControlHelp = `cap list
cap status [NAME]
cap logs [NAME] [-f] [-n LINES]
cap start NAME -- EXECUTABLE [ARGUMENT ...]
cap stop [NAME]
cap kill [NAME]
cap restart [NAME]
cap remove [NAME]
cap send NAME TEXT

NAME defaults to main. Ctrl+C stops following logs, not the target.
Detach with F12 or Ctrl+B then D to keep the shell and processes running.
Exiting the management shell stops its processes and ends the capture.
`

type captureControlOptions struct {
	request captureControlRequest
	follow  bool
	lines   int
}

func parseCaptureControl(arguments []string) (captureControlOptions, error) {
	options := captureControlOptions{lines: 100}
	if len(arguments) == 0 {
		return options, fmt.Errorf("a capture control command is required")
	}
	options.request.Action = arguments[0]
	values := arguments[1:]
	switch arguments[0] {
	case "list":
		if len(values) != 0 {
			return options, fmt.Errorf("usage: cap list")
		}
	case "start":
		if len(values) < 2 {
			return options, fmt.Errorf("usage: cap start NAME -- EXECUTABLE [ARGUMENT ...]")
		}
		options.request.Name = values[0]
		options.request.Command = values[1:]
		if options.request.Command[0] == "--" {
			options.request.Command = options.request.Command[1:]
		}
		if len(options.request.Command) == 0 {
			return options, fmt.Errorf("start requires an executable")
		}
	case "send":
		if len(values) != 2 {
			return options, fmt.Errorf("usage: cap send NAME TEXT")
		}
		options.request.Name, options.request.Text = values[0], values[1]
	case "logs":
		for index := 0; index < len(values); index++ {
			switch value := values[index]; value {
			case "-f", "--follow":
				options.follow = true
			case "-n", "--lines":
				index++
				if index >= len(values) {
					return options, fmt.Errorf("%s requires a nonnegative line count", value)
				}
				count, err := strconv.Atoi(values[index])
				if err != nil || count < 0 {
					return options, fmt.Errorf("invalid line count: %s", values[index])
				}
				options.lines = count
			default:
				if strings.HasPrefix(value, "-") || options.request.Name != "" {
					return options, fmt.Errorf("unexpected logs argument: %s", value)
				}
				options.request.Name = value
			}
		}
	case "status", "stop", "kill", "restart", "remove":
		if len(values) > 1 {
			return options, fmt.Errorf("usage: cap %s [NAME]", arguments[0])
		}
		if len(values) == 1 {
			options.request.Name = values[0]
		}
	default:
		return options, fmt.Errorf("unknown capture control command: %s", arguments[0])
	}
	return options, nil
}

func callCaptureControl(ctx context.Context, socketPath string, request captureControlRequest) (json.RawMessage, error) {
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return nil, err
	}
	var response struct {
		Result json.RawMessage `json:"result"`
		Error  string          `json:"error"`
	}
	if err := json.NewDecoder(connection).Decode(&response); err != nil {
		return nil, err
	}
	if response.Error != "" {
		return nil, errors.New(response.Error)
	}
	return response.Result, nil
}

func RunCaptureControl(ctx context.Context, arguments []string, output io.Writer) error {
	if len(arguments) == 0 || (len(arguments) == 1 && (arguments[0] == "help" || arguments[0] == "--help" || arguments[0] == "-h")) {
		_, err := io.WriteString(output, captureControlHelp)
		return err
	}
	options, err := parseCaptureControl(arguments)
	if err != nil {
		return err
	}
	socketPath := os.Getenv(captureSocketVariable)
	if socketPath == "" {
		return fmt.Errorf("cap must run inside a capture management shell")
	}
	if options.request.Action == "start" {
		options.request.WorkingDir, err = os.Getwd()
		if err != nil {
			return err
		}
		options.request.Environment = environmentMap(os.Environ())
	}
	result, err := callCaptureControl(ctx, socketPath, options.request)
	if err != nil {
		return err
	}
	if options.request.Action == "logs" {
		var process captureProcessStatus
		if err := json.Unmarshal(result, &process); err != nil {
			return err
		}
		return readCaptureOutput(ctx, process.LogPath, options.lines, options.follow, output)
	}
	var formatted any
	if err := json.Unmarshal(result, &formatted); err != nil {
		return err
	}
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(formatted)
}

func readCaptureOutput(ctx context.Context, path string, lines int, follow bool, output io.Writer) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	tail := make([]string, 0)
	for {
		line, readErr := reader.ReadString('\n')
		if line != "" && lines > 0 {
			tail = append(tail, line)
			if len(tail) > lines {
				tail = tail[1:]
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return readErr
			}
			break
		}
	}
	for _, line := range tail {
		if _, err := io.WriteString(output, line); err != nil {
			return err
		}
	}
	if !follow {
		return nil
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := io.Copy(output, file); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
