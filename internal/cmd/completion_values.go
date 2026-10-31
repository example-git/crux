package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/connection"
	"github.com/example-git/crux/internal/db"
	"github.com/example-git/crux/internal/projects"
	"github.com/example-git/crux/internal/server"
	"github.com/example-git/crux/internal/session"
	"github.com/spf13/cobra"
)

func configureDynamicShellCompletions(root *cobra.Command) {
	register := func(command *cobra.Command, name string, complete cobra.CompletionFunc) {
		if command.Flags().Lookup(name) == nil && command.PersistentFlags().Lookup(name) == nil {
			return
		}
		if _, exists := command.GetFlagCompletionFunc(name); !exists {
			_ = command.RegisterFlagCompletionFunc(name, complete)
		}
	}
	register(root, "session", completeSessionFlag)
	register(root, "connection", completeConnections)
	register(root, "cwd", completeProjectDirectories)
	if root.PersistentFlags().Lookup("data-dir") != nil {
		_ = root.MarkPersistentFlagDirname("data-dir")
	}
	for _, command := range root.Commands() {
		switch command.Name() {
		case "run":
			register(command, "session", completeSessionFlag)
		case "session":
			for _, child := range command.Commands() {
				switch child.Name() {
				case "show", "delete", "rename":
					child.ValidArgsFunction = completeSessionArgument
				}
			}
		}
	}
}

func completionFailure(command *cobra.Command, err error) ([]string, cobra.ShellCompDirective) {
	fmt.Fprintln(command.ErrOrStderr(), "completion:", err)
	return nil, cobra.ShellCompDirectiveError | cobra.ShellCompDirectiveNoFileComp
}

func completionDescription(value string) string {
	return strings.Join(strings.FieldsFunc(ansi.Strip(value), func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}), " ")
}

func completeSessionArgument(command *cobra.Command, args []string, prefix string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return completeSessions(command, prefix, false)
}

func completeSessionFlag(command *cobra.Command, _ []string, prefix string) ([]string, cobra.ShellCompDirective) {
	return completeSessions(command, prefix, true)
}

func completeSessions(command *cobra.Command, prefix string, allowRemote bool) ([]string, cobra.ShellCompDirective) {
	ctx, cancel := context.WithTimeout(command.Context(), 3*time.Second)
	defer cancel()
	name, _ := command.Flags().GetString("connection")
	host, _ := command.Flags().GetString("host")
	if allowRemote && (name != "" || command.Flags().Changed("host")) {
		values, err := remoteSessionCompletions(ctx, command, name, host, prefix)
		if err != nil {
			return completionFailure(command, err)
		}
		return values, cobra.ShellCompDirectiveNoFileComp
	}
	cwd, _ := command.Flags().GetString("cwd")
	if cwd == "" {
		cwd = "."
	}
	cwd, err := filepath.Abs(cwd)
	if err != nil {
		return completionFailure(command, err)
	}
	info, err := os.Stat(cwd)
	if err != nil {
		return completionFailure(command, err)
	}
	if !info.IsDir() {
		return completionFailure(command, fmt.Errorf("working directory is not a directory: %s", cwd))
	}
	dataDir, _ := command.Flags().GetString("data-dir")
	dataDir, err = config.CompletionDataDirectory(ctx, cwd, dataDir)
	if err != nil {
		return completionFailure(command, err)
	}
	path := filepath.Join(dataDir, "crux.db")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, cobra.ShellCompDirectiveNoFileComp
	} else if err != nil {
		return completionFailure(command, err)
	}
	conn, err := db.ConnectReadOnly(ctx, path)
	if err != nil {
		return completionFailure(command, err)
	}
	defer conn.Close()
	items, err := session.NewService(db.New(conn), conn).List(ctx)
	if err != nil {
		return completionFailure(command, err)
	}
	var values []string
	for _, item := range items {
		values = appendSessionCompletion(values, item.ID, item.Title, prefix)
	}
	return values, cobra.ShellCompDirectiveNoFileComp
}

func appendSessionCompletion(values []string, id, title, prefix string) []string {
	value := id
	if !strings.HasPrefix(value, prefix) {
		value = session.HashID(id)
		if !strings.HasPrefix(value, prefix) {
			return values
		}
	}
	return append(values, value+"\t"+completionDescription(title))
}

func remoteSessionCompletions(ctx context.Context, command *cobra.Command, name, host, prefix string) ([]string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	var remote *client.Client
	if name != "" {
		saved, exists, err := connection.Get(ctx, name)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, fmt.Errorf("saved connection not found: %s", name)
		}
		remote, err = client.NewAuthenticatedClient(cwd, saved)
		if err != nil {
			return nil, err
		}
	} else {
		address, err := server.ParseHostURL(host)
		if err != nil {
			return nil, err
		}
		if address.Scheme == "tcp" && !server.IsLoopbackHost(address) {
			return nil, fmt.Errorf("remote TCP servers require a saved authenticated connection")
		}
		remote, err = client.NewClient(cwd, address.Scheme, address.Host)
		if err != nil {
			return nil, err
		}
	}
	workspaces, err := remote.ListWorkspaces(ctx)
	if err != nil {
		return nil, err
	}
	path, _ := command.Flags().GetString("cwd")
	if path == "" {
		path = cwd
	}
	dataDir, _ := command.Flags().GetString("data-dir")
	for _, workspace := range workspaces {
		if workspace.Path != path || (dataDir != "" && workspace.DataDir != dataDir) {
			continue
		}
		items, err := remote.ListSessions(ctx, workspace.ID)
		if err != nil {
			return nil, err
		}
		var values []string
		for _, item := range items {
			values = appendSessionCompletion(values, item.ID, item.Title, prefix)
		}
		return values, nil
	}
	return nil, fmt.Errorf("no existing server workspace for %q; completion does not create workspaces", path)
}

func completeConnections(command *cobra.Command, _ []string, prefix string) ([]string, cobra.ShellCompDirective) {
	ctx, cancel := context.WithTimeout(command.Context(), 3*time.Second)
	defer cancel()
	items, err := connection.List(ctx)
	if err != nil {
		return completionFailure(command, err)
	}
	var values []string
	for _, item := range items {
		if strings.HasPrefix(item.Name, prefix) {
			values = append(values, item.Name+"\t"+completionDescription(item.Address))
		}
	}
	return values, cobra.ShellCompDirectiveNoFileComp
}

func completeProjectDirectories(command *cobra.Command, _ []string, prefix string) ([]string, cobra.ShellCompDirective) {
	name, _ := command.Flags().GetString("connection")
	if name != "" {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	items, err := projects.List()
	if err != nil {
		return completionFailure(command, err)
	}
	var values []string
	for _, item := range items {
		if strings.HasPrefix(item.Path, prefix) {
			values = append(values, item.Path+"\tKnown project")
		}
	}
	if len(values) == 0 {
		return nil, cobra.ShellCompDirectiveFilterDirs
	}
	return values, cobra.ShellCompDirectiveNoFileComp
}
