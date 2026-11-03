package instructionhttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/agent/prompt"
	"github.com/stretchr/testify/require"
)

func TestSavePublishesOnlyAfterRevisionCheckedWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provider.txt")
	require.NoError(t, os.WriteFile(path, []byte("before"), 0o600))
	before, err := ReadFile(path)
	require.NoError(t, err)
	published := 0
	handler := Handler(func(context.Context, string) ([]prompt.InstructionFile, error) {
		return []prompt.InstructionFile{{Path: path, Scope: "provider", Provider: "fixture"}}, nil
	}, func(_ context.Context, id string, file prompt.InstructionFile) error {
		require.Equal(t, "workspace", id)
		require.Equal(t, "fixture", file.Provider)
		content, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, "after", string(content))
		published++
		if published == 1 {
			return errors.New("publication unavailable")
		}
		return nil
	})
	body, err := json.Marshal(map[string]string{"text": "after", "revision": before.Revision})
	require.NoError(t, err)
	request := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPut, "/?path="+url.QueryEscape(path), strings.NewReader(string(body)))
		r.SetPathValue("id", "workspace")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	first := request()
	require.Equal(t, http.StatusBadGateway, first.Code)
	require.Contains(t, first.Body.String(), "publication unavailable")
	require.Equal(t, 1, published)
	second := request()
	require.Equal(t, http.StatusConflict, second.Code)
	require.Equal(t, 1, published)
	after, err := ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "after", after.Text)
	require.NotEqual(t, before.Revision, after.Revision)
	var failure struct {
		Saved Document `json:"saved_document"`
	}
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &failure))
	require.Equal(t, after, failure.Saved)
	body, err = json.Marshal(map[string]string{"text": after.Text, "revision": failure.Saved.Revision})
	require.NoError(t, err)
	third := request()
	require.Equal(t, http.StatusOK, third.Code)
	require.Equal(t, 2, published)
}
