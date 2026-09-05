package localaddon

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/example-git/crux/internal/compatibility"
)

type parser struct {
	args  []string
	index int
}

func (p *parser) more() bool {
	return p.index < len(p.args)
}

func (p *parser) next() string {
	value := p.args[p.index]
	p.index++
	return value
}

func (p *parser) value(option, argument string) (string, error) {
	if _, value, ok := strings.Cut(argument, "="); ok {
		if value == "" {
			return "", fmt.Errorf("missing value for %s", option)
		}
		return value, nil
	}
	if !p.more() {
		return "", fmt.Errorf("missing value for %s", option)
	}
	return p.next(), nil
}

func optionName(argument string) string {
	name, _, _ := strings.Cut(argument, "=")
	return name
}

func consumeNoopValue(p *parser, name, argument string) error {
	_, err := p.value(name, argument)
	return err
}

func consumeNoopOptionalValue(p *parser, argument string) {
	if strings.Contains(argument, "=") {
		return
	}
	if p.more() && !strings.HasPrefix(p.args[p.index], "-") {
		p.next()
	}
}

func consumeNoopVariadic(p *parser, name, argument string) error {
	if _, err := p.value(name, argument); err != nil {
		return err
	}
	for p.more() && !strings.HasPrefix(p.args[p.index], "-") {
		p.next()
	}
	return nil
}

func enum(value string, allowed ...string) bool {
	return slices.Contains(allowed, value)
}

func environmentBool(environment []string, key string) bool {
	prefix := key + "="
	for _, entry := range slices.Backward(environment) {
		if value, ok := strings.CutPrefix(entry, prefix); ok {
			parsed, err := strconv.ParseBool(value)
			return err == nil && parsed
		}
	}
	return false
}

func absoluteFrom(base, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(base, path)
}

func nonTerminalOutput(writer io.Writer) bool {
	if writer == nil {
		return false
	}
	if file, ok := writer.(*os.File); ok {
		info, err := file.Stat()
		return err == nil && info.Mode()&os.ModeCharDevice == 0
	}
	return true
}

func stdinText(reader io.Reader) (string, bool, error) {
	if reader == nil {
		return "", false, nil
	}
	if file, ok := reader.(*os.File); ok {
		info, err := file.Stat()
		if err != nil {
			return "", false, err
		}
		if info.Mode()&os.ModeCharDevice != 0 {
			return "", false, nil
		}
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return "", false, err
	}
	return string(data), len(data) != 0, nil
}

func parseDuration(value string) (time.Duration, error) {
	duration, err := time.ParseDuration(value)
	if err == nil {
		return duration, nil
	}
	seconds, secondsErr := strconv.ParseFloat(value, 64)
	if secondsErr != nil || seconds < 0 {
		return 0, err
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

func parseError(code int, format string, values ...any) error {
	return &compatibility.ExitError{Code: code, Stderr: fmt.Sprintf(format, values...)}
}

func successOutput(text string, stderr bool) error {
	exit := &compatibility.ExitError{Code: 0}
	if stderr {
		exit.Stderr = text
	} else {
		exit.Stdout = text
	}
	return exit
}
