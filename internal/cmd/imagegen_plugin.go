package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/imagegen"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

var newImagegenRuntime = openImagegenRuntime

func openImagegenRuntime(cmd *cobra.Command) (*imagegen.PluginRuntime, error) {
	cwd, err := ResolveCwd(cmd)
	if err != nil {
		return nil, err
	}
	dataDir, err := cmd.Flags().GetString("data-dir")
	if err != nil {
		return nil, err
	}
	debug, err := cmd.Flags().GetBool("debug")
	if err != nil {
		return nil, err
	}
	store, err := config.LoadIsolated(cwd, dataDir, debug, config.SnapshotEnvironment())
	if err != nil {
		return nil, err
	}
	return imagegen.NewHostPluginRuntime(cmd.Context(), store, imagegen.PluginCredentialBindings{})
}

func runImagegenPlugin(cmd *cobra.Command, mode string) error {
	if cmd.Flags().Changed("host") || cmd.Flags().Changed("connection") {
		return errors.New("image generation command must run on the execution host")
	}
	flags, err := imagegenFlags(cmd)
	if err != nil {
		return err
	}
	if flags.out == "" && flags.outDir == "" {
		return errors.New("exactly one of --out or --out-dir is required")
	}
	if flags.out != "" && flags.outDir != "" {
		return errors.New("--out and --out-dir cannot be used together")
	}
	if flags.out != "" && flags.n != 1 {
		return errors.New("--out requires --n=1; use --out-dir for multiple images")
	}
	backend, err := cmd.Flags().GetString("backend")
	if err != nil {
		return err
	}
	request := imagegen.JobRequest{Mode: mode, Backend: imagegen.Backend(backend), Prompt: flags.prompt, Model: flags.model, Count: flags.n, Quality: flags.quality, Size: flags.size, Background: flags.background, Force: flags.force}
	if mode == imagegen.ModeEdit {
		paths, err := cmd.Flags().GetStringArray("image")
		if err != nil {
			return err
		}
		for _, path := range paths {
			absolute, err := filepath.Abs(path)
			if err != nil {
				return err
			}
			request.InputPaths = append(request.InputPaths, absolute)
		}
	}
	runtime, err := newImagegenRuntime(cmd)
	if err != nil {
		return err
	}
	defer runtime.Manager.Close()
	jobs, err := imagegen.NewJobManagerWithStore("image-command", nil, imagegen.JobManagerOptions{PluginRuntime: runtime})
	if err != nil {
		return err
	}
	defer jobs.StopAll(context.Background())
	request, err = jobs.PrepareRequest(cmd.Context(), request)
	if err != nil {
		return err
	}
	ownership := managedtask.Ownership{ParentSessionID: uuid.NewString()}
	var job managedtask.View
	if flags.out != "" {
		path, pathErr := filepath.Abs(flags.out)
		if pathErr != nil {
			return pathErr
		}
		request.OutputPaths = []string{path}
		job, err = jobs.Enqueue(request, "Image generation", ownership)
	} else {
		path, pathErr := filepath.Abs(flags.outDir)
		if pathErr != nil {
			return pathErr
		}
		job, _, err = jobs.EnqueueNumbered(request, path, "Image generation", ownership)
	}
	if err != nil {
		return err
	}
	for {
		output, err := jobs.Output(cmd.Context(), job.ID, true, time.Minute)
		if err != nil {
			return err
		}
		if output.Output == "" {
			continue
		}
		var result imagegen.JobResult
		if err := json.Unmarshal([]byte(output.Output), &result); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), output.Output); err != nil {
			return err
		}
		if !result.Success {
			return errors.New("image generation failed; see job result")
		}
		return nil
	}
}
