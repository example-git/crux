package cmd

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/example-git/crux/internal/ui/demo"
	"github.com/spf13/cobra"
)

func init() {
	var port int
	var project, sessionID string
	command := &cobra.Command{Use: "demo", Short: "Serve the embedded TUI fixture playground", RunE: func(cmd *cobra.Command, args []string) error {
		if sessionID != "" && project == "" {
			project, _ = cmd.Flags().GetString("cwd")
			if project == "" {
				project, _ = os.Getwd()
			}
		}
		handler, err := demo.NewHandlerWithProject(project, sessionID)
		if err != nil {
			return err
		}
		listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			return err
		}
		server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
		fmt.Fprintf(cmd.OutOrStdout(), "Crux TUI demo: http://%s\n", listener.Addr())
		go func() {
			<-cmd.Context().Done()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			server.Shutdown(ctx)
		}()
		err = server.Serve(listener)
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}}
	command.Flags().IntVar(&port, "port", 8767, "Local demo HTTP port")
	command.Flags().StringVar(&project, "project", "", "Project whose .crux/crux.db may be read by the demo")
	command.Flags().StringVarP(&sessionID, "session", "s", "", "Open this stored session on page load")
	rootCmd.AddCommand(command)
}

// IsDemoInvocation lets main avoid opening the real traffic database for fixtures.
func IsDemoInvocation(args []string) bool {
	command, _, err := rootCmd.Find(args)
	return err == nil && command.Name() == "demo"
}
