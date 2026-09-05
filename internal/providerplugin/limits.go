package providerplugin

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/example-git/crux/internal/redact"
)

var (
	credentialDiagnosticPattern = regexp.MustCompile(`(?i)\b(authorization|api[_-]?key|access[_-]?token|refresh[_-]?token|client[_-]?secret)\s*[:=]\s*[^\s,;]+`)
	bearerDiagnosticPattern     = regexp.MustCompile(`(?i)\bbearer\s+[^\s,;]+`)
	urlDiagnosticPattern        = regexp.MustCompile(`(?i)\b(?:https?|wss?)://[^\s"']+`)
)

const (
	MaxBundleBytes       int64 = 64 << 20
	MaxFileBytes         int64 = 32 << 20
	MaxBundleFiles             = 1024
	MaxBundleDirectories       = 256
	MaxBundleDepth             = 16
	MaxRelativePathBytes       = 1024
	MaxDiagnosticBytes         = 1024
	MaxStaticTextBytes   int64 = 1 << 20
)

func safeDiagnostic(code, message string) Diagnostic {
	return Diagnostic{Code: code, Message: safeDiagnosticMessage(message)}
}

func safeDetailedDiagnostic(code string, phase DiagnosticPhase, path, message string) Diagnostic {
	return Diagnostic{Code: code, Message: safeDiagnosticMessage(message), Severity: DiagnosticSeverityError, Phase: phase, Path: path}
}

func safeDiagnosticMessage(message string) string {
	message = redact.String(message)
	message = credentialDiagnosticPattern.ReplaceAllString(message, `$1=<redacted>`)
	message = bearerDiagnosticPattern.ReplaceAllString(message, "Bearer <redacted>")
	message = urlDiagnosticPattern.ReplaceAllString(message, "<redacted-url>")
	message = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, message)
	message = strings.TrimSpace(message)
	if len(message) > MaxDiagnosticBytes {
		message = message[:MaxDiagnosticBytes]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
		message += "…"
	}
	return message
}
