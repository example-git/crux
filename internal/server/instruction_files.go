package server

import (
	"context"
	"net/http"
	"slices"

	"github.com/example-git/crux/internal/agent/prompt"
	"github.com/example-git/crux/internal/instructionhttp"
)

func (s *Server) instructionFiles() http.HandlerFunc {
	return instructionhttp.Handler(func(_ context.Context, id string) ([]prompt.InstructionFile, error) {
		ws, err := s.backend.GetWorkspace(id)
		if err != nil {
			return nil, err
		}
		builder, err := prompt.NewPrompt("coder", "")
		if err != nil {
			return nil, err
		}
		files, err := builder.InstructionFiles(ws.Cfg)
		if ws.Cfg.RemoteAuthority() != nil {
			files = slices.DeleteFunc(files, func(file prompt.InstructionFile) bool { return file.Scope == "provider" })
		}
		return files, err
	}, nil)
}
