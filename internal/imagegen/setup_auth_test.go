package imagegen

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/cookieutil"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/question"
	"github.com/stretchr/testify/require"
)

func TestImageSetupBrowserAuthentication(t *testing.T) {
	service, source := imageSetupFixture(t)
	data, err := os.ReadFile(filepath.Join(source, "manifest.json"))
	require.NoError(t, err)
	var value manifest.ImageManifest
	require.NoError(t, json.Unmarshal(data, &value))
	value.Credentials = []manifest.ImageCredential{{ID: "browser", Source: "browser", Domains: []string{"images.example.test"}}}
	value.Origins[0].Credentials = []string{"browser"}
	data, err = json.Marshal(value)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(source, "manifest.json"), data, 0o600))
	_, err = service.Runtime.Manager.Install(t.Context(), providerplugin.InstallRequest{Source: source, Trust: true})
	require.NoError(t, err)
	owner, err := service.Runtime.Manager.CaptureImageOwner(value.Backend)
	require.NoError(t, err)
	profileID := strings.Repeat("a", 64)
	service.browserProfiles = func() []cookieutil.BrowserProfile {
		return []cookieutil.BrowserProfile{{ID: profileID, Name: "Fixture browser"}}
	}
	questions := 0
	service.Questions = imageSetupQuestions{ask: func(ctx context.Context, request question.Request) ([]question.Answer, error) {
		questions++
		require.NoError(t, request.Validate())
		require.Equal(t, "parent", request.SessionID)
		require.Equal(t, "call", request.ToolCallID)
		require.Len(t, request.Questions, 1)
		require.Equal(t, profileID, request.Questions[0].Choices[0].ID)
		return []question.Answer{{QuestionID: request.Questions[0].ID, SelectedIDs: []string{profileID}}}, nil
	}}
	resolutions := 0
	service.Runtime.ResolveCredentials = func(ctx context.Context, bundle providerplugin.RegisteredImageBundle) (PluginCredentials, error) {
		resolutions++
		require.Equal(t, owner, bundle.Owner())
		require.Equal(t, profileID, service.Store.ImageConfiguration().Providers[owner.Backend].BrowserProfiles["browser"])
		return PluginCredentials{Identity: "memory-only", Validate: func() error { return nil }}, nil
	}
	err = service.Authenticate(t.Context(), SetupRequest{}, owner)
	require.ErrorContains(t, err, "explicit browser_profiles binding")
	require.Zero(t, questions)
	require.Zero(t, resolutions)
	require.NoError(t, service.Authenticate(t.Context(), SetupRequest{Interactive: true, SessionID: "parent", ToolCallID: "call"}, owner))
	require.Equal(t, 1, questions)
	require.Equal(t, 1, resolutions)
	stored, err := json.Marshal(service.Store.ImageConfiguration())
	require.NoError(t, err)
	require.Contains(t, string(stored), profileID)
	require.NotContains(t, string(stored), "memory-only")
	require.NoError(t, service.Authenticate(t.Context(), SetupRequest{}, owner))
	require.Equal(t, 1, questions)
	require.Equal(t, 2, resolutions)
	wrong := owner
	wrong.Digest = strings.Repeat("b", 64)
	require.Error(t, service.Authenticate(t.Context(), SetupRequest{}, wrong))
	require.Equal(t, 2, resolutions)
}

func TestImageBrowserProfileSelectionRejectsCancellationAndInvalidAnswers(t *testing.T) {
	service, _ := imageSetupFixture(t)
	profiles := []cookieutil.BrowserProfile{{ID: strings.Repeat("a", 64), Name: "Fixture browser"}}
	declaration := manifest.ImageCredential{ID: "browser", Source: "browser"}
	request := SetupRequest{Interactive: true, SessionID: "parent"}
	service.Questions = imageSetupQuestions{ask: func(context.Context, question.Request) ([]question.Answer, error) {
		return nil, question.ErrCancelled
	}}
	_, err := service.selectBrowserProfile(t.Context(), request, declaration, profiles)
	require.ErrorIs(t, err, question.ErrCancelled)
	service.Questions = imageSetupQuestions{ask: func(ctx context.Context, request question.Request) ([]question.Answer, error) {
		return []question.Answer{{QuestionID: request.Questions[0].ID, SelectedIDs: []string{"next"}}}, nil
	}}
	_, err = service.selectBrowserProfile(t.Context(), request, declaration, profiles)
	require.ErrorContains(t, err, "unavailable")
}
