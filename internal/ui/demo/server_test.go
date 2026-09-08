package demo

import (
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestPreviewEmbeddedAssetsAndControlBoundary(t *testing.T) {
	handler, err := NewHandler()
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	request, err := http.NewRequest(http.MethodGet, server.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Accept", "text/html")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	asset := regexp.MustCompile(`src="([^"]+\.js)"`).FindStringSubmatch(string(body))
	if len(asset) != 2 {
		t.Fatal("embedded HTML has no bundled script")
	}
	response, err = http.Get(server.URL + asset[1])
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || !strings.Contains(string(body), "cruxPreview") {
		t.Fatal("embedded control client missing")
	}
	response, err = http.Post(server.URL+"/api/control", "application/json", strings.NewReader(`{"action":"screenshot"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var result map[string]any
	json.NewDecoder(response.Body).Decode(&result)
	if response.StatusCode != 200 || result["captureRenderer"] != "goja-xterm-svg-resvg" {
		t.Fatalf("clientless capture failed: %v", result)
	}
	for index, dimensions := range [][2]int{{160, 45}, {65, 25}} {
		if index > 0 {
			response, err = http.Post(server.URL+"/api/control", "application/json", strings.NewReader(`{"action":"set","state":{"cols":65,"rows":25,"modal":"instructions"},"screenshot":true}`))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != 200 {
				t.Fatalf("compact capture: %v", result)
			}
		}
		screenshot := result["screenshot"].(map[string]any)
		imageResponse, err := http.Get(server.URL + screenshot["url"].(string))
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(imageResponse.Body)
		imageResponse.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		image, err := png.Decode(strings.NewReader(string(data)))
		if err != nil {
			t.Fatal(err)
		}
		if image.Bounds().Dx() != dimensions[0]*8 || image.Bounds().Dy() != dimensions[1]*16 {
			t.Fatalf("wrong PNG bounds: %v", image.Bounds())
		}
		t.Logf("capture %d: %v ms, %d PNG bytes", index, result["elapsedMs"], len(data))
		if directory := os.Getenv("CRUX_PREVIEW_TEST_ARTIFACTS"); directory != "" {
			if err := os.WriteFile(filepath.Join(directory, fmt.Sprintf("capture-%d.png", index)), data, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, command := range []string{`{"action":"screenshot","clientId":"missing"}`, `{"action":"screenshot","state":{"cols":0}}`, `{"action":"screenshot","state":{"bogus":1}}`, `{"action":"screenshot","fontSize":99}`} {
		response, err := http.Post(server.URL+"/api/control", "application/json", strings.NewReader(command))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode < 400 {
			t.Fatalf("invalid capture accepted: %s", command)
		}
	}
}
