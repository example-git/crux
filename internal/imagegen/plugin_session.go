package imagegen

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"

	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	validator "github.com/kaptinlin/jsonschema"
)

func imageConfiguration(value manifest.ImageManifest, configuration map[string]any) (map[string]any, [32]byte, error) {
	if configuration == nil {
		configuration = map[string]any{}
	}
	data, err := json.Marshal(configuration)
	if err != nil || len(data) > 1<<20 {
		return nil, [32]byte{}, errors.New("image configuration must be bounded JSON data")
	}
	schemaData, err := json.Marshal(value.Configuration.Schema)
	if err != nil || len(schemaData) > 1<<20 {
		return nil, [32]byte{}, errors.New("image configuration schema is invalid")
	}
	schema, err := validator.NewCompiler().Compile(schemaData)
	if err != nil || !schema.ValidateJSON(data).IsValid() {
		return nil, [32]byte{}, errors.New("image configuration does not satisfy the installed plugin schema")
	}
	var detached map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&detached); err != nil {
		return nil, [32]byte{}, errors.New("image configuration is invalid")
	}
	return detached, sha256.Sum256(data), nil
}

func imageSessionIdentity(configuration [32]byte, credentials PluginCredentials) ([32]byte, error) {
	if len(credentials.CookieJars) > 0 && credentials.Identity == "" {
		return [32]byte{}, errors.New("image browser credentials require an explicit session identity")
	}
	data, err := json.Marshal(struct {
		Configuration [32]byte
		Identity      string
		Values        map[string]any
	}{configuration, credentials.Identity, credentials.Values})
	if err != nil || len(data) > 1<<20 {
		return [32]byte{}, errors.New("image credential identity must be bounded JSON data")
	}
	return sha256.Sum256(data), nil
}

func (r *PluginRuntime) sessionFor(owner providerplugin.ImageOwner, identity [32]byte) *imagePluginSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sessions == nil {
		r.sessions = map[providerplugin.ImageOwner]*imagePluginSession{}
	}
	session := r.sessions[owner]
	if session == nil || session.identity != identity {
		session = &imagePluginSession{identity: identity, uploads: map[string]any{}}
		r.sessions[owner] = session
	}
	return session
}

func (r *PluginRuntime) invalidateSession(owner providerplugin.ImageOwner, session *imagePluginSession) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sessions[owner] == session {
		delete(r.sessions, owner)
	}
}

func (s *imagePluginSession) bootstrap(ctx context.Context, run func() (any, error)) (any, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		s.mu.Lock()
		if s.value != nil {
			value := s.value
			s.mu.Unlock()
			return value, nil
		}
		if s.loading != nil {
			loading := s.loading
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-loading:
				continue
			}
		}
		loading := make(chan struct{})
		s.loading = loading
		s.mu.Unlock()
		value, err := run()
		s.mu.Lock()
		if err == nil {
			s.value = value
		}
		s.loading = nil
		close(loading)
		s.mu.Unlock()
		return value, err
	}
}
