package cmd

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/example-git/crux/internal/imagegen"
	"github.com/spf13/cobra"
)

var imagegenCmd = &cobra.Command{
	Use:    "imagegen",
	Short:  "Generate or edit images",
	Hidden: true,
	Long: `Generate or edit images through an exact installed and trusted image-provider plugin.
Omitted backend selection uses configured images.preferred. This command runs on
the execution host and does not forward private bundles or browser credentials.`,
}

var imagegenGenerateCmd = &cobra.Command{
	Use:   "generate",
	Short: "Generate a new image from a prompt",
	RunE:  runImagegenGenerate,
}

var imagegenEditCmd = &cobra.Command{
	Use:   "edit",
	Short: "Create an image using one or more edit or reference images",
	RunE:  runImagegenEdit,
}

func init() {
	for _, c := range []*cobra.Command{imagegenGenerateCmd, imagegenEditCmd} {
		c.Flags().String("prompt", "", "Prompt describing the desired image (required)")
		c.Flags().String("backend", "auto", "Installed image backend ID or configured auto selection")
		c.Flags().String("model", "", "Image model (default: selected plugin default)")
		c.Flags().Int("n", 1, "Number of images to generate (1-10)")
		c.Flags().String("quality", "", "Quality supported by the selected image plugin")
		c.Flags().String("size", "", "Size: auto or WIDTHxHEIGHT within the documented bounds")
		c.Flags().String("background", "", "Background supported by the selected image plugin")
		c.Flags().StringP("out", "o", "", "Output file path (requires n=1)")
		c.Flags().String("out-dir", "", "Output directory with collision-free plugin-format numbered files")
		c.Flags().BoolP("force", "f", false, "Explicitly allow existing output files to be overwritten")
		_ = c.MarkFlagRequired("prompt")
	}
	imagegenEditCmd.Flags().StringArray("image", nil, "Input edit or reference image path (repeatable; order is meaningful)")
	_ = imagegenEditCmd.MarkFlagRequired("image")

	imagegenCmd.AddCommand(imagegenGenerateCmd, imagegenEditCmd)
	rootCmd.AddCommand(imagegenCmd)
}

func runImagegenGenerate(cmd *cobra.Command, _ []string) error {
	return runImagegenPlugin(cmd, imagegen.ModeGenerate)
}

func runImagegenEdit(cmd *cobra.Command, _ []string) error {
	return runImagegenPlugin(cmd, imagegen.ModeEdit)
}

type commonImagegenFlags struct {
	prompt     string
	model      string
	n          int
	quality    imagegen.Quality
	size       string
	background imagegen.Background
	out        string
	outDir     string
	force      bool
}

func imagegenFlags(cmd *cobra.Command) (commonImagegenFlags, error) {
	var flags commonImagegenFlags
	var err error
	flags.prompt, err = cmd.Flags().GetString("prompt")
	if err != nil {
		return flags, err
	}
	flags.model, err = cmd.Flags().GetString("model")
	if err != nil {
		return flags, err
	}
	flags.n, err = cmd.Flags().GetInt("n")
	if err != nil {
		return flags, err
	}
	quality, err := cmd.Flags().GetString("quality")
	if err != nil {
		return flags, err
	}
	flags.quality = imagegen.Quality(quality)
	flags.size, err = cmd.Flags().GetString("size")
	if err != nil {
		return flags, err
	}
	background, err := cmd.Flags().GetString("background")
	if err != nil {
		return flags, err
	}
	flags.background = imagegen.Background(background)
	flags.out, err = cmd.Flags().GetString("out")
	if err != nil {
		return flags, err
	}
	flags.outDir, err = cmd.Flags().GetString("out-dir")
	if err != nil {
		return flags, err
	}
	flags.force, err = cmd.Flags().GetBool("force")
	return flags, err
}

type imageOutputs struct {
	paths    []string
	reserved []*os.File
	force    bool
	finished bool
}

