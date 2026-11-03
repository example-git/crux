package pathtranslate

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type PathReference struct {
	Start int    `json:"start"`
	End   int    `json:"end"`
	Path  string `json:"path"`
	Base  bool   `json:"base,omitempty"`
}

type PathReferences struct {
	Paths []PathReference `json:"path_references,omitempty"`
}

func (t *Translator) ReferencedText(ctx context.Context, text, metadata string) (string, error) {
	var references PathReferences
	if metadata != "" {
		if err := json.Unmarshal([]byte(metadata), &references); err != nil {
			return "", err
		}
	}
	var output strings.Builder
	previous := 0
	for _, reference := range references.Paths {
		if reference.Start < previous || reference.End <= reference.Start || reference.End > len(text) || reference.Path == "" {
			return "", errors.New("invalid tool path reference")
		}
		prefix, err := t.Text(ctx, text[previous:reference.Start])
		if err != nil {
			return "", err
		}
		path, err := t.Path(ctx, reference.Path)
		if err != nil {
			return "", err
		}
		output.WriteString(prefix)
		if path == reference.Path {
			output.WriteString(text[reference.Start:reference.End])
		} else {
			spelling := text[reference.Start:reference.End]
			name := strings.TrimRight(spelling, string(filepath.Separator))
			if reference.Base || name == filepath.Base(reference.Path) {
				path = filepath.Base(path)
			}
			output.WriteString(path)
			output.WriteString(spelling[len(name):])
		}
		previous = reference.End
	}
	tail, err := t.Text(ctx, text[previous:])
	if err != nil {
		return "", err
	}
	output.WriteString(tail)
	return output.String(), nil
}

func mergePathReferences(text string, current, stored []PathReference) ([]PathReference, error) {
	references := append(append([]PathReference(nil), current...), stored...)
	for _, reference := range references {
		if reference.Start < 0 || reference.End <= reference.Start || reference.End > len(text) || reference.Path == "" {
			return nil, errors.New("invalid tool path reference")
		}
	}
	sort.SliceStable(references, func(i, j int) bool {
		if references[i].Start == references[j].Start {
			return references[i].End > references[j].End
		}
		return references[i].Start < references[j].Start
	})
	merged := make([]PathReference, 0, len(references))
	for _, reference := range references {
		if len(merged) > 0 && reference.Start < merged[len(merged)-1].End {
			if reference.End <= merged[len(merged)-1].End {
				continue
			}
			return nil, errors.New("overlapping tool path references")
		}
		merged = append(merged, reference)
	}
	return merged, nil
}

func RetainPathReferences(metadata string, originalLength, headEnd, tailStart, tailOffset int) (string, error) {
	if metadata == "" {
		return metadata, nil
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal([]byte(metadata), &values); err != nil {
		return "", err
	}
	raw, exists := values["path_references"]
	if !exists {
		return metadata, nil
	}
	var references []PathReference
	if err := json.Unmarshal(raw, &references); err != nil {
		return "", err
	}
	var retained []PathReference
	previous := 0
	for _, reference := range references {
		if reference.Start < previous || reference.End <= reference.Start || reference.End > originalLength || reference.Path == "" {
			return "", errors.New("invalid tool path reference")
		}
		previous = reference.End
		if reference.Start < headEnd {
			retained = append(retained, PathReference{Start: reference.Start, End: min(reference.End, headEnd), Path: reference.Path, Base: reference.Base})
		}
	}
	for _, reference := range references {
		if reference.End > tailStart {
			retained = append(retained, PathReference{Start: max(reference.Start, tailStart) - tailStart + tailOffset, End: reference.End - tailStart + tailOffset, Path: reference.Path, Base: reference.Base})
		}
	}
	encoded, err := json.Marshal(retained)
	if err != nil {
		return "", err
	}
	values["path_references"] = encoded
	result, err := json.Marshal(values)
	return string(result), err
}

var semanticResultPath = regexp.MustCompile(`^\d+\. (.*):\d+-\d+ \(score `)

func searchPathMetadata(text, metadata, toolName string) (string, error) {
	if toolName != "search" && toolName != "codebase_search" {
		return metadata, nil
	}
	var values map[string]json.RawMessage
	if metadata != "" {
		if err := json.Unmarshal([]byte(metadata), &values); err != nil {
			return "", err
		}
		if _, exists := values["path_references"]; exists {
			return metadata, nil
		}
	}
	var mode string
	if raw := values["mode"]; raw != nil {
		if err := json.Unmarshal(raw, &mode); err != nil {
			return "", err
		}
	}
	var references PathReferences
	offset := 0
	for _, line := range strings.Split(text, "\n") {
		start, end := 0, 0
		if toolName == "codebase_search" {
			if match := semanticResultPath.FindStringSubmatchIndex(line); len(match) > 0 {
				start, end = match[2], match[3]
			}
		} else if mode == "files" && filepath.IsAbs(line) {
			end = len(line)
		} else if mode == "content" {
			if filepath.IsAbs(line) && strings.HasSuffix(line, ":") {
				end = len(line) - 1
			} else if strings.HasPrefix(line, "  /") {
				start, end = 2, len(line)
			}
		}
		if end > start {
			references.Paths = append(references.Paths, PathReference{Start: offset + start, End: offset + end, Path: line[start:end]})
		}
		offset += len(line) + 1
	}
	encoded, err := json.Marshal(references)
	return string(encoded), err
}
