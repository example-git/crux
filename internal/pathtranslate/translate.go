package pathtranslate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/example-git/crux/internal/fsext"
)

type Translator struct {
	names          *Names
	settings       Settings
	cwd            string
	roots          *rootSpelling
	basenames      *rootSpelling
	labels         map[string]string
	labelPaths     map[string]string
	displayLabels  map[string]string
	labelSpellings *rootSpelling
}

func (t *Translator) Fingerprint() string {
	if t == nil || !t.settings.Enabled {
		return ""
	}
	encoded, _ := json.Marshal(t.settings)
	return t.names.identity("policy\x00" + string(encoded))
}

func New(names *Names, settings Settings, cwd string) (*Translator, error) {
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(cwd) {
		return nil, errors.New("path translation requires an absolute working directory")
	}
	if settings.Enabled && names == nil {
		return nil, errors.New("path translation identity store is required")
	}
	settings.Targets = append([]Target(nil), settings.Targets...)
	if settings.CensorImages != nil {
		value := *settings.CensorImages
		settings.CensorImages = &value
	}
	if settings.Enabled {
		for _, target := range settings.Targets {
			canonical, err := fsext.CanonicalPath(target.Path)
			if err != nil {
				return nil, fmt.Errorf("resolve selected path translation target: %w", err)
			}
			if canonical != target.Path {
				target.Path = canonical
				settings.Targets = append(settings.Targets, target)
			}
		}
	}
	translator := &Translator{names: names, settings: settings, cwd: filepath.Clean(cwd)}
	if settings.Enabled {
		translator.roots = &rootSpelling{}
		translator.basenames = &rootSpelling{}
		translator.labels = make(map[string]string)
		translator.labelPaths = make(map[string]string)
		translator.displayLabels = make(map[string]string)
		translator.labelSpellings = &rootSpelling{}
		for _, target := range settings.Targets {
			if target.Label != "" {
				identity := names.identity(target.Path)
				translator.labels[target.Label] = identity
				translator.labelPaths[identity] = target.Path
				translator.displayLabels[identity] = target.Label
				translator.labelSpellings.add(target.Label, identity)
			}
			translator.roots.add(target.Path, target.Path)
			translator.basenames.addBasename(target.Path)
			if relative, err := filepath.Rel(translator.cwd, target.Path); err == nil && relative != "." {
				translator.roots.add(relative, relative)
			}
		}
	}
	return translator, nil
}

func (t *Translator) absolute(path string) string {
	if !filepath.IsAbs(path) {
		path = filepath.Join(t.cwd, path)
	}
	return filepath.Clean(path)
}

func within(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

func (t *Translator) Protected(path string) bool {
	if !t.settings.Enabled {
		return false
	}
	path = t.absolute(path)
	for _, target := range t.settings.Targets {
		if path == target.Path || target.Recursive && within(path, target.Path) {
			return true
		}
	}
	return false
}

func (t *Translator) CensorImage(path string) bool {
	if !t.settings.Enabled {
		return false
	}
	protected, err := t.imageProtected(path)
	return err == nil && protected
}

func (t *Translator) censorImagePath(path string) bool {
	if !t.settings.Enabled {
		return false
	}
	path = t.absolute(path)
	for _, target := range t.settings.Targets {
		censor := target.CensorImages
		if t.settings.CensorImages != nil {
			censor = *t.settings.CensorImages
		}
		if censor && (path == target.Path || target.Recursive && within(path, target.Path)) {
			return true
		}
	}
	return false
}

func (t *Translator) CheckMove(source, destination string) error {
	if !t.settings.Enabled {
		return nil
	}
	source, destination = t.absolute(source), t.absolute(destination)
	if t.Protected(source) || t.Protected(destination) {
		return errors.New("renaming or moving protected paths is not permitted")
	}
	for _, target := range t.settings.Targets {
		if within(target.Path, source) || within(target.Path, destination) {
			return errors.New("renaming or moving an ancestor of a protected path is not permitted")
		}
	}
	return nil
}

func (t *Translator) Path(ctx context.Context, path string) (string, error) {
	if !t.settings.Enabled || path == "" {
		return path, nil
	}
	for _, component := range strings.Split(path, string(filepath.Separator)) {
		if IsAlias(component) {
			resolved, err := t.ResolvePath(ctx, path)
			if err != nil {
				return "", err
			}
			path = resolved
			break
		}
	}
	absolute := t.absolute(path)
	components := strings.Split(absolute, string(filepath.Separator))
	current := string(filepath.Separator)
	changed := false
	for index, component := range components {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		if !t.Protected(current) {
			continue
		}
		name, err := t.names.Register(ctx, current)
		if err != nil {
			return "", err
		}
		components[index] = name.Alias
		if label := t.displayLabels[name.Identity]; label != "" {
			components[index] = label
		}
		changed = true
	}
	if !changed {
		return path, nil
	}
	translated := strings.Join(components, string(filepath.Separator))
	if !filepath.IsAbs(path) && !t.Protected(t.cwd) {
		if relative, err := filepath.Rel(t.cwd, translated); err == nil {
			translated = relative
		}
	}
	return translated, nil
}

func (t *Translator) ResolvePath(ctx context.Context, path string) (string, error) {
	if t.names == nil {
		return path, nil
	}
	components := strings.Split(path, string(filepath.Separator))
	resolved := ""
	if filepath.IsAbs(path) {
		resolved = string(filepath.Separator)
	}
	changed := false
	for _, component := range components {
		if identity, ok := t.labels[component]; ok {
			resolved = t.labelPaths[identity]
			changed = true
		} else if IsAlias(component) {
			real, err := t.resolveIdentity(ctx, component)
			if err != nil {
				return "", err
			}
			resolved = real
			changed = true
		} else {
			resolved = filepath.Join(resolved, component)
		}
	}
	if !changed {
		return path, nil
	}
	return resolved, nil
}

func (t *Translator) JSON(ctx context.Context, input string, decode bool) (string, error) {
	return transformJSONStrings(input, func(key, value string) (string, error) {
		if decode {
			if key == "command" {
				return t.ResolveShell(ctx, value)
			}
			return t.ResolveText(ctx, value)
		}
		if pathField(key) {
			return t.Path(ctx, value)
		}
		return t.Text(ctx, value)
	})
}

func (t *Translator) Text(ctx context.Context, text string) (string, error) {
	if !t.settings.Enabled {
		return text, nil
	}
	var err error
	text, err = t.selectedRoots(ctx, text)
	if err != nil {
		return "", err
	}
	return transformPathTokens(text, func(token string) (string, error) {
		if !strings.ContainsRune(token, filepath.Separator) {
			return token, nil
		}
		return t.Path(ctx, token)
	})
}

func (t *Translator) ResolveText(ctx context.Context, text string) (string, error) {
	var err error
	text, err = t.resolveLabels(ctx, text)
	if err != nil {
		return "", err
	}
	if !strings.Contains(text, AliasPrefix) {
		return text, nil
	}
	return transformPathTokens(text, func(token string) (string, error) {
		return t.ResolvePath(ctx, token)
	})
}
