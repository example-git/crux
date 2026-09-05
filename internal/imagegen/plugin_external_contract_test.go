package imagegen

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sync/atomic"
	"testing"

	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/require"
)

type externalImageContractTransport func(*http.Request) (*http.Response, error)

func (f externalImageContractTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestExternalImageBundleContract(t *testing.T) {
	source := os.Getenv("CRUX_TEST_IMAGE_BUNDLE")
	contractPath := os.Getenv("CRUX_TEST_IMAGE_CONTRACT")
	if source == "" && contractPath == "" {
		t.Skip("no external image bundle contract selected")
	}
	require.NotEmpty(t, source)
	require.NotEmpty(t, contractPath)
	var contract struct {
		Environment     []string          `json:"environment"`
		HeaderPatterns  map[string]string `json:"header_patterns"`
		RequestCount    int               `json:"request_count"`
		ResponseCount   int               `json:"response_count"`
		EditBody        map[string]any    `json:"edit_body"`
		EditImagesField string            `json:"edit_images_field"`
		EditURLField    string            `json:"edit_url_field"`
		Credentials     map[string]any    `json:"credentials"`
		Headers         map[string]string `json:"headers"`
		GeneratePath    string            `json:"generate_path"`
		GenerateBody    map[string]any    `json:"generate_body"`
		EditPath        string            `json:"edit_path"`
		EditFields      map[string]string `json:"edit_fields"`
		EditFileField   string            `json:"edit_file_field"`
		ResponseField   string            `json:"response_field"`
		ImageField      string            `json:"image_field"`
	}
	data, err := os.ReadFile(contractPath)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &contract))
	manifestData, err := os.ReadFile(filepath.Join(source, "manifest.json"))
	require.NoError(t, err)
	_, err = manifest.DecodeImageStrict(manifestData)
	require.NoError(t, err)
	root := t.TempDir()
	manager, err := providerplugin.NewManager(t.Context(), providerplugin.DefaultPaths(filepath.Join(root, "data"), filepath.Join(root, "cache")))
	require.NoError(t, err)
	defer manager.Close()
	bundle, err := manager.InspectImageSource(t.Context(), source)
	require.NoError(t, err)
	_, err = manager.Install(t.Context(), providerplugin.InstallRequest{Source: source, ExpectedDigest: bundle.Digest, Trust: true})
	require.NoError(t, err)
	var pixels bytes.Buffer
	require.NoError(t, png.Encode(&pixels, image.NewRGBA(image.Rect(0, 0, 2, 2))))
	encoded := base64.StdEncoding.EncodeToString(pixels.Bytes())
	for _, mode := range []string{ModeGenerate, ModeEdit} {
		t.Run(string(mode), func(t *testing.T) {
			var calls atomic.Int64
			transport := externalImageContractTransport(func(request *http.Request) (*http.Response, error) {
				calls.Add(1)
				require.Equal(t, "POST", request.Method)
				for key, value := range contract.Headers {
					require.Equal(t, value, request.Header.Get(key), key)
				}
				for key, pattern := range contract.HeaderPatterns {
					require.Regexp(t, regexp.MustCompile(pattern), request.Header.Get(key), key)
				}
				if mode == ModeGenerate {
					require.Equal(t, contract.GeneratePath, request.URL.Path)
					var body map[string]any
					require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
					require.Equal(t, contract.GenerateBody, body)
				} else if contract.EditBody != nil {
					require.Equal(t, contract.EditPath, request.URL.Path)
					var body map[string]any
					require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
					images := body[contract.EditImagesField]
					delete(body, contract.EditImagesField)
					require.Equal(t, contract.EditBody, body)
					require.Equal(t, []any{map[string]any{contract.EditURLField: "data:image/png;base64," + encoded}}, images)
				} else {
					require.Equal(t, contract.EditPath, request.URL.Path)
					require.NoError(t, request.ParseMultipartForm(1<<20))
					defer request.MultipartForm.RemoveAll()
					require.Len(t, request.MultipartForm.Value, len(contract.EditFields))
					for key, value := range contract.EditFields {
						require.Equal(t, []string{value}, request.MultipartForm.Value[key], key)
					}
					require.Len(t, request.MultipartForm.File, 1)
					files := request.MultipartForm.File[contract.EditFileField]
					require.Len(t, files, 1)
					require.Equal(t, "input.png", files[0].Filename)
					file, err := files[0].Open()
					require.NoError(t, err)
					original, err := io.ReadAll(file)
					require.NoError(t, err)
					require.NoError(t, file.Close())
					require.Equal(t, pixels.Bytes(), original)
				}
				images := make([]any, contract.ResponseCount)
				for index := range images {
					images[index] = map[string]any{contract.ImageField: encoded}
				}
				body, err := json.Marshal(map[string]any{contract.ResponseField: images})
				require.NoError(t, err)
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(body)), Request: request}, nil
			})
			runtime := &PluginRuntime{Environment: contract.Environment, Manager: manager, Client: &http.Client{Transport: transport}, ResolveCredentials: func(_ context.Context, _ providerplugin.RegisteredImageBundle) (PluginCredentials, error) {
				return PluginCredentials{Values: contract.Credentials}, nil
			}}
			request := JobRequest{Mode: mode, Prompt: "synthetic test image", Count: 2}
			var inputs []EditImage
			if mode == ModeEdit {
				inputs = []EditImage{{Filename: "input.png", MIMEType: "image/png", Data: pixels.Bytes()}}
			}
			response, err := runtime.Execute(t.Context(), bundle.Owner(), request, inputs)
			require.NoError(t, err)
			require.Empty(t, response.Failures)
			require.Len(t, response.Data, 2)
			for _, output := range response.Data {
				require.Equal(t, encoded, output.B64JSON)
			}
			require.Equal(t, int64(contract.RequestCount), calls.Load())
		})
	}
}
