package copilot

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/env"
	"golang.org/x/net/http/httpproxy"
)

const (
	catalogSourceURL      = "https://models.dev/api.json"
	catalogCacheTTL       = 24 * time.Hour
	catalogRetryInterval  = 5 * time.Minute
	catalogRequestTimeout = 5 * time.Second
)

type cachedCatalog struct {
	Version   int             `json:"version"`
	Source    string          `json:"source"`
	FetchedAt time.Time       `json:"fetched_at"`
	Models    []catalog.Model `json:"models"`
}

type catalogCacheState struct {
	mu          sync.Mutex
	loaded      bool
	entry       *cachedCatalog
	nextAttempt time.Time
	lastError   error
}

var catalogCacheStates sync.Map

func CachedCatalogProvider(ctx context.Context, cacheRoot string, environment env.Env) (catalog.Provider, error) {
	path := filepath.Join(cacheRoot, "models", "copilot.json")
	value, _ := catalogCacheStates.LoadOrStore(path, &catalogCacheState{})
	state := value.(*catalogCacheState)
	state.mu.Lock()
	defer state.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return catalog.Provider{}, err
	}
	if !state.loaded {
		state.entry, state.lastError = readCatalogCache(path)
		state.loaded = true
	}
	now := time.Now()
	if state.entry != nil {
		age := now.Sub(state.entry.FetchedAt)
		if age >= 0 && age < catalogCacheTTL {
			return providerWithCatalog(state.entry.Models), nil
		}
	}
	if now.Before(state.nextAttempt) {
		if state.entry != nil {
			return providerWithCatalog(state.entry.Models), state.lastError
		}
		return CatalogProvider(), state.lastError
	}
	models, err := fetchCatalog(ctx, environment)
	if err != nil {
		if ctx.Err() != nil {
			return catalog.Provider{}, ctx.Err()
		}
		state.nextAttempt = time.Now().Add(catalogRetryInterval)
		if state.entry != nil {
			state.lastError = fmt.Errorf("refresh Copilot model catalog; using last valid cache: %w", err)
			return providerWithCatalog(state.entry.Models), state.lastError
		}
		state.lastError = fmt.Errorf("refresh Copilot model catalog; using built-in snapshot: %w", err)
		return CatalogProvider(), state.lastError
	}
	state.entry = &cachedCatalog{Version: 1, Source: catalogSourceURL, FetchedAt: time.Now(), Models: models}
	state.nextAttempt = time.Time{}
	state.lastError = writeCatalogCache(path, state.entry)
	return providerWithCatalog(models), state.lastError
}

func providerWithCatalog(models []catalog.Model) catalog.Provider {
	provider := CatalogProvider()
	provider.Models = slices.Clone(models)
	for index := range provider.Models {
		provider.Models[index].ReasoningLevels = slices.Clone(provider.Models[index].ReasoningLevels)
	}
	for _, selected := range []*string{&provider.DefaultLargeModelID, &provider.DefaultSmallModelID} {
		if !slices.ContainsFunc(models, func(model catalog.Model) bool { return model.ID == *selected }) {
			*selected = models[0].ID
		}
	}
	return provider
}

func fetchCatalog(ctx context.Context, environment env.Env) ([]catalog.Model, error) {
	if environment == nil {
		environment = env.New()
	}
	ctx, cancel := context.WithTimeout(ctx, catalogRequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, catalogSourceURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "crux-copilot-catalog/1")
	transport := http.DefaultTransport
	if base, ok := transport.(*http.Transport); ok {
		copy := base.Clone()
		defer copy.CloseIdleConnections()
		proxy := (&httpproxy.Config{
			HTTPProxy:  cmp.Or(environment.Get("HTTP_PROXY"), environment.Get("http_proxy")),
			HTTPSProxy: cmp.Or(environment.Get("HTTPS_PROXY"), environment.Get("https_proxy")),
			NoProxy:    cmp.Or(environment.Get("NO_PROXY"), environment.Get("no_proxy")),
			CGI:        environment.Get("REQUEST_METHOD") != "",
		}).ProxyFunc()
		copy.Proxy = func(request *http.Request) (*url.URL, error) { return proxy(request.URL) }
		transport = copy
	}
	client := &http.Client{Transport: transport, Timeout: catalogRequestTimeout}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("catalog source returned HTTP %d", response.StatusCode)
	}
	return decodeCatalog(io.LimitReader(response.Body, 64<<20))
}

