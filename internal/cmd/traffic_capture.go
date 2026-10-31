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

var trafficCaptureProcessCmd = &cobra.Command{
	Use:                "__traffic-capture-process EXECUTABLE ARGV...",
	Hidden:             true,
	DisableFlagParsing: true,
	Args:               cobra.MinimumNArgs(2),
	RunE: func(_ *cobra.Command, args []string) error {
		return trafficcapture.RunCapturedProcess(args)
	},
}

var trafficCaptureControlCmd = &cobra.Command{
	Use:                "__traffic-capture-control COMMAND [ARGS]",
	Hidden:             true,
	DisableFlagParsing: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return trafficcapture.RunCaptureControl(cmd.Context(), args, cmd.OutOrStdout())
	},
}

var trafficCaptureManagerCmd = &cobra.Command{
	Use:    "__traffic-capture-manager CONFIG",
	Hidden: true,
	Args:   cobra.ExactArgs(1),
	RunE:   func(_ *cobra.Command, args []string) error { return trafficcapture.RunCaptureManager(args[0]) },
}

func init() {
	rootCmd.AddCommand(trafficCaptureWorkerCmd, trafficCapturePaneLogCmd, trafficCaptureProcessCmd, trafficCaptureControlCmd, trafficCaptureManagerCmd)
}
