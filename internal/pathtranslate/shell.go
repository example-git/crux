package pathtranslate

import (
	"context"
	"strings"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

func shellLiteral(value string) ([]syntax.WordPart, error) {
	quoted, err := syntax.Quote(value, syntax.LangBash)
	if err != nil {
		return nil, err
	}
	file, err := syntax.NewParser().Parse(strings.NewReader("printf "+quoted), "")
	if err != nil {
		return nil, err
	}
	return file.Stmts[0].Cmd.(*syntax.CallExpr).Args[1].Parts, nil
}

func (t *Translator) ResolveShell(ctx context.Context, command string) (string, error) {
	if !strings.Contains(command, AliasPrefix) && len(t.labels) == 0 {
		return command, nil
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return "", err
	}
	var translateParts func([]syntax.WordPart, bool) ([]syntax.WordPart, error)
	translateParts = func(parts []syntax.WordPart, quoted bool) ([]syntax.WordPart, error) {
		var result []syntax.WordPart
		for _, part := range parts {
			switch value := part.(type) {
			case *syntax.DblQuoted:
				value.Parts, err = translateParts(value.Parts, true)
				if err != nil {
					return nil, err
				}
			case *syntax.SglQuoted:
				if !strings.Contains(value.Value, AliasPrefix) && len(t.labels) == 0 {
					break
				}
				literal, err := expand.Literal(nil, &syntax.Word{Parts: []syntax.WordPart{value}})
				if err != nil {
					return nil, err
				}
				resolved, err := t.ResolveText(ctx, literal)
				if err != nil {
					return nil, err
				}
				replacement, err := shellLiteral(resolved)
				if err != nil {
					return nil, err
				}
				result = append(result, replacement...)
				continue
			case *syntax.Lit:
				prefix, path := "", value.Value
				if before, after, found := strings.Cut(path, "="); found {
					prefix, path = before+"=", after
				}
				components := strings.Split(path, "/")
				lastAlias := -1
				for index, component := range components {
					if IsAlias(component) || t.labels[component] != "" {
						lastAlias = index
					}
				}
				if lastAlias < 0 {
					break
				}
				resolved, err := t.ResolvePath(ctx, strings.Join(components[:lastAlias+1], "/"))
				if err != nil {
					return nil, err
				}
				suffix := ""
				if lastAlias+1 < len(components) {
					suffix = "/" + strings.Join(components[lastAlias+1:], "/")
				}
				if quoted {
					value.Value = prefix + strings.NewReplacer("\\", "\\\\", "$", "\\$", "`", "\\`", "\"", "\\\"").Replace(resolved) + suffix
					break
				}
				if prefix != "" {
					result = append(result, &syntax.Lit{Value: prefix})
				}
				replacement, err := shellLiteral(resolved)
				if err != nil {
					return nil, err
				}
				result = append(result, replacement...)
				if suffix != "" {
					result = append(result, &syntax.Lit{Value: suffix})
				}
				continue
			}
			result = append(result, part)
		}
		return result, nil
	}
	syntax.Walk(file, func(node syntax.Node) bool {
		if err != nil {
			return false
		}
		if word, ok := node.(*syntax.Word); ok {
			word.Parts, err = translateParts(word.Parts, false)
		}
		return err == nil
	})
	if err != nil {
		return "", err
	}
	var output strings.Builder
	if err := syntax.NewPrinter().Print(&output, file); err != nil {
		return "", err
	}
	return output.String(), nil
}
