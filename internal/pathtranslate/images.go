package pathtranslate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/fsext"
	"github.com/example-git/crux/internal/imageutil"
)

func localImagePath(value string) (string, error) {
	if strings.HasPrefix(strings.ToLower(value), "file:") {
		uri, err := url.Parse(value)
		if err != nil {
			return "", fmt.Errorf("decode image file URI: %w", err)
		}
		if uri.Host != "" && !strings.EqualFold(uri.Host, "localhost") || uri.Opaque != "" || !filepath.IsAbs(uri.Path) || uri.RawQuery != "" || uri.Fragment != "" {
			return "", fmt.Errorf("image file URI must identify a local absolute path")
		}
		return uri.Path, nil
	}
	if strings.Contains(value, "://") {
		return "", nil
	}
	if uri, err := url.Parse(value); err == nil && uri.Scheme != "" {
		return "", nil
	}
	return value, nil
}

func ToolImageSources(metadata, input string) ([]string, error) {
	var sources []string
	seen := make(map[string]bool)
	add := func(value string) {
		if value != "" && !seen[value] {
			seen[value] = true
			sources = append(sources, value)
		}
	}
	if metadata != "" {
		var source struct {
			FilePath   string   `json:"file_path"`
			SourcePath string   `json:"source_path"`
			Path       string   `json:"path"`
			URI        string   `json:"uri"`
			Paths      []string `json:"image_source_paths"`
			Sources    []string `json:"source_paths"`
		}
		if err := json.Unmarshal([]byte(metadata), &source); err != nil {
			return nil, err
		}
		for _, value := range append(source.Paths, source.FilePath, source.SourcePath, source.Path, source.URI) {
			add(value)
		}
		for _, value := range source.Sources {
			add(value)
		}
	}
	if input != "" {
		_, err := transformJSONStrings(input, func(key, value string) (string, error) {
			if pathField(key) || key == "source_path" || key == "image_path" || key == "uri" || key == "url" || key == "paths" {
				add(value)
			}
			return value, nil
		})
		if err != nil {
			return nil, err
		}
	}
	return sources, nil
}

func (t *Translator) imageProtected(value string) (bool, error) {
	path, err := localImagePath(value)
	if err != nil || path == "" {
		return false, err
	}
	path = t.absolute(path)
	if t.censorImagePath(path) {
		return true, nil
	}
	canonical, err := t.canonicalImagePath(path)
	if err != nil {
		return false, err
	}
	return t.censorImagePath(canonical), nil
}

func (t *Translator) canonicalImagePath(path string) (string, error) {
	canonical, err := fsext.CanonicalPath(t.absolute(path))
	if err != nil {
		return "", fmt.Errorf("resolve image source: %w", err)
	}
	return canonical, nil
}

func (t *Translator) imageSources(ctx context.Context, sources []string) ([]string, error) {
	var result []string
	seen := make(map[string]bool)
	for _, source := range sources {
		path, err := localImagePath(source)
		if err != nil {
			return nil, err
		}
		if path == "" {
			continue
		}
		path, err = t.ResolvePath(ctx, path)
		if err != nil {
			return nil, err
		}
		path = t.absolute(path)
		canonical, err := t.canonicalImagePath(path)
		if err != nil {
			return nil, err
		}
		for _, value := range []string{path, canonical} {
			if !seen[value] {
				seen[value] = true
				result = append(result, value)
			}
		}
	}
	return result, nil
}

func imageData(mediaType string, data []byte) bool {
	return strings.HasPrefix(strings.ToLower(mediaType), "image/") || strings.HasPrefix(http.DetectContentType(data), "image/")
}

func (t *Translator) ImageFile(ctx context.Context, file fantasy.FilePart) (fantasy.FilePart, error) {
	fingerprint := t.Fingerprint()
	if file.PathTranslated && file.PathTranslationPolicy == fingerprint {
		return file, nil
	}
	if file.PathTranslationOriginal != nil {
		file = *file.PathTranslationOriginal
	}
	if !t.settings.Enabled {
		return file, nil
	}
	original := file
	if imageData(file.MediaType, file.Data) {
		sources := append(append([]string(nil), file.SourcePaths...), file.SourcePath)
		if file.SourcePath == "" && len(file.SourcePaths) == 0 && (filepath.IsAbs(file.Filename) || strings.HasPrefix(strings.ToLower(file.Filename), "file:")) {
			sources = append(sources, file.Filename)
		}
		var err error
		sources, err = t.imageSources(ctx, sources)
		if err != nil {
			return fantasy.FilePart{}, err
		}
		file.SourcePaths, original.SourcePaths = sources, sources
		for _, source := range sources {
			if !t.censorImagePath(source) {
				continue
			}
			data, err := imageutil.Censor(file.Data)
			if err != nil {
				return fantasy.FilePart{}, err
			}
			file.Data, file.MediaType = data, "image/png"
			file.NeedsImageNormalization = true
			break
		}
	}
	filename := file.Filename
	if strings.HasPrefix(strings.ToLower(filename), "file:") {
		var err error
		filename, err = localImagePath(filename)
		if err != nil {
			return fantasy.FilePart{}, err
		}
	}
	filename, err := t.Path(ctx, filename)
	if err != nil {
		return fantasy.FilePart{}, err
	}
	file.Filename, file.PathTranslated = filename, true
	file.PathTranslationPolicy, file.PathTranslationOriginal = fingerprint, &original
	return file, nil
}
