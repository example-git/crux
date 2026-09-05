package shell

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/itchyny/gojq"
	"mvdan.cc/sh/v3/interp"
)

const jqUsage = `jq - Go implementation of jq (gojq 0.12.19 builtin)

Synopsis:
  %% echo '{"foo": 128}' | jq '.foo'

Usage:
  jq [OPTIONS] [FILTER] [FILE...]

Options:
  -r, --raw-output              output raw strings
  -j, --join-output             implies -r with no newline delimiter
  -c, --compact-output          output without pretty-printing
  -s, --slurp                   read all inputs into an array
  -n, --null-input              use null as input value
  -e, --exit-status             exit 1 when the last value is false or null
  -R, --raw-input               read input as raw strings
      --arg name value          set a string value to a variable
      --argjson name value      set a JSON value to a variable
  -h, --help                    display this help
`

type JQVariable struct {
	Name  string
	Value any
}

type JQOptions struct {
	Filter        string
	Files         []string
	RawOutput     bool
	JoinOutput    bool
	CompactOutput bool
	Slurp         bool
	NullInput     bool
	ExitStatus    bool
	RawInput      bool
	Variables     []JQVariable
	Environment   []string
}

// handleJQ implements the jq builtin using gojq. It supports a subset of jq
// flags: -r (raw output), -c (compact output), -s (slurp), -n (null input),
// -e (exit status), -R (raw input), and --arg name value.
//
// ctx is polled at each iteration of the output loop and at each reader in
// [readInputs] so that hook timeouts or other cancellations can interrupt
// long-running queries. A cancelled context surfaces as ctx.Err(), not an
// [interp.ExitStatus], so callers (e.g. the hook runner) can distinguish
// "filter exited non-zero" from "we ran out of time".
//
// Note that this is somewhat of a reimplmentation of the CLI of the glorious
// github.com/itchyny/gojq, and we'd ideally get the CLI exposed upstream to
// avoid this falling out of sync.
func handleJQ(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	var options JQOptions
	var querySet bool
	i := 1
	for i < len(args) {
		arg := args[i]
		switch {
		case arg == "-h" || arg == "--help":
			fmt.Fprint(stdout, jqUsage)
			return nil
		case arg == "-r" || arg == "--raw-output":
			options.RawOutput = true
		case arg == "-j" || arg == "--join-output":
			options.JoinOutput = true
			options.RawOutput = true
		case arg == "-c" || arg == "--compact-output":
			options.CompactOutput = true
		case arg == "-s" || arg == "--slurp":
			options.Slurp = true
		case arg == "-n" || arg == "--null-input":
			options.NullInput = true
		case arg == "-e" || arg == "--exit-status":
			options.ExitStatus = true
		case arg == "-R" || arg == "--raw-input":
			options.RawInput = true
		case arg == "--arg":
			if i+2 >= len(args) {
				fmt.Fprintln(stderr, "jq: --arg requires name and value")
				return interp.ExitStatus(2)
			}
			options.Variables = append(options.Variables, JQVariable{Name: args[i+1], Value: args[i+2]})
			i += 2
		case arg == "--argjson":
			if i+2 >= len(args) {
				fmt.Fprintln(stderr, "jq: --argjson requires name and value")
				return interp.ExitStatus(2)
			}
			var value any
			if err := json.Unmarshal([]byte(args[i+2]), &value); err != nil {
				fmt.Fprintf(stderr, "jq: invalid JSON for --argjson %s: %s\n", args[i+1], err)
				return interp.ExitStatus(2)
			}
			options.Variables = append(options.Variables, JQVariable{Name: args[i+1], Value: value})
			i += 2
		case arg == "--":
			i++
			options.Files = append(options.Files, args[i:]...)
			i = len(args)
			continue
		case strings.HasPrefix(arg, "-") && querySet:
			fmt.Fprintf(stderr, "jq: unknown option: %s\n", arg)
			return interp.ExitStatus(2)
		default:
			if !querySet {
				options.Filter = arg
				querySet = true
			} else {
				options.Files = append(options.Files, arg)
			}
		}
		i++
	}

	return RunJQ(ctx, options, stdin, stdout, stderr)
}

