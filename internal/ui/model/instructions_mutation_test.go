package model

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/example-git/crux/foundation/bubbletea"
	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

type instructionMutationWorkspace struct {
	*testWorkspace
	surfaces                   []providerregistry.Surface
	mutationErr                error
	mutationCalls, reloadCalls int
	toolingOwner               providerregistry.RegistrationOwner
}

func (w *instructionMutationWorkspace) ProviderSurfaces() []providerregistry.Surface {
	return w.surfaces
}
func (w *instructionMutationWorkspace) PermissionSkipRequests() bool                     { return false }
func (w *instructionMutationWorkspace) LSPGetStates() map[string]workspace.LSPClientInfo { return nil }
func (w *instructionMutationWorkspace) SetConfigField(config.Scope, string, any) error {
	w.mutationCalls++
	return w.mutationErr
}
func (w *instructionMutationWorkspace) SetProviderToolingInstructions(_ config.Scope, owner providerregistry.RegistrationOwner, profile string) error {
	w.mutationCalls++
	w.toolingOwner = owner
	return w.mutationErr
}
func (w *instructionMutationWorkspace) ReloadProviderContextInstructions(_ context.Context, owner providerregistry.RegistrationOwner) error {
	w.reloadCalls++
	w.toolingOwner = owner
	return w.mutationErr
}

func instructionUISnapshot(t *testing.T, profile string) proto.Workspace {
	t.Helper()
	owner := providerregistry.RegistrationOwner{ProviderID: "synthetic", Construction: providerregistry.ConstructionGenericJSON, HasManifest: true, ManifestID: "plugin.synthetic", ManifestVersion: "1.0.0"}
	surfaces := []providerregistry.Surface{{ID: owner.ProviderID, Name: "Synthetic", Owner: &owner, Available: true,
		Instructions: &providerregistry.InstructionSurface{Default: "stock", Profiles: map[string]string{"stock": "Synthetic native tooling instructions"}}}}
	cfg := &config.Config{Options: &config.Options{}, Providers: csync.NewMapFrom(map[string]config.ProviderConfig{owner.ProviderID: {ID: owner.ProviderID, ToolingInstructions: profile}}),
		Models: map[config.SelectedModelType]config.SelectedModel{
			config.SelectedModelTypeLarge: {Provider: owner.ProviderID, Model: "fixture"},
			config.SelectedModelTypeSmall: {Provider: owner.ProviderID, Model: "fixture"},
		}}
	require.NoError(t, cfg.BindProviderSurfaceOwners(surfaces))
	return proto.Workspace{ID: "instructions", Path: t.TempDir(), Config: cfg, ProviderSurfaces: surfaces}
}

func newInstructionMutationUI(t *testing.T) (*UI, *instructionMutationWorkspace, *dialog.Instructions) {
	t.Helper()
	snapshot := instructionUISnapshot(t, config.ToolingInstructionsCrux)
	ws := &instructionMutationWorkspace{testWorkspace: &testWorkspace{cfg: snapshot.Config}, surfaces: snapshot.ProviderSurfaces}
	ui := newTestUI()
	ui.com.Workspace = ws
	ui.focus = uiFocusNone
	ui.agentBusyCache.set(false)
	ui.yoloCache.set(false)
	ui.lspCheckedAt = time.Now()
	ui.dialog = dialog.NewOverlay()
	instructions := dialog.NewInstructions(ui.com)
	ui.dialog.OpenDialog(instructions)
	return ui, ws, instructions
}

func selectInstructionNative(t *testing.T, instructions *dialog.Instructions) dialog.Action {
	t.Helper()
	// The existing three instruction-mode choices precede the native checkbox.
	for range 3 {
		require.Nil(t, instructions.HandleMsg(tea.KeyPressMsg{Code: tea.KeyDown}))
	}
	return instructions.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
}

func TestInstructionMutationCompletionDeliveredThroughUI(t *testing.T) {
	ui, ws, instructions := newInstructionMutationUI(t)
	command := ui.handleDialogAction(selectInstructionNative(t, instructions))
	require.Zero(t, ws.mutationCalls)
	messages := collectCommandMessages(command)
	require.Len(t, messages, 1)
	completed, ok := messages[0].(dialog.ActionInstructionMutationCompleted)
	require.True(t, ok)
	require.NoError(t, completed.Err)
	require.Equal(t, *ws.surfaces[0].Owner, ws.toolingOwner)
	require.Zero(t, ws.updateAgentCalls)
	_, command = ui.Update(completed)
	require.Zero(t, ws.updateAgentCalls, "UI must schedule agent rebuild")
	collectCommandMessages(command)
	require.Equal(t, 1, ws.updateAgentCalls)
}

