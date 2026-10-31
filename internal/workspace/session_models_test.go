package workspace

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerregistry"

	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/session"
	"github.com/stretchr/testify/require"
)

func TestSessionModelsSurviveSessionProtocolRoundTrip(t *testing.T) {
	models := json.RawMessage(`{"large":{"model":{"provider":"first","model":"first-model","provider_options":{"precise":9007199254740993}}},"small":{"model":{"provider":"first","model":"small-model"}}}`)
	current := session.Session{ID: "goal-session", Models: models, ModelRevision: 7, Goal: session.Goal{
		Objective:  "finish the work",
		Status:     session.GoalStatusActive,
		Generation: 3,
	}}
	encoded, err := json.Marshal(sessionToProto(current))
	require.NoError(t, err)
	var wire proto.Session
	require.NoError(t, json.Unmarshal(encoded, &wire))
	require.JSONEq(t, string(models), string(wire.Models))
	require.Equal(t, current.Goal, protoToSession(wire).Goal)
	require.Equal(t, current.Models, protoToSession(wire).Models)
	require.Equal(t, current.ModelRevision, protoToSession(wire).ModelRevision)
}

func TestClientWorkspaceSessionModelSelectionUsesDedicatedOperation(t *testing.T) {
	models := config.AgentModelState{Large: &config.OwnedSelectedModel{Model: config.SelectedModel{Provider: "first", Model: "main", ProviderOptions: map[string]any{"precise": json.Number("9007199254740993")}}, Owner: providerregistry.RegistrationOwner{ProviderID: "first"}}}
	models.Small = models.Large
	data, err := json.Marshal(models)
	require.NoError(t, err)
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPut, r.Method)
		require.Equal(t, "/v1/workspaces/workspace/sessions/current", r.URL.Path)
		var body map[string]json.RawMessage
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Len(t, body, 1)
		var operation struct {
			Models   json.RawMessage `json:"models"`
			Revision int64           `json:"revision"`
		}
		require.NoError(t, json.Unmarshal(body["model_selection"], &operation))
		require.JSONEq(t, string(data), string(operation.Models))
		require.EqualValues(t, 3, operation.Revision)
		require.NoError(t, json.NewEncoder(w).Encode(proto.Session{ID: "current", Models: data, ModelRevision: 4, Goal: proto.Goal{Objective: "finish", Status: "paused", Generation: 2}}))
	}))
	defer host.Close()
	address, err := url.Parse(host.URL)
	require.NoError(t, err)
	remote, err := client.NewClient(t.TempDir(), "tcp", address.Host)
	require.NoError(t, err)
	ws := NewClientWorkspace(remote, proto.Workspace{ID: "workspace"})
	current, err := ws.SetSessionModels(t.Context(), "current", 3, models)
	require.NoError(t, err)
	require.EqualValues(t, 4, current.ModelRevision)
	require.JSONEq(t, string(data), string(current.Models))
	require.Equal(t, session.GoalStatusPaused, current.Goal.Status)
	require.EqualValues(t, 2, current.Goal.Generation)
}
