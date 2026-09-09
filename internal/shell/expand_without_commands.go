package shell

import (
	"context"
	"errors"
	"io"
	"strings"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

// ExpandValueWithoutCommands uses ExpandValue's single-word semantics with an
// explicit environment and nounset policy. It rejects command substitutions
// throughout the parsed word, including unused parameter-expansion branches.
// It performs no command, directory, process-environment, or filesystem lookup.
// Errors deliberately omit the template, environment, and expansion result.
func ExpandValueWithoutCommands(ctx context.Context, value string, environment []string, strict bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !strings.ContainsAny(value, "$`\\'\"") {
		return value, nil
	}
	word, err := syntax.NewParser().Document(strings.NewReader(value))
	if err != nil {
		return "", errors.New("value expansion could not be parsed")
	}
	command := false
	syntax.Walk(word, func(node syntax.Node) bool {
		switch node.(type) {
		case *syntax.CmdSubst, *syntax.ProcSubst:
			command = true
		}
		return !command
	})
	if command {
		return "", errors.New("value expansion requires command substitution")
	}
	result, err := expand.Document(&expand.Config{
		Env: expand.ListEnviron(environment...), NoUnset: strict,
		CmdSubst: func(io.Writer, *syntax.CmdSubst) error {
			return errors.New("command substitution is unavailable")
		},
	}, word)
	if err != nil {
		return "", errors.New("value expansion could not be resolved")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return result, nil
}
