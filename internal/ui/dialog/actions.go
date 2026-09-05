package dialog

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	tea "charm.land/bubbletea/v2"
	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/commands"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/permission"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/session"
	"github.com/example-git/crux/internal/skills"
	"github.com/example-git/crux/internal/tmuxsession"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/util"
)

// ActionClose is a message to close the current dialog.
type ActionClose struct {
	Dismiss bool
}

// ActionQuit is a message to quit the application.
type ActionQuit = tea.QuitMsg

// ActionOpenDialog is a message to open a dialog.
type ActionOpenDialog struct {
	DialogID string
}

type ActionAgentDefinitionCreated struct {
	Path       string
	RefreshErr error
}

// ActionSelectSession is a message indicating a session has been selected.
type ActionSelectSession struct {
	Session session.Session
}

type ActionAttachTmuxSession struct {
	Session tmuxsession.Session
}

// ActionSelectModel is a message indicating a model has been selected.
type ActionSelectModel struct {
	Provider         catalog.Provider
	Model            config.SelectedModel
	ModelType        config.SelectedModelType
	ProviderOwner    providerregistry.RegistrationOwner
	ProviderOwnerSet bool
	ReAuthenticate   bool
}

func (a ActionSelectModel) ValidateProviderOwner(cfg *config.Config) error {
	providerID := a.Model.Provider
	if providerID == "" || string(a.Provider.ID) != providerID {
		return fmt.Errorf("model selection provider identity is invalid")
	}
	if cfg == nil {
		return fmt.Errorf("configuration not found")
	}
	if !a.ProviderOwnerSet {
		return fmt.Errorf("model selection provider owner is missing for %s", providerID)
	}
	current, ok := cfg.ProviderOwner(providerID)
	if !ok || current != a.ProviderOwner {
		return fmt.Errorf("model selection provider owner changed for %s", providerID)
	}
	return nil
}

// Messages for commands
type (
	ActionNewSession              struct{}
	ActionToggleHelp              struct{}
	ActionToggleCompactMode       struct{}
	ActionToggleThinking          struct{}
	ActionTogglePills             struct{}
	ActionExternalEditor          struct{}
	ActionToggleYoloMode          struct{}
	ActionTogglePlanMode          struct{}
	ActionToggleNotifications     struct{}
	ActionSelectNotificationStyle struct {
		Style string
	}
	ActionSelectProject struct {
		Slug string
	}
	ProjectSelectionDoneMsg struct {
		Slug string
		Err  error
	}
	ActionToggleTransparentBackground struct{}
	ActionInitializeProject           struct{}
	ActionSummarize                   struct {
		SessionID string
	}
	// ActionSelectReasoningEffort is a message indicating a reasoning effort
	// has been selected.
	ActionSelectReasoningEffort struct {
		Effort string
	}
	ActionPermissionResponse struct {
		Permission permission.PermissionRequest
		Action     PermissionAction
	}
	// ActionRunCustomCommand is a message to run a custom command.
	ActionRunCustomCommand struct {
		Content   string
		Arguments []commands.Argument
		Args      map[string]string // Actual argument values
		Skill     *skills.Skill     // Set when this is a skill command
	}
	// ActionAttachSkill is sent when a skill is selected from the commands
	// dialog to be attached to the conversation as a markdown attachment.
	ActionAttachSkill struct {
		ID   string
		Name string
	}
	// ActionRunMCPPrompt is a message to run a custom command.
	ActionRunMCPPrompt struct {
		Title       string
		Description string
		PromptID    string
		ClientID    string
		Arguments   []commands.Argument
		Args        map[string]string // Actual argument values
	}
	// ActionEnableDockerMCP is a message to enable Docker MCP.
	ActionEnableDockerMCP struct{}
	// ActionDisableDockerMCP is a message to disable Docker MCP.
	ActionDisableDockerMCP struct{}
)

// Messages for MCP OAuth authentication dialog.
type (
	// ActionMCPAuthStarted is sent when the user approves authentication
	// for an MCP server. The UI should initiate the actual auth flow
	// using the provided context, which the dialog will cancel if the
	// user closes it.
	ActionMCPAuthStarted struct {
		Name string
		Ctx  context.Context
	}

	// ActionMCPAuthComplete is sent when MCP authentication succeeds.
	ActionMCPAuthComplete struct {
		Name string
	}

	// ActionMCPAuthErrored is sent when MCP authentication fails.
	ActionMCPAuthErrored struct {
		Name  string
		Error error
	}
)

// Messages for API key input dialog.
type (
	ActionChangeAPIKeyState struct {
		State APIKeyInputState
	}
)

// ActionCmd represents an action that carries a [tea.Cmd] to be passed to the
// Bubble Tea program loop.
type ActionCmd struct {
	Cmd tea.Cmd
}

// ActionFilePickerSelected is a message indicating a file has been selected in
// the file picker dialog.
type ActionFilePickerSelected struct {
	Path           string
	MaxSourceBytes int64
}

// Cmd returns a command that reads the file at path and sends a
// [message.Attachement] to the program.
func (a ActionFilePickerSelected) Cmd() tea.Cmd {
	path := a.Path
	if path == "" {
		return nil
	}
	return func() tea.Msg {
		limit := a.MaxSourceBytes
		if limit <= 0 {
			limit = common.MaxAttachmentSize
		}
		isFileLarge, err := common.IsFileTooBig(path, limit)
		if err != nil {
			return util.InfoMsg{
				Type: util.InfoTypeError,
				Msg:  fmt.Sprintf("unable to read the image: %v", err),
			}
		}
		if isFileLarge {
			return util.InfoMsg{
				Type: util.InfoTypeError,
				Msg:  fmt.Sprintf("image too large, max %dMB before resizing", limit/(1024*1024)),
			}
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return util.InfoMsg{
				Type: util.InfoTypeError,
				Msg:  fmt.Sprintf("unable to read the image: %v", err),
			}
		}

		mimeBufferSize := min(512, len(content))
		mimeType := http.DetectContentType(content[:mimeBufferSize])
		fileName := filepath.Base(path)

		return message.Attachment{
			FilePath: path,
			FileName: fileName,
			MimeType: mimeType,
			Content:  content,
		}
	}
}