func prepareImageOutputs(out, outDir string, n int, force bool) (*imageOutputs, error) {
	if out == "" && outDir == "" {
		return nil, errors.New("exactly one of --out or --out-dir is required")
	}
	if out != "" && outDir != "" {
		return nil, errors.New("--out and --out-dir cannot be used together")
	}
	if out != "" && n != 1 {
		return nil, errors.New("--out requires --n=1; use --out-dir for multiple images")
	}

	plan := &imageOutputs{force: force}
	if out != "" {
		resolved, err := filepath.Abs(out)
		if err != nil {
			return nil, fmt.Errorf("resolve output path %q: %w", out, err)
		}
		plan.paths = []string{resolved}
	} else {
		resolvedDir, err := filepath.Abs(outDir)
		if err != nil {
			return nil, fmt.Errorf("resolve output directory %q: %w", outDir, err)
		}
		if info, err := os.Stat(resolvedDir); err == nil && !info.IsDir() {
			return nil, fmt.Errorf("output directory %q is not a directory", resolvedDir)
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("inspect output directory %q: %w", resolvedDir, err)
		}
		for i := 0; i < n; i++ {
			plan.paths = append(plan.paths, filepath.Join(resolvedDir, fmt.Sprintf("image_%d.png", i+1)))
		}
	}

	for _, path := range plan.paths {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("create output directory %q: %w", filepath.Dir(path), err)
		}
		info, err := os.Stat(path)
		switch {
		case err == nil && info.IsDir():
			return nil, fmt.Errorf("output path %q is a directory", path)
		case err == nil && !force:
			return nil, fmt.Errorf("output already exists: %s (use --force to overwrite)", path)
		case err != nil && !errors.Is(err, os.ErrNotExist):
			return nil, fmt.Errorf("inspect output path %q: %w", path, err)
		}
	}

	if force {
		return plan, nil
	}
	for _, path := range plan.paths {
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			plan.abort()
			if errors.Is(err, os.ErrExist) {
				return nil, fmt.Errorf("output already exists: %s (use --force to overwrite)", path)
			}
			return nil, fmt.Errorf("reserve output path %q: %w", path, err)
		}
		plan.reserved = append(plan.reserved, file)
	}
	return plan, nil
}

func (o *imageOutputs) abort() {
	if o == nil || o.finished {
		return
	}
	for i, file := range o.reserved {
		_ = file.Close()
		_ = os.Remove(o.paths[i])
	}
	o.finished = true
}

func (o *imageOutputs) write(response *imagegen.Response) error {
	if response == nil {
		return errors.New("image API returned no response")
	}
	if response.AuthMode == imagegen.AuthAPIKey {
		fmt.Fprintln(os.Stderr, "Authenticated with OPENAI_API_KEY (no usable Codex account)")
	} else {
		fmt.Fprintln(os.Stderr, "Authenticated with the signed-in Codex account")
	}

	succeeded := make([]bool, len(o.paths))
	failures := append([]imagegen.ImageVariantFailure(nil), response.Failures...)
	for index, image := range response.Data {
		variant := image.Variant
		if variant == 0 {
			variant = index + 1
		}
		if variant < 1 || variant > len(o.paths) || succeeded[variant-1] {
			failures = append(failures, imagegen.ImageVariantFailure{Variant: variant, Error: "image response reported an invalid variant"})
			continue
		}
		data, err := base64.StdEncoding.DecodeString(image.B64JSON)
		if err != nil {
			failures = append(failures, imagegen.ImageVariantFailure{Variant: variant, Error: fmt.Sprintf("decode image data: %v", err)})
			continue
		}
		path := o.paths[variant-1]
		if o.force {
			err = os.WriteFile(path, data, 0o644)
		} else {
			_, err = o.reserved[variant-1].Write(data)
			closeErr := o.reserved[variant-1].Close()
			if err == nil {
				err = closeErr
			}
		}
		if err != nil {
			failures = append(failures, imagegen.ImageVariantFailure{Variant: variant, Error: err.Error()})
			if !o.force {
				_ = os.Remove(path)
			}
			continue
		}
		succeeded[variant-1] = true
		fmt.Println(path)
	}

	successCount := 0
	for index, success := range succeeded {
		if success {
			successCount++
			continue
		}
		if !o.force {
			_ = o.reserved[index].Close()
			_ = os.Remove(o.paths[index])
		}
	}
	if successCount == 0 {
		return errors.New("no image variants completed")
	}
	for _, failure := range failures {
		fmt.Fprintf(os.Stderr, "Image variant %d failed: %s\n", failure.Variant, strings.TrimSpace(failure.Error))
	}
	o.finished = true
	return nil
}

func imagegenError(err error) error {
	if errors.Is(err, imagegen.ErrNoCredentials) {
		return err
	}
	return fmt.Errorf("image generation failed: %w", err)
}
