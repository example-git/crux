package shell

import (
	"context"
	"crypto/rand"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

func prepareNativeDirectoryReporting(ctx context.Context, command *exec.Cmd) (func() error, error) {
	finish := func() error { return nil }
	reporter, ok := accessFilter(ctx).(interface {
		NativeDirectoryReport() (string, func() error, error)
	})
	if !ok {
		return finish, nil
	}
	if len(command.Args) == 0 {
		return finish, nil
	}
	arguments := append([]string(nil), command.Args...)
	arguments[0] = command.Path
	commandIndex := nativeShellCommandIndex(arguments)
	if commandIndex < 0 {
		return finish, nil
	}
	path, finish, err := reporter.NativeDirectoryReport()
	if err != nil || path == "" {
		return finish, err
	}
	quoted, err := syntax.Quote(path, syntax.LangBash)
	if err != nil {
		_ = finish()
		return nil, err
	}
	command.Args = append([]string(nil), command.Args...)
	bootstrap := strings.ReplaceAll(nativeDirectoryBootstrap, "__crux_", "__crux_"+rand.Text()+"_")
	bootstrap = strings.ReplaceAll(bootstrap, "__CRUX_DIRECTORY_JOURNAL__", quoted)
	command.Args[commandIndex] = bootstrap + "\n" + command.Args[commandIndex]
	return finish, nil
}

func nativeShellCommandIndex(args []string) int {
	if len(args) == 0 {
		return -1
	}
	switch filepath.Base(args[0]) {
	case "env", "sudo", "command", "exec":
		wrapper := filepath.Base(args[0])
		options := true
		for index := 1; index < len(args); index++ {
			argument := args[index]
			if options && argument == "--" {
				options = false
				continue
			}
			if options && strings.HasPrefix(argument, "-") {
				switch argument {
				case "-S", "--split-string":
					if wrapper == "env" {
						return -1
					}
				case "-u", "-g", "-h", "-p", "-r", "-t", "-C", "-D", "-R", "-T", "--unset", "--chdir", "--user", "--group", "--host", "--prompt", "--role", "--type", "--close-from", "--chroot", "--command-timeout":
					index++
				}
				continue
			}
			if strings.Contains(argument, "=") {
				continue
			}
			if nested := nativeShellCommandIndex(args[index:]); nested >= 0 {
				return index + nested
			}
			return -1
		}
		return -1
	case "bash", "sh", "zsh":
	default:
		return -1
	}
	for index := 1; index < len(args); index++ {
		argument := args[index]
		if argument == "--" || !strings.HasPrefix(argument, "-") && !strings.HasPrefix(argument, "+") {
			return -1
		}
		switch argument {
		case "-o", "+o", "-O", "+O", "--rcfile", "--init-file":
			index++
			continue
		}
		if strings.HasPrefix(argument, "--") {
			continue
		}
		if strings.HasPrefix(argument, "-") {
			for flag := 1; flag < len(argument); flag++ {
				if argument[flag] == 'o' || argument[flag] == 'O' {
					if flag == len(argument)-1 {
						index++
					}
					break
				}
				if argument[flag] == 'c' && index+1 < len(args) {
					return index + 1
				}
			}
		}
	}
	return -1
}

func rejectNestedOutputRedirects(args []string) error {
	index := nativeShellCommandIndex(args)
	if index < 0 {
		return nil
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(args[index]), "")
	if err != nil {
		return fmt.Errorf("cannot inspect nested shell command for output redirection; execute the command directly in the tool shell: %w", err)
	}
	var rejected error
	syntax.Walk(file, func(node syntax.Node) bool {
		if rejected != nil {
			return false
		}
		if redirect, ok := node.(*syntax.Redirect); ok {
			op := redirect.Op.String()
			if op == ">&" {
				target := redirect.Word.Lit()
				if target == "-" || target != "" && strings.Trim(target, "0123456789") == "" {
					return true
				}
			}
			switch op {
			case ">", ">>", ">|", "&>", "&>>", "<>", ">&":
				rejected = fmt.Errorf("output redirection inside a nested shell is not allowed for subagent commands; move the redirection outside the shell command so task stdout is retained (for example: bash -c 'command' > output.log)")
				return false
			}
		}
		return true
	})
	return rejected
}

func ReadOutputPaths(ctx context.Context) ([]string, error) {
	if ctx != nil {
		if source, ok := accessFilter(ctx).(interface{ ReadOutputPaths() ([]string, error) }); ok {
			paths, err := source.ReadOutputPaths()
			if err != nil {
				return nil, fmt.Errorf("read native shell directories: %w", err)
			}
			return paths, nil
		}
	}
	return OutputPaths(ctx), nil
}

const nativeDirectoryBootstrap = `if [ -z "${BASH_VERSION-}${ZSH_VERSION-}" ]; then
    printf '%s\n' 'Crux native directory reporting requires Bash or Zsh' >&2
    exit 125
fi
__crux_report_directory() {
    builtin local __crux_report_status=$?
    builtin local __crux_directory=${PWD-}
    case "$__crux_directory" in /*) ;; *) __crux_directory= ;; esac
    if [ -z "$__crux_directory" ] || ! builtin test . -ef "$__crux_directory"; then
        if ! __crux_directory=$(builtin pwd -P && builtin printf '.'); then
            builtin printf '%s\n' 'Crux could not resolve the native shell working directory' >&2
            exit 125
        fi
        __crux_directory=${__crux_directory%.}
        __crux_directory=${__crux_directory%$'\n'}
    fi
    if [ "${__crux_reported_directory-}" != "$__crux_directory" ]; then
        if ! builtin printf '%s\000' "$__crux_directory" >> __CRUX_DIRECTORY_JOURNAL__; then
            builtin printf '%s\n' 'Crux could not record the native shell working directory' >&2
            exit 125
        fi
        __crux_reported_directory=$__crux_directory
    fi
    return "$__crux_report_status"
}
if [ -n "${BASH_VERSION-}" ]; then
    __crux_debug_declaration=$(builtin trap -p DEBUG)
    __crux_previous_debug=:
    if [ -n "$__crux_debug_declaration" ]; then
        __crux_debug_declaration=${__crux_debug_declaration#trap -- }
        __crux_debug_declaration=${__crux_debug_declaration% DEBUG}
        builtin eval "__crux_previous_debug=$__crux_debug_declaration"
    fi
    builtin trap 'if __crux_report_directory; then
'"$__crux_previous_debug"'
else
'"$__crux_previous_debug"'
fi' DEBUG
    builtin set -T
elif [ -n "${ZSH_VERSION-}" ]; then
    if (( $+functions[TRAPDEBUG] )); then
        builtin functions -c TRAPDEBUG __crux_previous_debug_function
        TRAPDEBUG() {
            if __crux_report_directory; then
                __crux_previous_debug_function "$@"
            else
                __crux_previous_debug_function "$@"
            fi
        }
    else
        __crux_saved_traps=$(builtin trap)
        __crux_previous_debug=$(
            builtin unalias trap 2>/dev/null || :
            trap() {
                if [ "$1" = -- ]; then shift; fi
                __crux_trap_body=$1
                shift
                for __crux_trap_signal; do
                    case "$__crux_trap_signal" in
                        DEBUG|SIGDEBUG) builtin printf '%s' "$__crux_trap_body" ;;
                    esac
                done
            }
            builtin eval "$__crux_saved_traps"
        )
        builtin trap 'if __crux_report_directory; then
'"${__crux_previous_debug:-:}"'
else
'"${__crux_previous_debug:-:}"'
fi' DEBUG
    fi
fi
if __crux_report_directory; then :; else :; fi
`
