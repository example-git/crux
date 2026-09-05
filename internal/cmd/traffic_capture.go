package cmd

import (
	"github.com/example-git/crux/internal/trafficcapture"
	"github.com/spf13/cobra"
)

var trafficCaptureWorkerCmd = &cobra.Command{
	Use:    "__traffic-capture-worker CONFIG",
	Hidden: true,
	Args:   cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		return trafficcapture.RunWorker(args[0])
	},
}

var trafficCapturePaneLogCmd = &cobra.Command{
	Use:    "__traffic-capture-pane-log PATH",
	Hidden: true,
	Args:   cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return trafficcapture.WritePaneLog(args[0], cmd.InOrStdin())
	},
}

func init() {
	rootCmd.AddCommand(trafficCaptureWorkerCmd, trafficCapturePaneLogCmd)
}
