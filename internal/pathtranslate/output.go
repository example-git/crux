package pathtranslate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/example-git/crux/internal/fsext"
)

var outputPathTokens = regexp.MustCompile("\"[^\"]*\"|'[^']*'|`[^`]*`|[^\\s\"'`<>\\[\\]{}(),;|=&:]+")

func (t *Translator) OutputReferences(text string, paths []string) ([]PathReference, error) {
	if !t.settings.Enabled {
		return nil, nil
	}
	directories := make(map[string]bool)
	explicit := make(map[string][]string)
	for _, path := range append(append([]string(nil), paths...), t.cwd) {
		if !filepath.IsAbs(path) {
			continue
		}
		path = filepath.Clean(path)
		if canonical, err := fsext.CanonicalPath(path); err == nil {
			path = canonical
		}
		if t.Protected(path) {
			explicit[filepath.Base(path)] = append(explicit[filepath.Base(path)], path)
		}
		info, err := os.Stat(path)
		if err == nil && info.IsDir() {
			directories[path] = true
		} else {
			directories[filepath.Dir(path)] = true
		}
	}
	roots := make([]string, 0, len(directories))
	for directory := range directories {
		roots = append(roots, directory)
	}
	sort.Strings(roots)
	cache := make(map[string]string)
	resolve := func(value string) (string, error) {
		if cached, exists := cache[value]; exists {
			return cached, nil
		}
		if value == "" || value == "." || value == ".." || strings.Contains(value, AliasPrefix) {
			return "", nil
		}
		candidates := append([]string(nil), explicit[value]...)
		for _, directory := range roots {
			candidate := value
			if !filepath.IsAbs(candidate) {
				candidate = filepath.Join(directory, candidate)
			}
			if !t.Protected(candidate) {
				continue
			}
			if _, err := os.Lstat(candidate); err == nil {
				candidates = append(candidates, filepath.Clean(candidate))
			}
		}
		var selected string
		for _, candidate := range candidates {
			if selected != "" && candidate != selected {
				return "", fmt.Errorf("shell output filename has multiple possible protected targets; request an explicit path listing")
			}
			selected = candidate
		}
		if selected == "" {
			cache[value] = ""
			return "", nil
		}
		cache[value] = selected
		return selected, nil
	}
	var references []PathReference
	offset := 0
	for len(text) > 0 {
		end := strings.IndexAny(text, "\n\r\x00\t")
		if end < 0 {
			end = len(text)
		}
		line := text[:end]
		var suffix *PathReference
		for start, character := range line {
			if unicode.IsSpace(character) || start > 0 && line[start-1] != ' ' {
				continue
			}
			candidate := strings.TrimRightFunc(line[start:], unicode.IsSpace)
			path, err := resolve(candidate)
			if err != nil {
				return nil, err
			}
			if path != "" {
				name := strings.TrimRight(candidate, string(filepath.Separator))
				if name == "" {
					name = candidate
				}
				suffix = &PathReference{Start: offset + start, End: offset + start + len(name), Path: path, Base: !strings.ContainsRune(name, filepath.Separator)}
				line = line[:start]
				break
			}
		}
		for _, span := range outputPathTokens.FindAllStringIndex(line, -1) {
			start, end := span[0], span[1]
			if end-start >= 2 && strings.ContainsRune("\"'`", rune(line[start])) {
				start++
				end--
			}
			candidate := line[start:end]
			path, err := resolve(candidate)
			if err != nil {
				return nil, err
			}
			if path != "" {
				name := strings.TrimRight(candidate, string(filepath.Separator))
				if name == "" {
					name = candidate
				}
				references = append(references, PathReference{Start: offset + start, End: offset + start + len(name), Path: path, Base: !strings.ContainsRune(name, filepath.Separator)})
			}
		}
		if suffix != nil {
			references = append(references, *suffix)
		}
		text = text[end:]
		offset += end
		if text != "" {
			text = text[1:]
			offset++
		}
	}
	return references, nil
}

func (t *Translator) OutputText(ctx context.Context, text string, paths []string) (string, error) {
	references, err := t.OutputReferences(text, paths)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(PathReferences{Paths: references})
	if err != nil {
		return "", err
	}
	return t.ReferencedText(ctx, text, string(encoded))
}

func (t *Translator) OutputJSONReferences(text string, paths []string) ([]PathReference, error) {
	decoder := json.NewDecoder(strings.NewReader(text))
	var references []PathReference
	for {
		start := int(decoder.InputOffset())
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		value, ok := token.(string)
		if !ok {
			continue
		}
		start += strings.IndexByte(text[start:int(decoder.InputOffset())], '"')
		found, err := t.OutputReferences(value, paths)
		if err != nil {
			return nil, err
		}
		for _, reference := range found {
			prefix, err := json.Marshal(value[:reference.Start])
			if err != nil {
				return nil, err
			}
			through, err := json.Marshal(value[:reference.End])
			if err != nil {
				return nil, err
			}
			reference.Start = start + len(prefix) - 1
			reference.End = start + len(through) - 1
			references = append(references, reference)
		}
	}
	return references, nil
}
