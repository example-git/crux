package imagegen

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/providerplugin"
	"github.com/stretchr/testify/require"
)

func TestExternalImageWorkflowContract(t *testing.T) {
	source, contractPath := os.Getenv("CRUX_TEST_IMAGE_BUNDLE"), os.Getenv("CRUX_TEST_IMAGE_WORKFLOW")
	if contractPath == "" {
		t.Skip("no external workflow contract selected")
	}
	var contract struct {
		CookieCredential string `json:"cookie_credential"`
		CookieURL        string `json:"cookie_url"`
		CookieName       string `json:"cookie_name"`
		Cases            []struct {
			Mode   string `json:"mode"`
			Input  string `json:"input"`
			Output string `json:"output"`
			Steps  []struct {
				Method         string            `json:"method"`
				Path           string            `json:"path"`
				Query          map[string]string `json:"query"`
				Form           map[string]string `json:"form"`
				EncodedField   string            `json:"encoded_field"`
				StringPointer  string            `json:"string_pointer"`
				Payload        map[string]any    `json:"payload"`
				Response       string            `json:"response"`
				ResponseBase64 string            `json:"response_base64"`
				ContentType    string            `json:"content_type"`
			} `json:"steps"`
		} `json:"cases"`
	}
	data, err := os.ReadFile(contractPath)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &contract))
	root := t.TempDir()
	manager, err := providerplugin.NewManager(t.Context(), providerplugin.DefaultPaths(filepath.Join(root, "data"), filepath.Join(root, "cache")))
	require.NoError(t, err)
	defer manager.Close()
	bundle, err := manager.InspectImageSource(t.Context(), source)
	require.NoError(t, err)
	_, err = manager.Install(t.Context(), providerplugin.InstallRequest{Source: source, ExpectedDigest: bundle.Digest, Trust: true})
	require.NoError(t, err)
	for _, test := range contract.Cases {
		t.Run(test.Mode, func(t *testing.T) {
			index := 0
			transport := externalImageContractTransport(func(request *http.Request) (*http.Response, error) {
				if index >= len(test.Steps) {
					return nil, fmt.Errorf("unexpected request %d", index)
				}
				expected := test.Steps[index]
				index++
				require.Equal(t, expected.Method, request.Method, "step %d", index)
				require.Equal(t, expected.Path, request.URL.Path, "step %d", index)
				for name, value := range expected.Query {
					require.Equal(t, value, request.URL.Query().Get(name), "step %d query %s", index, name)
				}
				require.NoError(t, request.ParseForm())
				for name, value := range expected.Form {
					require.Equal(t, value, request.PostForm.Get(name), "step %d form %s", index, name)
				}
				if expected.EncodedField != "" {
					var payload any
					require.NoError(t, json.Unmarshal([]byte(request.PostForm.Get(expected.EncodedField)), &payload))
					if expected.StringPointer != "" {
						text, ok := externalContractPointer(t, payload, expected.StringPointer).(string)
						require.True(t, ok)
						require.NoError(t, json.Unmarshal([]byte(text), &payload))
					}
					for pointer, value := range expected.Payload {
						require.Equal(t, value, externalContractPointer(t, payload, pointer), "step %d payload %s", index, pointer)
					}
				}
				body := []byte(expected.Response)
				if expected.ResponseBase64 != "" {
					body, err = base64.StdEncoding.DecodeString(expected.ResponseBase64)
					require.NoError(t, err)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{expected.ContentType}}, Body: io.NopCloser(bytes.NewReader(body)), Request: request}, nil
			})
			jar, err := cookiejar.New(nil)
			require.NoError(t, err)
			address, err := url.Parse(contract.CookieURL)
			require.NoError(t, err)
			jar.SetCookies(address, []*http.Cookie{{Name: contract.CookieName, Value: "synthetic-session", Path: "/", Secure: true}})
			runtime := &PluginRuntime{Manager: manager, UploadDirectory: t.TempDir(), Client: &http.Client{Transport: transport}, ResolveCredentials: func(context.Context, providerplugin.RegisteredImageBundle) (PluginCredentials, error) {
				return PluginCredentials{Identity: "synthetic-browser", CookieJars: map[string]http.CookieJar{contract.CookieCredential: jar}}, nil
			}}
			var inputs []EditImage
			if test.Input != "" {
				data, err := base64.StdEncoding.DecodeString(test.Input)
				require.NoError(t, err)
				inputs = []EditImage{{Filename: "input.png", MIMEType: "image/png", Data: data}}
			}
			response, err := runtime.Execute(t.Context(), bundle.Owner(), JobRequest{Mode: test.Mode, Prompt: "synthetic test image", Count: 1, Size: "1024x1024"}, inputs)
			require.NoError(t, err)
			require.Empty(t, response.Failures)
			require.Len(t, response.Data, 1)
			require.Equal(t, test.Output, response.Data[0].B64JSON)
			require.Equal(t, len(test.Steps), index)
		})
	}
}

func externalContractPointer(t *testing.T, value any, pointer string) any {
	t.Helper()
	for _, part := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		part = strings.NewReplacer("~1", "/", "~0", "~").Replace(part)
		switch container := value.(type) {
		case []any:
			index, err := strconv.Atoi(part)
			require.NoError(t, err)
			require.GreaterOrEqual(t, index, 0)
			require.Less(t, index, len(container))
			value = container[index]
		case map[string]any:
			var ok bool
			value, ok = container[part]
			require.True(t, ok)
		default:
			t.Fatalf("invalid contract pointer %s", pointer)
		}
	}
	return value
}
