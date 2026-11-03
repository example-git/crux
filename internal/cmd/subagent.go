package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/example-git/crux/internal/subagentscript"
	"github.com/spf13/cobra"
)

func newSubagentCommand() *cobra.Command {
	command := &cobra.Command{
		Use: "subagent", Aliases: []string{"subagents"},
		Short: "Script subagents in the invoking Crux session",
		Long:  "Control subagents using the session environment supplied to Crux shell and script tools. All output is JSON. Session and workspace overrides are not accepted by this interface.",
	}
	command.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		for _, flag := range []string{"cwd", "data-dir", "host", "connection"} {
			if cmd.Flags().Changed(flag) {
				return fmt.Errorf("--%s cannot override the invoking session binding", flag)
			}
		}
		return nil
	}
	for _, action := range []string{"types", "list", "start", "output", "wait", "stop", "continue", "call", "script"} {
		child := &cobra.Command{Use: action, Args: cobra.NoArgs}
		switch action {
		case "types":
			child.Short = "List available subagent presets"
		case "list":
			child.Short = "List this session's subagents"
		case "start":
			child.Use, child.Short, child.Args = "start [prompt...]", "Start a background subagent", cobra.ArbitraryArgs
			child.Flags().String("type", "", "Subagent preset; omitted uses the generic task agent")
			child.Flags().String("prompt-file", "", "Read the prompt from a file, or - for stdin")
		case "output", "wait", "stop":
			child.Use, child.Short, child.Args = action+" <task-id>", action+" a session-owned subagent", cobra.ExactArgs(1)
			if action == "wait" {
				child.Flags().Duration("timeout", 0, "Optional wait limit, from 0 to 10m; omitted waits until terminal")
			}
		case "continue":
			child.Use, child.Short, child.Args = "continue <task-id> [prompt...]", "Continue a terminal subagent transcript", cobra.MinimumNArgs(1)
			child.Flags().String("prompt-file", "", "Read the prompt from a file, or - for stdin")
		case "call":
			child.Short = "Read one JSON API request from stdin"
		case "script":
			child.Use, child.Short, child.Args = "script <file.js|->", "Run a JavaScript orchestration script", cobra.ExactArgs(1)
		}
		child.RunE = func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			client, err := subagentscript.ClientFromEnvironment(os.Getenv)
			if err != nil {
				return err
			}
			defer client.Close()
			encoder := json.NewEncoder(cmd.OutOrStdout())
			if action == "script" {
				source, err := readSubagentInput(cmd, args[0])
				if err != nil {
					return err
				}
				result, runErr := subagentscript.Run(ctx, client, client.SessionID, client.ParentSessionID, string(source))
				if err := encoder.Encode(result); err != nil {
					return err
				}
				return runErr
			}
			request, err := subagentRequest(cmd, action, args)
			if err != nil {
				return err
			}
			result, err := client.Call(ctx, request)
			if err != nil {
				return err
			}
			return encoder.Encode(result)
		}
		command.AddCommand(child)
	}
	return command
}

func readSubagentInput(cmd *cobra.Command, path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(cmd.InOrStdin())
	}
	return os.ReadFile(path)
}

func subagentRequest(cmd *cobra.Command, action string, args []string) (subagentscript.Request, error) {
	request := subagentscript.Request{Action: action}
	if action == "call" {
		data, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return request, err
		}
		if err := subagentscript.Decode(data, &request); err != nil {
			return request, err
		}
		return request, request.Validate()
	}
	if action == "output" || action == "wait" || action == "stop" || action == "continue" {
		request.TaskID, args = args[0], args[1:]
	}
	if action == "start" || action == "continue" {
		file, _ := cmd.Flags().GetString("prompt-file")
		if cmd.Flags().Changed("prompt-file") {
			if file == "" || len(args) != 0 {
				return request, fmt.Errorf("--prompt-file requires a path and cannot be combined with prompt arguments")
			}
			data, err := readSubagentInput(cmd, file)
			if err != nil {
				return request, err
			}
			request.Prompt = string(data)
		} else {
			request.Prompt = strings.Join(args, " ")
		}
	}
	if action == "start" {
		request.SubagentType, _ = cmd.Flags().GetString("type")
	}
	if action == "wait" && cmd.Flags().Changed("timeout") {
		timeout, _ := cmd.Flags().GetDuration("timeout")
		if timeout < 0 || timeout > 10*time.Minute || timeout%time.Millisecond != 0 {
			return request, fmt.Errorf("--timeout must be whole milliseconds between 0 and 10m")
		}
		milliseconds := timeout.Milliseconds()
		request.TimeoutMS = &milliseconds
	}
	return request, request.Validate()
}
