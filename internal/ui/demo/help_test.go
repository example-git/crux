package demo

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestPreviewHelpAndGeneratedSchemas(t *testing.T) {
	handler, err := NewHandler()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		path, accept, contentType, contains string
		status                              int
	}{
		{"/", "", "text/markdown", "# Crux UI preview", 200},
		{"/", "*/*", "text/markdown", "# Crux UI preview", 200},
		{"/", "text/html", "text/html", "<html", 200},
		{"/", "text/html,text/markdown", "text/markdown", "# Crux UI preview", 200},
		{"/help", "text/html", "text/markdown", "1280×720", 200},
		{"/help/", "", "text/markdown", "# Crux UI preview", 200},
		{"/help/index.md", "", "text/markdown", "Generated schema", 200},
		{"/help/missing.md", "", "text/plain", "404", 404},
		{"/help/screenshots.md", "", "text/markdown", "1280×720", 200},
		{"/help/controls.md", "", "text/markdown", "/api/catalog", 200},
		{"/help/auditing.md", "", "text/markdown", "Reproducible audit", 200},
		{"/help/options.md", "", "text/markdown", "`cols`", 200},
		{"/help/fixture.md", "", "text/markdown", "`examples`", 200},
		{"/help/frame.md", "", "text/markdown", "`renderer`", 200},
	} {
		t.Run(test.path+test.accept, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			request.Header.Set("Accept", test.accept)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status || !strings.HasPrefix(response.Header().Get("Content-Type"), test.contentType) || !strings.Contains(response.Body.String(), test.contains) {
				t.Fatalf("unexpected response: %d %s %s", response.Code, response.Header().Get("Content-Type"), response.Body.String())
			}
			if test.path == "/" && response.Header().Get("Vary") != "Accept" {
				t.Fatal("missing Accept cache variation")
			}
			if test.contentType == "text/markdown" {
				for _, match := range regexp.MustCompile(`\]\((/help/[^)]+)\)`).FindAllStringSubmatch(response.Body.String(), -1) {
					linked := httptest.NewRecorder()
					handler.ServeHTTP(linked, httptest.NewRequest(http.MethodGet, match[1], nil))
					if linked.Code != 200 {
						t.Fatalf("broken guide link %s: %d", match[1], linked.Code)
					}
				}
			}
		})
	}
	for _, name := range []string{"options", "fixture", "frame"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/help/"+name+".schema.json", nil))
		var document map[string]any
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &document) != nil {
			t.Fatalf("invalid %s schema: %s", name, response.Body.String())
		}
		if document["$schema"] == nil || document["$defs"] == nil {
			t.Fatalf("incomplete %s schema", name)
		}
		if name == "options" {
			properties := document["$defs"].(map[string]any)["PreviewOptions"].(map[string]any)["properties"].(map[string]any)
			for field, expected := range map[string]float64{"cols": 160, "rows": 45} {
				if properties[field].(map[string]any)["default"] != expected {
					t.Fatalf("wrong %s default", field)
				}
			}
		}
	}
}