func TestInstructionMutationCompletionSeparatesDialogAndExecutionLifetimes(t *testing.T) {
	for _, mode := range []string{"rejected", "closed", "replaced-dialog", "closed-rejected", "replaced-owner", "replaced-model"} {
		t.Run(mode, func(t *testing.T) {
			ui, ws, instructions := newInstructionMutationUI(t)
			if mode == "rejected" || mode == "closed-rejected" {
				ws.mutationErr = errors.New("synthetic acknowledgement rejection")
			}
			messages := collectCommandMessages(ui.handleDialogAction(selectInstructionNative(t, instructions)))
			completed := messages[0].(dialog.ActionInstructionMutationCompleted)
			switch mode {
			case "closed", "closed-rejected":
				ui.dialog.CloseDialog(dialog.InstructionsID)
			case "replaced-dialog":
				ui.dialog.CloseDialog(dialog.InstructionsID)
				ui.dialog.OpenDialog(dialog.NewInstructions(ui.com))
			case "replaced-owner":
				owner := *ws.surfaces[0].Owner
				owner.ManifestVersion = "2.0.0"
				ws.surfaces[0].Owner = &owner
			case "replaced-model":
				ws.cfg.Models[config.SelectedModelTypeLarge] = config.SelectedModel{Provider: "synthetic", Model: "replacement"}
			}
			_, command := ui.Update(completed)
			result := collectCommandMessages(command)
			if mode == "closed" || mode == "replaced-dialog" {
				require.Equal(t, 1, ws.updateAgentCalls)
			} else {
				require.Zero(t, ws.updateAgentCalls)
			}
			if mode == "rejected" || mode == "closed-rejected" {
				requireCommandError(t, result, "synthetic acknowledgement rejection")
			}
			if mode == "replaced-owner" || mode == "replaced-model" {
				requireCommandError(t, result, "selection changed")
			}
		})
	}
}

func TestInstructionProviderEditorExitPublishesBeforeRebuild(t *testing.T) {
	// home.Dir is initialized at process startup; isolate it before starting
	// this production-path file preparation test.
	if os.Getenv("CRUX_UI_EDITOR_FIXTURE") != "1" {
		command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestInstructionProviderEditorExitPublishesBeforeRebuild$", "-test.count=1")
		command.Env = append(os.Environ(), "HOME="+t.TempDir(), "CRUX_UI_EDITOR_FIXTURE=1")
		output, err := command.CombinedOutput()
		require.NoError(t, err, "%s", output)
		return
	}
	for _, mode := range []string{"success", "editor-error", "reload-error", "closed", "replaced-dialog", "closed-editor-error", "closed-reload-error", "closed-before-launch", "owner-changed"} {
		t.Run(mode, func(t *testing.T) {
			ui, ws, instructions := newInstructionMutationUI(t)
			for range 4 {
				instructions.HandleMsg(tea.KeyPressMsg{Code: tea.KeyDown})
			}
			action := instructions.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}).(dialog.ActionCmd)
			prepared := action.Cmd().(dialog.ActionInstructionEditorPrepared)
			require.NoError(t, prepared.Err)
			require.NotNil(t, prepared.Cmd)
			require.Zero(t, ws.reloadCalls)
			if mode == "closed-before-launch" {
				ui.dialog.CloseDialog(dialog.InstructionsID)
				_, command := ui.Update(prepared)
				require.Empty(t, collectCommandMessages(command), "closed preparation must not launch terminal editor")
				require.ErrorContains(t, instructions.CheckOperation(prepared.Operation), "no longer current")
				require.Zero(t, ws.reloadCalls)
				require.Zero(t, ws.updateAgentCalls)
				return
			}
			_, editorCommand := ui.Update(prepared)
			require.NotNil(t, editorCommand, "preparation must reach top-level editor dispatch")
			// Bubble Tea owns executing the terminal editor. Simulate its
			// completion so tests never launch an interactive process.
			exited := dialog.ActionInstructionEditorExited{Operation: prepared.Operation}
			switch mode {
			case "editor-error", "closed-editor-error":
				exited.Err = errors.New("fixture editor failed")
			case "reload-error", "closed-reload-error":
				ws.mutationErr = errors.New("fixture publication rejected")
			case "closed":
				ui.dialog.CloseDialog(dialog.InstructionsID)
			case "replaced-dialog":
				ui.dialog.CloseDialog(dialog.InstructionsID)
				ui.dialog.OpenDialog(dialog.NewInstructions(ui.com))
			case "owner-changed":
				owner := *ws.surfaces[0].Owner
				owner.ManifestVersion = "2.0.0"
				ws.surfaces[0].Owner = &owner
			}
			if mode == "closed-editor-error" || mode == "closed-reload-error" {
				ui.dialog.CloseDialog(dialog.InstructionsID)
			}
			_, command := ui.Update(exited)
			require.Zero(t, ws.reloadCalls, "publication must run in a command")
			messages := collectCommandMessages(command)
			if mode == "success" || mode == "reload-error" || mode == "closed" || mode == "replaced-dialog" || mode == "closed-reload-error" {
				require.Equal(t, 1, ws.reloadCalls)
				require.Equal(t, *ws.surfaces[0].Owner, ws.toolingOwner)
				require.Zero(t, ws.updateAgentCalls)
				require.Len(t, messages, 1)
				completed := messages[0].(dialog.ActionInstructionMutationCompleted)
				_, command = ui.Update(completed)
				result := collectCommandMessages(command)
				if mode == "success" || mode == "closed" || mode == "replaced-dialog" {
					require.Equal(t, 1, ws.updateAgentCalls)
				} else {
					requireCommandError(t, result, "fixture publication rejected")
					require.Zero(t, ws.updateAgentCalls)
				}
			} else {
				require.Zero(t, ws.reloadCalls)
				require.Zero(t, ws.updateAgentCalls)
				if mode == "editor-error" || mode == "closed-editor-error" {
					requireCommandError(t, messages, "fixture editor failed")
				}
				if mode == "owner-changed" {
					requireCommandError(t, messages, "selection changed")
				}
			}
		})
	}
}

