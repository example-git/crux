package webclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"

	"github.com/example-git/crux/internal/agent/prompt"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/instructionhttp"
)

type remoteInstructionFile struct {
	prompt.InstructionFile
	Source string `json:"source"`
}

func (a *remoteAttachment) providerInstructionFiles(_ context.Context, id string) ([]prompt.InstructionFile, error) {
	if a.workspace.Snapshot().ID != id {
		return nil, errors.New("remote workspace attachment changed")
	}
	store := a.client.LocalRuntimeStore()
	if store == nil {
		return nil, errors.New("client instruction configuration is unavailable")
	}
	builder, err := prompt.NewPrompt("coder", "")
	if err != nil {
		return nil, err
	}
	files, err := builder.InstructionFiles(store)
	return slices.DeleteFunc(files, func(file prompt.InstructionFile) bool { return file.Scope != "provider" }), err
}

func (a *remoteAttachment) instructions(w http.ResponseWriter, r *http.Request) {
	a.instructionMu.Lock()
	defer a.instructionMu.Unlock()
	snapshot := a.workspace.Snapshot()
	if snapshot.Authority == nil {
		http.Error(w, "remote workspace authority is unavailable", http.StatusConflict)
		return
	}
	r.SetPathValue("id", snapshot.ID)
	source := r.URL.Query().Get("source")
	if source != "" && source != "client" && source != "remote" {
		http.Error(w, "invalid instruction source", http.StatusBadRequest)
		return
	}
	if r.URL.Query().Get("path") != "" && source == "client" {
		instructionhttp.Handler(a.providerInstructionFiles, func(ctx context.Context, _ string, file prompt.InstructionFile) error {
			cfg := a.workspace.Config()
			if cfg.Models[config.SelectedModelTypeLarge].Provider != file.Provider && cfg.Models[config.SelectedModelTypeSmall].Provider != file.Provider {
				return nil
			}
			owner, ok := cfg.ProviderOwner(file.Provider)
			if !ok {
				return errors.New("instruction file saved, but provider ownership is unavailable")
			}
			if err := a.workspace.ReloadProviderContextInstructions(ctx, owner); err != nil {
				return fmt.Errorf("instruction file saved, but runtime publication failed: %w", err)
			}
			return nil
		}).ServeHTTP(w, r)
		return
	}
	response, err := a.client.WorkspaceRequest(r.Context(), snapshot.ID, r.Method, "/instructions", r.URL.Query(), r.Body, *snapshot.Authority)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	if r.Method != http.MethodGet || r.URL.Query().Get("path") != "" || response.StatusCode != http.StatusOK {
		w.Header().Set("Content-Type", response.Header.Get("Content-Type"))
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
		return
	}
	var files []prompt.InstructionFile
	if err := json.NewDecoder(response.Body).Decode(&files); err != nil {
		http.Error(w, "invalid remote instruction catalog", http.StatusBadGateway)
		return
	}
	local, err := a.providerInstructionFiles(r.Context(), snapshot.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	catalog := make([]remoteInstructionFile, 0, len(files)+len(local))
	for _, file := range files {
		catalog = append(catalog, remoteInstructionFile{InstructionFile: file, Source: "remote"})
	}
	for _, file := range local {
		catalog = append(catalog, remoteInstructionFile{InstructionFile: file, Source: "client"})
	}
	writeJSON(w, catalog)
}
