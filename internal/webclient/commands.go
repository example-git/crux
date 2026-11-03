package webclient

import (
	"encoding/json"
	"net/http"

	"github.com/example-git/crux/internal/commands"
	"github.com/example-git/crux/internal/config"
)

type commandBackend interface {
	GetWorkspaceConfig(string) (*config.Config, error)
}

type commandArgument struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Required bool   `json:"required"`
}

type customCommand struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Content   string            `json:"content"`
	Arguments []commandArgument `json:"arguments"`
}

func customCommands(backend commandBackend) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg, err := backend.GetWorkspaceConfig(r.PathValue("id"))
		if err != nil {
			http.Error(w, "workspace not found", http.StatusNotFound)
			return
		}
		loaded, err := commands.LoadCustomCommands(cfg)
		if err != nil {
			http.Error(w, "could not load custom commands", http.StatusInternalServerError)
			return
		}
		result := make([]customCommand, 0, len(loaded))
		for _, command := range loaded {
			arguments := make([]commandArgument, 0, len(command.Arguments))
			for _, argument := range command.Arguments {
				arguments = append(arguments, commandArgument{ID: argument.ID, Title: argument.Title, Required: argument.Required})
			}
			result = append(result, customCommand{ID: command.ID, Name: command.Name, Content: command.Content, Arguments: arguments})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	}
}