func TestInstructionNativeToggleUsesWorkspaceSDK(t *testing.T) {
	for _, mode := range []string{"accepted", "rejected", "closed", "replaced-dialog", "closed-rejected"} {
		rejected := mode == "rejected" || mode == "closed-rejected"
		t.Run(mode, func(t *testing.T) {
			initial := instructionUISnapshot(t, config.ToolingInstructionsCrux)
			acknowledged := instructionUISnapshot(t, config.ToolingInstructionsNative)
			acknowledged.Path = initial.Path
			var writes, rebuilds atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "PUT /v1/workspaces/instructions/config/provider-tooling":
					writes.Add(1)
					var request proto.ProviderToolingRequest
					require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
					require.Equal(t, *initial.ProviderSurfaces[0].Owner, request.Owner)
					require.NotNil(t, request.Scope)
					require.Equal(t, config.ScopeGlobal, *request.Scope)
					require.Equal(t, config.ToolingInstructionsNative, request.Profile)
					if rejected {
						w.WriteHeader(http.StatusBadRequest)
						_ = json.NewEncoder(w).Encode(proto.Error{Message: "fixture tooling rejected"})
						return
					}
					require.NoError(t, json.NewEncoder(w).Encode(proto.ProviderToolingState{Scope: *request.Scope, Owner: request.Owner, Profile: request.Profile}))
				case "GET /v1/workspaces/instructions":
					require.NoError(t, json.NewEncoder(w).Encode(acknowledged))
				case "POST /v1/workspaces/instructions/agent/update":
					rebuilds.Add(1)
					var request proto.AgentUpdateRequest
					require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
					require.Equal(t, acknowledged.Config.AgentModelState(), request.State)
					w.WriteHeader(http.StatusOK)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()
			address, err := url.Parse(srv.URL)
			require.NoError(t, err)
			sdk, err := client.NewClient(initial.Path, "tcp", address.Host)
			require.NoError(t, err)
			retained := workspace.NewClientWorkspace(sdk, initial)
			ui := newTestUI()
			ui.focus = uiFocusNone
			ui.agentBusyCache.set(false)
			ui.yoloCache.set(false)
			ui.lspCheckedAt = time.Now()
			ui.com.Workspace = retained
			ui.dialog = dialog.NewOverlay()
			instructions := dialog.NewInstructions(ui.com)
			ui.dialog.OpenDialog(instructions)
			command := ui.handleDialogAction(selectInstructionNative(t, instructions))
			require.Zero(t, writes.Load())
			completed := collectCommandMessages(command)[0].(dialog.ActionInstructionMutationCompleted)
			require.EqualValues(t, 1, writes.Load())
			require.Zero(t, rebuilds.Load())
			if mode == "closed" || mode == "closed-rejected" {
				ui.dialog.CloseDialog(dialog.InstructionsID)
			}
			if mode == "replaced-dialog" {
				ui.dialog.CloseDialog(dialog.InstructionsID)
				ui.dialog.OpenDialog(dialog.NewInstructions(ui.com))
			}
			_, command = ui.Update(completed)
			messages := collectCommandMessages(command)
			if rejected {
				requireCommandError(t, messages, "fixture tooling rejected")
				require.Zero(t, rebuilds.Load())
				provider, _ := retained.Config().Providers.Get("synthetic")
				require.Equal(t, config.ToolingInstructionsCrux, provider.ToolingInstructions)
			} else {
				require.EqualValues(t, 1, rebuilds.Load())
			}
		})
	}
}
