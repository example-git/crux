package model

import (
	"encoding/json"
	"image"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

func runtimeControlUISnapshot(t *testing.T, kind string, inherited any) proto.Workspace {
	t.Helper()
	snapshot := instructionUISnapshot(t, config.ToolingInstructionsCrux)
	control := manifest.RuntimeControl{ID: "vendor.mode", Label: "Vendor mode", Type: kind, Scope: "model", RequestPath: "/vendor/mode"}
	binding := providerregistry.RuntimeControlBinding{Kind: providerregistry.RuntimeControlModelOption}
	snapshot.ProviderSurfaces[0].RuntimeControls = []providerregistry.RuntimeControlSurface{{RuntimeControl: control, Available: true, AvailableModels: []string{"fixture"},
		Binding: &binding, DescriptorDigest: providerregistry.RuntimeControlDescriptorDigest(control, binding)}}
	provider, _ := snapshot.Config.Providers.Get("synthetic")
	provider.Models = []catalog.Model{{ID: "fixture", Name: "Fixture"}}
	provider.ProviderOptions = map[string]any{control.ID: inherited}
	snapshot.Config.Providers.Set("synthetic", provider)
	return snapshot
}

func runtimeControlUIState(t *testing.T, current proto.Workspace, target config.RuntimeControlTarget) config.RuntimeControlState {
	t.Helper()
	state, err := config.RuntimeControlEffectiveStateForSurface(current.Config, config.ScopeGlobal, target, current.ProviderSurfaces[0])
	require.NoError(t, err)
	state.ScopedKnown = true
	if value, exists := current.Config.Models[config.SelectedModelTypeLarge].ProviderOptions[target.ControlID]; exists {
		raw, err := json.Marshal(value)
		require.NoError(t, err)
		state.Scoped = config.RuntimeControlValue{Present: true, Value: raw}
	}
	return state
}

func drawRuntimeControlDialog(instructions *dialog.Instructions) string {
	screen := uv.NewScreenBuffer(110, 55)
	instructions.Draw(screen, image.Rect(0, 0, 110, 55))
	return ansi.Strip(screen.String())
}

func runtimeControlUICommandMessages(command tea.Cmd) []tea.Msg {
	var messages []tea.Msg
	for _, message := range collectCommandMessages(command) {
		switch message.(type) {
		case busyStateMsg, lspStatesMsg:
			// The Update TTL backstop is independent of instruction control
			// completion. Its command still executes through the actual SDK.
		default:
			messages = append(messages, message)
		}
	}
	return messages
}

func TestInstructionRuntimeControlUsesWorkspaceSDKAndMainUICompletion(t *testing.T) {
	for _, tc := range []struct {
		name, kind, input string
		inherited         any
	}{
		{"accepted", "string", "new", "old"},
		{"fallback", "string", "new", "old"},
		{"false", "boolean", "false", true},
		{"zero", "integer", "0", 7},
		{"empty", "string", "", "old"},
		{"reset", "string", "", "provider fallback"},
		{"rejected", "string", "new", "old"},
		{"closed", "string", "new", "old"},
		{"replaced-dialog", "string", "new", "old"},
		{"closed-rejected", "string", "new", "old"},
		{"changed-definition", "string", "new", "old"},
		{"read-rejected", "string", "new", "old"},
		{"closed-read-rejected", "string", "new", "old"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			initial := runtimeControlUISnapshot(t, tc.kind, tc.inherited)
			current := runtimeControlUISnapshot(t, tc.kind, tc.inherited)
			current.Path = initial.Path
			if tc.name == "fallback" {
				for _, snapshot := range []*proto.Workspace{&initial, &current} {
					provider, _ := snapshot.Config.Providers.Get("synthetic")
					provider.ProviderOptions = nil
					snapshot.Config.Providers.Set("synthetic", provider)
					control := &snapshot.ProviderSurfaces[0].RuntimeControls[0]
					control.Default = tc.inherited
					control.Binding.FallbackMode = "before-request-transform"
					control.DescriptorDigest = providerregistry.RuntimeControlDescriptorDigest(control.RuntimeControl, *control.Binding)
				}
			}
			if tc.name == "reset" {
				for _, snapshot := range []*proto.Workspace{&initial, &current} {
					selected := snapshot.Config.Models[config.SelectedModelTypeLarge]
					selected.ProviderOptions = map[string]any{"vendor.mode": "saved"}
					snapshot.Config.Models[config.SelectedModelTypeLarge] = selected
				}
			}
			var reads, writes, rebuilds atomic.Int32
			rejected := strings.Contains(tc.name, "rejected")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "POST /v1/workspaces/instructions/config/runtime-control/resolve":
					reads.Add(1)
					if strings.HasSuffix(tc.name, "read-rejected") {
						w.WriteHeader(http.StatusConflict)
						_ = json.NewEncoder(w).Encode(proto.Error{Message: "fixture saved value pending acknowledgement"})
						return
					}
					var request proto.RuntimeControlRequest
					require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
					require.NotNil(t, request.Scope)
					require.Equal(t, config.ScopeGlobal, *request.Scope)
					require.NoError(t, json.NewEncoder(w).Encode(runtimeControlUIState(t, current, request.Target)))
				case "DELETE /v1/workspaces/instructions/config/runtime-control":
					writes.Add(1)
					var request proto.RuntimeControlRequest
					require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
					require.Equal(t, "vendor.mode", request.Target.ControlID)
					require.Equal(t, *initial.ProviderSurfaces[0].Owner, request.Target.Owner)
					require.Equal(t, initial.ProviderSurfaces[0].RuntimeControls[0].DescriptorDigest, request.Target.DescriptorDigest)
					require.NotNil(t, request.Scope)
					require.Equal(t, config.ScopeGlobal, *request.Scope)
					selected := current.Config.Models[config.SelectedModelTypeLarge]
					delete(selected.ProviderOptions, request.Target.ControlID)
					current.Config.Models[config.SelectedModelTypeLarge] = selected
					require.NoError(t, json.NewEncoder(w).Encode(runtimeControlUIState(t, current, request.Target)))
				case "PUT /v1/workspaces/instructions/config/runtime-control":
					writes.Add(1)
					var request proto.SetRuntimeControlRequest
					require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
					require.Equal(t, "vendor.mode", request.Target.ControlID)
					require.Equal(t, *initial.ProviderSurfaces[0].Owner, request.Target.Owner)
					require.Equal(t, initial.ProviderSurfaces[0].RuntimeControls[0].DescriptorDigest, request.Target.DescriptorDigest)
					require.NotNil(t, request.Scope)
					require.Equal(t, config.ScopeGlobal, *request.Scope)
					if rejected {
						w.WriteHeader(http.StatusConflict)
						_ = json.NewEncoder(w).Encode(proto.Error{Message: "fixture control rejected"})
						return
					}
					var value any
					decoder := json.NewDecoder(strings.NewReader(string(request.Value)))
					decoder.UseNumber()
					require.NoError(t, decoder.Decode(&value))
					selected := current.Config.Models[config.SelectedModelTypeLarge]
					selected.ProviderOptions = map[string]any{request.Target.ControlID: value}
					current.Config.Models[config.SelectedModelTypeLarge] = selected
					require.NoError(t, json.NewEncoder(w).Encode(runtimeControlUIState(t, current, request.Target)))
				case "GET /v1/workspaces/instructions":
					require.NoError(t, json.NewEncoder(w).Encode(current))
				case "GET /v1/workspaces/instructions/agent":
					require.NoError(t, json.NewEncoder(w).Encode(proto.AgentInfo{}))
				case "GET /v1/workspaces/instructions/permissions/skip":
					require.NoError(t, json.NewEncoder(w).Encode(proto.PermissionSkipRequest{}))
				case "POST /v1/workspaces/instructions/agent/update":
					rebuilds.Add(1)
					var request proto.AgentUpdateRequest
					require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
					actual, err := json.Marshal(request.State)
					require.NoError(t, err)
					want, err := json.Marshal(current.Config.AgentModelState())
					require.NoError(t, err)
					require.True(t, config.RuntimeControlJSONEqual(actual, want))
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
			// Open through the production method: it must schedule actual typed
			// resolution and leave the request count untouched until execution.
			command := ui.openInstructionsDialog()
			instructions := ui.dialog.Dialog(dialog.InstructionsID).(*dialog.Instructions)
			require.Zero(t, reads.Load())
			for range 5 {
				instructions.HandleMsg(tea.KeyPressMsg{Code: tea.KeyDown})
			}
			require.Contains(t, drawRuntimeControlDialog(instructions), "Loading")
			loaded := runtimeControlUICommandMessages(command)[0].(dialog.ActionInstructionControlsLoaded)
			require.Contains(t, drawRuntimeControlDialog(instructions), "Loading", "read command cannot edit UI state")
			if tc.name == "closed-read-rejected" {
				ui.dialog.CloseDialog(dialog.InstructionsID)
			}
			_, command = ui.Update(loaded)
			if strings.HasSuffix(tc.name, "read-rejected") {
				requireCommandError(t, runtimeControlUICommandMessages(command), "pending acknowledgement")
				require.Zero(t, writes.Load())
				require.Zero(t, rebuilds.Load())
				return
			}
			require.Empty(t, runtimeControlUICommandMessages(command))
			if tc.name == "reset" {
				require.Contains(t, drawRuntimeControlDialog(instructions), "saved · model")
				command = ui.handleDialogAction(instructions.HandleMsg(tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl}))
			} else {
				if tc.name == "fallback" {
					require.Contains(t, drawRuntimeControlDialog(instructions), "At request time · configured fallback: old · manifest")
				} else {
					require.Contains(t, drawRuntimeControlDialog(instructions), "provider · inherited")
				}
				require.Nil(t, instructions.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}))
				if tc.input != "" {
					instructions.HandleMsg(tea.KeyPressMsg{Code: []rune(tc.input)[0], Text: tc.input})
				}
				command = ui.handleDialogAction(instructions.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}))
			}
			require.Zero(t, writes.Load())
			completed := runtimeControlUICommandMessages(command)[0].(dialog.ActionInstructionMutationCompleted)
			require.EqualValues(t, 1, writes.Load())
			require.Zero(t, rebuilds.Load())
			switch tc.name {
			case "closed", "closed-rejected":
				ui.dialog.CloseDialog(dialog.InstructionsID)
			case "replaced-dialog":
				ui.dialog.CloseDialog(dialog.InstructionsID)
				ui.dialog.OpenDialog(dialog.NewInstructions(ui.com))
			case "changed-definition":
				// Admit a later descriptor through the same actual SDK before
				// delivering the older mutation completion to the UI.
				control := &current.ProviderSurfaces[0].RuntimeControls[0]
				control.Description = "Changed while the mutation completion was queued"
				control.DescriptorDigest = providerregistry.RuntimeControlDescriptorDigest(control.RuntimeControl, *control.Binding)
				target := completed.ControlState.Target
				target.DescriptorDigest = ""
				_, err := retained.RuntimeControlState(t.Context(), config.ScopeGlobal, target)
				require.NoError(t, err)
			}
			_, command = ui.Update(completed)
			require.Zero(t, rebuilds.Load(), "rebuild must be scheduled, not run in Update")
			messages := runtimeControlUICommandMessages(command)
			if rejected {
				requireCommandError(t, messages, "fixture control rejected")
				require.Zero(t, rebuilds.Load())
				return
			}
			if tc.name == "changed-definition" {
				requireCommandError(t, messages, "definition changed")
				require.Zero(t, rebuilds.Load())
				return
			}
			require.EqualValues(t, 1, rebuilds.Load())
			if tc.name == "closed" || tc.name == "replaced-dialog" {
				require.EqualValues(t, 1, reads.Load(), "closed dialog must not refresh its display")
				return
			}
			if tc.name == "reset" {
				require.Contains(t, drawRuntimeControlDialog(instructions), "provider fallback · provider · inherited")
			} else {
				require.Contains(t, drawRuntimeControlDialog(instructions), " · model")
				require.NotContains(t, drawRuntimeControlDialog(instructions), "At request time")
			}
			require.Len(t, messages, 1, "successful metadata edit refreshes effective sibling controls")
			_, command = ui.Update(messages[0])
			require.Empty(t, runtimeControlUICommandMessages(command))
			require.EqualValues(t, 2, reads.Load())
			if tc.name == "empty" {
				require.Contains(t, drawRuntimeControlDialog(instructions), `"" · model`)
			}
		})
	}
}
