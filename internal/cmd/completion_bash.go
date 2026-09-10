package cmd

import (
	"fmt"
	"io"
)

// Cobra's minimal Bash initializer still requires _get_comp_words_by_ref
// from bash-completion. Override just that initializer so direct eval also
// works in a stock Bash shell, without defining helpers for other commands.
func writeStandaloneBashCompletion(out io.Writer, name string) error {
	_, err := fmt.Fprintf(out, `
__%[1]s_init_completion()
{
    COMPREPLY=()
    if declare -F _get_comp_words_by_ref >/dev/null 2>&1; then
        _get_comp_words_by_ref "$@" cur prev words cword
        return
    fi

    # Bash splits '=' and ':' into separate completion words. Rejoin them
    # for Cobra, including --flag=value and tcp://host:port arguments.
    local i index=-1 join=0 word
    words=()
    for ((i=0; i<=COMP_CWORD; i++)); do
        word=${COMP_WORDS[i]}
        if ((index >= 0)) && [[ -n $word && -z ${word//[=:]/} ]]; then
            words[index]+=$word
            join=1
        elif ((join)); then
            words[index]+=$word
            join=0
        else
            words+=("$word")
            ((index++))
        fi
    done
    cword=$index
    cur=${words[cword]}
    prev=
    if ((cword > 0)); then
        prev=${words[cword-1]}
    fi
    return 0
}
`, name)
	return err
}