func decodeCatalog(reader io.Reader) ([]catalog.Model, error) {
	var source struct {
		Copilot struct {
			ID     string `json:"id"`
			Models map[string]struct {
				ID               string `json:"id"`
				Name             string `json:"name"`
				Status           string `json:"status"`
				Reasoning        bool   `json:"reasoning"`
				ReasoningOptions []struct {
					Type   string   `json:"type"`
					Values []string `json:"values"`
				} `json:"reasoning_options"`
				Modalities struct {
					Input  []string `json:"input"`
					Output []string `json:"output"`
				} `json:"modalities"`
				Limit struct {
					Context int64 `json:"context"`
					Output  int64 `json:"output"`
				} `json:"limit"`
			} `json:"models"`
		} `json:"github-copilot"`
	}
	decoder := json.NewDecoder(reader)
	if err := decoder.Decode(&source); err != nil {
		return nil, fmt.Errorf("decode Copilot catalog: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("catalog source contains trailing data")
	}
	if source.Copilot.ID != "github-copilot" {
		return nil, errors.New("catalog source is missing github-copilot")
	}
	models := make([]catalog.Model, 0, len(source.Copilot.Models))
	for id, row := range source.Copilot.Models {
		if row.Status == "deprecated" || row.Status == "retired" {
			continue
		}
		if len(row.Modalities.Output) == 0 {
			return nil, fmt.Errorf("missing output modality for Copilot model %q", id)
		}
		if !slices.Equal(row.Modalities.Output, []string{"text"}) {
			continue
		}
		if row.ID != "" && row.ID != id || !slices.Contains(row.Modalities.Input, "text") {
			return nil, fmt.Errorf("invalid Copilot model %q", id)
		}
		model := catalog.Model{
			ID: id, Name: row.Name, ContextWindow: row.Limit.Context, DefaultMaxTokens: row.Limit.Output,
			CanReason: row.Reasoning, SupportsImages: slices.Contains(row.Modalities.Input, "image"),
		}
		if row.Reasoning {
			for _, option := range row.ReasoningOptions {
				if option.Type == "effort" {
					model.ReasoningLevels = slices.Clone(option.Values)
					break
				}
			}
			if len(model.ReasoningLevels) > 0 {
				model.DefaultReasoningEffort = model.ReasoningLevels[0]
				if slices.Contains(model.ReasoningLevels, "medium") {
					model.DefaultReasoningEffort = "medium"
				}
			}
		}
		models = append(models, model)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	if err := validateCatalog(models); err != nil {
		return nil, err
	}
	return models, nil
}

func validateCatalog(models []catalog.Model) error {
	if len(models) == 0 {
		return errors.New("Copilot catalog contains no active text models")
	}
	seen := make(map[string]bool, len(models))
	for _, model := range models {
		if model.ID == "" || seen[model.ID] || model.Name == "" || model.ContextWindow <= 0 || model.DefaultMaxTokens <= 0 || model.DefaultMaxTokens > model.ContextWindow {
			return fmt.Errorf("invalid Copilot catalog model %q", model.ID)
		}
		seen[model.ID] = true
		for _, level := range model.ReasoningLevels {
			if level == "" {
				return fmt.Errorf("empty reasoning effort for Copilot model %q", model.ID)
			}
		}
		if model.DefaultReasoningEffort != "" && !slices.Contains(model.ReasoningLevels, model.DefaultReasoningEffort) {
			return fmt.Errorf("invalid default reasoning effort for Copilot model %q", model.ID)
		}
	}
	return nil
}

func readCatalogCache(path string) (*cachedCatalog, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var entry cachedCatalog
	decoder := json.NewDecoder(io.LimitReader(file, 4<<20))
	if err := decoder.Decode(&entry); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("Copilot catalog cache contains trailing data")
	}
	if entry.Version != 1 || entry.Source != catalogSourceURL || entry.FetchedAt.IsZero() {
		return nil, errors.New("invalid Copilot catalog cache metadata")
	}
	if err := validateCatalog(entry.Models); err != nil {
		return nil, err
	}
	return &entry, nil
}

func writeCatalogCache(path string, entry *cachedCatalog) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create Copilot model cache directory: %w", err)
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".copilot-*.tmp")
	if err != nil {
		return fmt.Errorf("create Copilot model cache: %w", err)
	}
	defer os.Remove(file.Name())
	if err := json.NewEncoder(file).Encode(entry); err != nil {
		file.Close()
		return fmt.Errorf("encode Copilot model cache: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("flush Copilot model cache: %w", err)
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return fmt.Errorf("replace Copilot model cache: %w", err)
	}
	return nil
}
