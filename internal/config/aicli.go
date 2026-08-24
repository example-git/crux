package config

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// aiCliDir returns ~/.ai-cli, the root of the ai-mux-tui instruction store.
func aiCliDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".ai-cli")
}

// AiCliProjectInstructionsPath computes the per-project instructions path
// that ai-mux-tui writes to. The path is keyed on the resolved working
// directory so each project gets a stable slot:
//
//	~/.ai-cli/project-prompts/<slug>-<hash>/instructions.txt
func AiCliProjectInstructionsPath(workingDir string) string {
	root := aiCliDir()
	if root == "" || workingDir == "" {
		return ""
	}
	label := filepath.Base(workingDir)
	if label == "" || label == "." || label == "/" {
		label = "project"
	}
	slug := slugify(label)
	digest := sha256.Sum256([]byte(workingDir))
	hash := hex.EncodeToString(digest[:])[:12]
	return filepath.Join(root, "project-prompts", slug+"-"+hash, "instructions.txt")
}

func slugify(value string) string {
	slug := strings.TrimSpace(strings.ToLower(value))
	slug = slugRe.ReplaceAllString(slug, "-")
	slug = strings.Trim(slug, "-")
	if slug == "" {
		slug = "project"
	}
	if len(slug) > 48 {
		slug = slug[:48]
	}
	return slug
}
