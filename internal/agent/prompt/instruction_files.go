package prompt

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/filepathext"
)

type InstructionFile struct {
	Path     string `json:"path"`
	Scope    string `json:"scope"`
	Provider string `json:"provider,omitempty"`
}

func (p *Prompt) InstructionFiles(store *config.ConfigStore) ([]InstructionFile, error) {
	snapshot := store.RuntimeSnapshot()
	cfg := snapshot.Config()
	files := []InstructionFile{}
	seen := map[string]bool{}
	add := func(path, scope, provider string) {
		path = filepath.Clean(path)
		if !seen[path] {
			seen[path] = true
			files = append(files, InstructionFile{Path: path, Scope: scope, Provider: provider})
		}
	}
	if cfg.Options != nil {
		for _, group := range []struct {
			paths []string
			scope string
		}{{cfg.Options.ContextPaths, "project"}, {cfg.Options.GlobalContextPaths, "global"}} {
			for _, path := range group.paths {
				path = filepathext.SmartJoin(store.WorkingDir(), expandPath(path, snapshot.Resolve))
				info, err := os.Stat(path)
				if err != nil && !os.IsNotExist(err) {
					return nil, err
				}
				if info == nil || !info.IsDir() {
					add(path, group.scope, "")
					continue
				}
				if err := filepath.WalkDir(path, func(path string, entry fs.DirEntry, err error) error {
					if err != nil {
						return err
					}
					if !entry.IsDir() {
						add(path, group.scope, "")
					}
					return nil
				}); err != nil {
					return nil, err
				}
			}
		}
	}
	if cfg.Providers != nil {
		for provider := range cfg.Providers.Seq2() {
			if err := validateProviderInstructionsID(provider); err != nil {
				return nil, err
			}
			add(filepath.Join(p.providerInstructionsDir, provider+".txt"), "provider", provider)
		}
	}
	slices.SortFunc(files, func(a, b InstructionFile) int { return strings.Compare(a.Path, b.Path) })
	return files, nil
}
