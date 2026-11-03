package webclient

import (
	"context"
	"net/http"

	"github.com/example-git/crux/internal/agent/prompt"
	"github.com/example-git/crux/internal/backend"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/instructionhttp"
)

const instructionFileLimit = instructionhttp.FileLimit

var errInstructionFileChanged = instructionhttp.ErrFileChanged

type instructionDocument = instructionhttp.Document

var readInstructionFile = instructionhttp.ReadFile
var saveInstructionFile = instructionhttp.SaveFile
var readRootedInstruction = instructionhttp.ReadRooted

func instructionFiles(native *backend.Backend) http.HandlerFunc {
	handler := instructionhttp.Handler(func(_ context.Context, id string) ([]prompt.InstructionFile, error) {
		ws, err := native.GetWorkspace(id)
		if err != nil {
			return nil, err
		}
		if ws.Cfg.RemoteAuthority() != nil {
			return nil, config.ErrClientRuntimeManaged
		}
		builder, err := prompt.NewPrompt("coder", "")
		if err != nil {
			return nil, err
		}
		return builder.InstructionFiles(ws.Cfg)
	}, nil)
	return func(w http.ResponseWriter, r *http.Request) {
		ws, err := native.GetWorkspace(r.PathValue("id"))
		if err != nil {
			http.Error(w, "workspace not found", http.StatusNotFound)
			return
		}
		if ws.Cfg.RemoteAuthority() != nil {
			http.Error(w, config.ErrClientRuntimeManaged.Error(), http.StatusConflict)
			return
		}
		handler.ServeHTTP(w, r)
	}
}
