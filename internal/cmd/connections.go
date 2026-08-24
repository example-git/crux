package cmd

import (
	"fmt"

	"github.com/example-git/crux/internal/connection"
	"github.com/spf13/cobra"
)

var connectionsCmd = &cobra.Command{
	Use:   "connections",
	Short: "Manage authenticated network connections",
	RunE: func(cmd *cobra.Command, _ []string) error {
		items, err := connection.List(cmd.Context())
		if err != nil {
			return err
		}
		if len(items) == 0 {
			fmt.Println("No saved connections.")
			return nil
		}
		for _, item := range items {
			fmt.Printf("%s\t%s\n", item.Name, item.Address)
		}
		return nil
	},
}

var connectionsServerInitCmd = &cobra.Command{
	Use:   "server-init",
	Short: "Create the server identity and print its public pairing code",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		code, err := connection.EnsureServerIdentity(cmd.Context())
		if err != nil {
			return err
		}
		fmt.Println(code)
		return nil
	},
}

var connectionsAddCmd = &cobra.Command{
	Use:   "add <name> <tcp://host:port> <server-pairing-code>",
	Short: "Save a server and create a client identity",
	Args:  cobra.ExactArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		_, clientCode, err := connection.Add(cmd.Context(), args[0], args[1], args[2])
		if err != nil {
			return err
		}
		fmt.Printf("Saved connection %s. Give this public client pairing code to the server owner:\n%s\n", args[0], clientCode)
		return nil
	},
}

var connectionsAuthorizeCmd = &cobra.Command{
	Use:   "authorize <name> <client-pairing-code>",
	Short: "Authorize a client public key on this server",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connection.AuthorizeClient(cmd.Context(), args[0], args[1]); err != nil {
			return err
		}
		fmt.Printf("Authorized client %s.\n", args[0])
		return nil
	},
}

func init() {
	connectionsCmd.AddCommand(
		connectionsServerInitCmd,
		connectionsAddCmd,
		connectionsAuthorizeCmd,
	)
	rootCmd.AddCommand(connectionsCmd)
}
