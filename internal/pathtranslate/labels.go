package pathtranslate

import (
	"context"
	"strings"
	"unicode/utf8"
)

func (t *Translator) resolveLabels(ctx context.Context, text string) (string, error) {
	if len(t.labels) == 0 {
		return text, nil
	}
	var output strings.Builder
	for offset := 0; offset < len(text); {
		previous, _ := utf8.DecodeLastRuneInString(text[:offset])
		node := t.labelSpellings
		var matched *rootSpelling
		end := offset
		if offset == 0 || rootBoundary(previous) {
			for index := offset; index < len(text); index++ {
				node = node.children[text[index]]
				if node == nil {
					break
				}
				if node.path != "" && spellingEnd(text, index+1) {
					matched, end = node, index+1
				}
			}
		}
		if matched == nil {
			_, size := utf8.DecodeRuneInString(text[offset:])
			output.WriteString(text[offset : offset+size])
			offset += size
			continue
		}
		output.WriteString(AliasPrefix + matched.path)
		offset = end
	}
	return output.String(), nil
}

func (t *Translator) resolveIdentity(ctx context.Context, alias string) (string, error) {
	if path, ok := t.labelPaths[strings.TrimPrefix(alias, AliasPrefix)]; ok {
		return path, nil
	}
	return t.names.Resolve(ctx, alias)
}