func RunJQ(ctx context.Context, options JQOptions, stdin io.Reader, stdout, stderr io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	filter := options.Filter
	if filter == "" {
		filter = "."
	}
	query, err := gojq.Parse(filter)
	if err != nil {
		fmt.Fprintf(stderr, "jq: %s\n", err)
		return interp.ExitStatus(3)
	}

	argNames := make([]string, 0, len(options.Variables))
	argValues := make([]any, 0, len(options.Variables))
	for _, variable := range options.Variables {
		argNames = append(argNames, "$"+variable.Name)
		argValues = append(argValues, variable.Value)
	}
	environment := options.Environment
	if environment == nil {
		environment = os.Environ()
	}
	compilerOptions := []gojq.CompilerOption{
		gojq.WithEnvironLoader(func() []string {
			return append([]string(nil), environment...)
		}),
	}
	if len(argNames) > 0 {
		compilerOptions = append(compilerOptions, gojq.WithVariables(argNames))
	}
	code, err := gojq.Compile(query, compilerOptions...)
	if err != nil {
		fmt.Fprintf(stderr, "jq: %s\n", err)
		return interp.ExitStatus(3)
	}

	inputs, err := readInputs(ctx, stdin, options.Files, options.NullInput, options.RawInput, options.Slurp)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		fmt.Fprintf(stderr, "jq: %s\n", err)
		return interp.ExitStatus(2)
	}

	rawOutput := options.RawOutput || options.JoinOutput
	var lastFalsy bool
	for _, input := range inputs {
		iter := code.Run(input, argValues...)
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			value, ok := iter.Next()
			if !ok {
				break
			}
			if runErr, ok := value.(error); ok {
				fmt.Fprintf(stderr, "jq: %s\n", runErr)
				return interp.ExitStatus(5)
			}
			if options.ExitStatus {
				lastFalsy = value == nil || value == false
			}
			if err := writeValue(stdout, value, rawOutput, options.CompactOutput, options.JoinOutput); err != nil {
				return err
			}
		}
	}
	if options.ExitStatus && lastFalsy {
		return interp.ExitStatus(1)
	}
	return nil
}

// readInputs reads JSON (or raw) input values from stdin or files.
//
// ctx is polled in three places so that a cancellation observed mid-read
// short-circuits promptly:
//   - between readers (before opening the next file / consuming stdin);
//   - on every io.Read call via ctxReader, so io.ReadAll on a large but
//     non-blocking source (e.g. the bytes.NewReader payload the hook
//     runner supplies) returns ctx.Err() on the next chunk boundary;
//   - inside the post-read value accumulation loops (raw-input line
//     split and JSON stream decode), which are otherwise unbounded in
//     the size of the input.
//
// A reader that blocks forever in Read (e.g. an unterminated pipe) can
// still outlast ctx; the outer abandon-goroutine path in the hook
// runner (internal/hooks/runner.go) is the authoritative enforcer for
// that case.
func readInputs(ctx context.Context, stdin io.Reader, files []string, nullInput, rawInput, slurp bool) ([]any, error) {
	if nullInput {
		return []any{nil}, nil
	}

	var readers []io.Reader
	if len(files) > 0 {
		for _, f := range files {
			file, err := os.Open(f)
			if err != nil {
				return nil, err
			}
			defer file.Close()
			readers = append(readers, file)
		}
	} else {
		readers = []io.Reader{stdin}
	}

	var vals []any
	for _, r := range readers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, err := io.ReadAll(ctxReader{ctx: ctx, r: r})
		if err != nil {
			// ctxReader surfaces ctx.Err() verbatim; preserve it so the
			// caller can distinguish cancellation from a parse error.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, err
		}

		if rawInput {
			lines := strings.Split(string(data), "\n")
			if slurp {
				vals = append(vals, strings.Join(lines, "\n"))
			} else {
				for _, line := range lines {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
					if line != "" || !slurp {
						vals = append(vals, line)
					}
				}
			}
			continue
		}

		// Decode potentially multiple JSON values from the stream.
		dec := json.NewDecoder(strings.NewReader(string(data)))
		var streamVals []any
		for {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			var v any
			if err := dec.Decode(&v); err != nil {
				if err == io.EOF {
					break
				}
				return nil, fmt.Errorf("parse error: %w", err)
			}
			streamVals = append(streamVals, v)
		}

		if slurp {
			vals = append(vals, streamVals)
		} else {
			vals = append(vals, streamVals...)
		}
	}

	if len(vals) == 0 {
		return []any{nil}, nil
	}
	return vals, nil
}

// ctxReader wraps an io.Reader so that each Read call checks ctx first.
// This makes io.ReadAll over a large but non-blocking source (e.g. a
// bytes.Reader of the hook stdin payload) cancellable on the next chunk
// boundary. A reader that itself blocks in Read will still outlast ctx —
// the hook runner's abandon-goroutine path is the enforcer of last resort
// for that case.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (cr ctxReader) Read(p []byte) (int, error) {
	if err := cr.ctx.Err(); err != nil {
		return 0, err
	}
	return cr.r.Read(p)
}

// writeValue writes a single jq output value.
func writeValue(w io.Writer, v any, raw, compact, join bool) error {
	if raw {
		if s, ok := v.(string); ok {
			if _, err := fmt.Fprint(w, s); err != nil {
				return err
			}
			if !join {
				_, err := fmt.Fprint(w, "\n")
				return err
			}
			return nil
		}
	}

	var bs []byte
	var err error
	if compact {
		bs, err = gojq.Marshal(v)
	} else {
		bs, err = json.MarshalIndent(v, "", "  ")
	}
	if err != nil {
		return err
	}
	if _, writeErr := w.Write(bs); writeErr != nil {
		return writeErr
	}
	_, err = fmt.Fprint(w, "\n")
	return err
}
