package pathtranslate

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
)

type Scope struct {
	Kind      string `json:"kind"`
	SessionID string `json:"session_id,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Model     string `json:"model,omitempty"`
}

func (s Scope) Key() (string, error) {
	if s.Kind != "session" {
		return "", fmt.Errorf("unsupported path translation scope %q: only session scope is supported", s.Kind)
	}
	if s.SessionID == "" || s.Provider != "" || s.Model != "" {
		return "", errors.New("session scope requires only a session ID")
	}
	data, err := json.Marshal(s)
	return string(data), err
}

type Target struct {
	Path         string `json:"path"`
	Label        string `json:"label,omitempty"`
	Recursive    bool   `json:"recursive"`
	CensorImages bool   `json:"censor_images"`
}

type Settings struct {
	CensorImages *bool    `json:"censor_images,omitempty"`
	Enabled      bool     `json:"enabled"`
	Inherit      bool     `json:"inherit,omitempty"`
	Targets      []Target `json:"targets"`
}

func (s Settings) Validate() error {
	if s.Inherit && (s.Enabled || len(s.Targets) != 0 || s.CensorImages != nil) {
		return errors.New("inherited path translation cannot include an enabled override or targets")
	}
	seen := make(map[string]bool, len(s.Targets))
	labels := make(map[string]bool, len(s.Targets))
	for _, target := range s.Targets {
		if !filepath.IsAbs(target.Path) || filepath.Clean(target.Path) != target.Path {
			return errors.New("path translation targets must be clean absolute paths")
		}
		if target.Path == string(filepath.Separator) {
			return errors.New("the filesystem root has no name to translate; select a file or directory")
		}
		if target.Label != "" {
			if target.Label == "." || target.Label == ".." || strings.ContainsAny(target.Label, "/\\") || strings.Contains(target.Label, AliasPrefix) || strings.IndexFunc(target.Label, unicode.IsControl) >= 0 || strings.TrimSpace(target.Label) != target.Label {
				return errors.New("folder label must be a single name without slashes, control characters, outer whitespace, or the reserved pt_ prefix")
			}
			if labels[target.Label] {
				return errors.New("folder labels must be unique within this scope")
			}
			labels[target.Label] = true
		}
		if seen[target.Path] {
			return errors.New("duplicate path translation target")
		}
		seen[target.Path] = true
	}
	return nil
}
