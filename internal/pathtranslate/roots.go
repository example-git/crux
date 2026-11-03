package pathtranslate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/example-git/crux/internal/fsext"
)

type rootSpelling struct {
	children  map[byte]*rootSpelling
	path      string
	ambiguous bool
}

func (r *rootSpelling) add(spelling, path string) {
	for index := range len(spelling) {
		if r.children == nil {
			r.children = make(map[byte]*rootSpelling)
		}
		next := r.children[spelling[index]]
		if next == nil {
			next = &rootSpelling{}
			r.children[spelling[index]] = next
		}
		r = next
	}
	if r.path == "" {
		r.path = path
	} else if r.path != path {
		r.ambiguous = true
	}
}

func rootBoundary(value rune) bool {
	return unicode.IsSpace(value) || strings.ContainsRune("/\\\"'`<>[]{}(),;|=&:", value)
}

func (t *Translator) selectedRoots(ctx context.Context, text string) (string, error) {
	translated, err := t.selectedSpellings(ctx, text, t.roots, false)
	if err != nil {
		return "", err
	}
	return t.selectedSpellings(ctx, translated, t.basenames, true)
}

func (r *rootSpelling) addBasename(path string) {
	basename := filepath.Base(path)
	canonical, err := fsext.CanonicalPath(path)
	if err == nil {
		path = canonical
	}
	r.add(basename, path)
}

func spellingEnd(text string, index int) bool {
	if index == len(text) {
		return true
	}
	next, _ := utf8.DecodeRuneInString(text[index:])
	if rootBoundary(next) {
		return true
	}
	if next == '.' {
		rest := strings.TrimLeft(text[index:], ".")
		next, _ = utf8.DecodeRuneInString(rest)
		return rest == "" || unicode.IsSpace(next) || strings.ContainsRune("\"'`)]}", next)
	}
	return false
}

func (t *Translator) selectedSpellings(ctx context.Context, text string, roots *rootSpelling, basename bool) (string, error) {
	if roots == nil {
		return text, nil
	}
	var output strings.Builder
	for offset := 0; offset < len(text); {
		previous, _ := utf8.DecodeLastRuneInString(text[:offset])
		if offset > 0 && (!rootBoundary(previous) || basename && (previous == '/' || previous == '\\')) {
			_, size := utf8.DecodeRuneInString(text[offset:])
			output.WriteString(text[offset : offset+size])
			offset += size
			continue
		}
		node := roots
		var matched *rootSpelling
		end := offset
		for index := offset; index < len(text); index++ {
			node = node.children[text[index]]
			if node == nil {
				break
			}
			next, _ := utf8.DecodeRuneInString(text[index+1:])
			if node.path != "" && spellingEnd(text, index+1) && (!basename || next != '/' && next != '\\') {
				matched, end = node, index+1
			}
		}
		if matched == nil {
			_, size := utf8.DecodeRuneInString(text[offset:])
			output.WriteString(text[offset : offset+size])
			offset += size
			continue
		}
		if matched.ambiguous {
			return "", errors.New("selected filename is ambiguous; use its full path")
		}
		path := matched.path
		if end < len(text) && text[end] == filepath.Separator {
			limit := strings.IndexAny(text[end:], "\n\r\x00")
			if limit < 0 {
				limit = len(text) - end
			}
			suffix := text[end : end+limit]
			for candidateEnd := len(suffix); candidateEnd > 0; candidateEnd-- {
				if candidateEnd < len(suffix) {
					next, _ := utf8.DecodeRuneInString(suffix[candidateEnd:])
					if !rootBoundary(next) && next != '.' {
						continue
					}
				}
				candidate := path + suffix[:candidateEnd]
				if _, err := os.Lstat(t.absolute(candidate)); err == nil {
					path = candidate
					end += candidateEnd
					break
				}
			}
		}
		translated, err := t.Path(ctx, path)
		if err != nil {
			return "", err
		}
		if basename {
			translated = filepath.Base(translated)
		}
		output.WriteString(translated)
		offset = end
	}
	return output.String(), nil
}

func (t *Translator) withToolPaths(ctx context.Context, input string) (*Translator, error) {
	if input == "" {
		return t, nil
	}
	var paths []string
	_, err := transformJSONStrings(input, func(key, value string) (string, error) {
		if !pathField(key) || value == "" {
			return value, nil
		}
		resolved, err := t.ResolveText(ctx, value)
		if err != nil {
			return "", err
		}
		if t.Protected(resolved) {
			paths = append(paths, t.absolute(resolved))
		}
		return value, nil
	})
	if err != nil || len(paths) == 0 {
		return t, err
	}
	copy := *t
	copy.roots = &rootSpelling{}
	copy.basenames = &rootSpelling{}
	for _, target := range t.settings.Targets {
		paths = append(paths, target.Path)
	}
	for _, path := range paths {
		copy.roots.add(path, path)
		copy.basenames.addBasename(path)
		if relative, err := filepath.Rel(t.cwd, path); err == nil && relative != "." {
			copy.roots.add(relative, relative)
		}
	}
	return &copy, nil
}

func pathField(key string) bool {
	switch key {
	case "path", "path_prefix", "file", "files", "file_path", "working_dir", "directory", "root", "old_path", "new_path", "source", "destination", "filename", "paths", "output_path", "output_directory":
		return true
	}
	return false
}
