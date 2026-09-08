package imagegen

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/cookieutil"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/question"
)

func (s *SetupService) Authenticate(ctx context.Context, request SetupRequest, owner providerplugin.ImageOwner) error {
	if s == nil || s.Runtime == nil || s.Runtime.Manager == nil || s.Store == nil {
		return ErrSetupRequired
	}
	if err := s.Runtime.Manager.ValidateImageOwner(ctx, owner); err != nil {
		return err
	}
	bundle, err := s.Runtime.Manager.ImageBundleForOwner(owner)
	if err != nil {
		return err
	}
	expected := s.Store.ImageConfiguration()
	value := s.Store.ImageConfiguration()
	if value == nil {
		value = &config.ImageConfiguration{}
	}
	provider, exists := value.Providers[owner.Backend]
	if exists && provider.Owner != owner {
		return errors.New("configured image owner changed before authentication")
	}
	provider.Owner = owner
	changed, err := s.selectBrowserProfiles(ctx, request, bundle, &provider)
	if err != nil {
		return err
	}
	if changed {
		if value.Providers == nil {
			value.Providers = map[string]config.ImageProviderConfiguration{}
		}
		value.Providers[owner.Backend] = provider
		if err := s.Runtime.Manager.ValidateImageOwner(ctx, owner); err != nil {
			return err
		}
		if err := s.Store.CompareAndSetImageConfiguration(expected, value); err != nil {
			return err
		}
	}
	if s.Runtime.ResolveCredentials == nil {
		return errors.New("image credential resolver is unavailable")
	}
	credentials, err := s.Runtime.ResolveCredentials(ctx, bundle)
	if err != nil {
		return fmt.Errorf("image provider authentication failed: %w", err)
	}
	if err := s.Runtime.Manager.ValidateImageOwner(ctx, owner); err != nil {
		return err
	}
	if credentials.Validate != nil {
		return credentials.Validate()
	}
	return nil
}

func (s *SetupService) selectBrowserProfiles(ctx context.Context, request SetupRequest, bundle providerplugin.RegisteredImageBundle, provider *config.ImageProviderConfiguration) (bool, error) {
	changed := false
	for _, declaration := range bundle.Manifest.Credentials {
		if declaration.Source != "browser" || provider.BrowserProfiles[declaration.ID] != "" {
			continue
		}
		if !request.Interactive || s.Questions == nil || request.SessionID == "" {
			return false, fmt.Errorf("image browser authentication for %q requires an explicit browser_profiles binding or interactive image setup", declaration.ID)
		}
		var profiles []cookieutil.BrowserProfile
		if s.browserProfiles != nil {
			profiles = s.browserProfiles()
		} else {
			profiles = cookieutil.BrowserProfiles(s.Store.HostEnvironment())
		}
		selected, err := s.selectBrowserProfile(ctx, request, declaration, profiles)
		if err != nil {
			return false, err
		}
		if provider.BrowserProfiles == nil {
			provider.BrowserProfiles = map[string]string{}
		}
		provider.BrowserProfiles[declaration.ID] = selected
		changed = true
	}
	return changed, nil
}

func (s *SetupService) selectBrowserProfile(ctx context.Context, request SetupRequest, declaration manifest.ImageCredential, profiles []cookieutil.BrowserProfile) (string, error) {
	if len(profiles) == 0 {
		return "", errors.New("no browser profiles are available on the execution host; open and sign in to the image provider in your browser")
	}
	for start := 0; ; {
		end := min(start+2, len(profiles))
		choices := make([]question.Choice, 0, 5)
		for _, profile := range profiles[start:end] {
			label := profile.Name
			if len(label) > question.MaxChoiceLabelLength {
				label = strings.ToValidUTF8(label[:question.MaxChoiceLabelLength-3], "") + "..."
			}
			choices = append(choices, question.Choice{ID: profile.ID, Label: label})
		}
		if start > 0 {
			choices = append(choices, question.Choice{ID: "previous", Label: "Previous profiles"})
		}
		if end < len(profiles) {
			choices = append(choices, question.Choice{ID: "next", Label: "More profiles"})
		}
		choices = append(choices, question.Choice{ID: "cancel", Label: "Cancel authentication"})
		id := "image-browser-" + declaration.ID
		answers, err := s.Questions.Ask(ctx, question.Request{SessionID: request.SessionID, ToolCallID: request.ToolCallID, Questions: []question.Question{{ID: id, Type: question.TypeSingleChoice, Text: "Authenticate this image provider from a browser?", Description: "Choose its signed-in browser profile. Setup saves only the profile ID. Cookies are imported on use and retained in memory for this provider; they are never saved or written back to the browser.", Choices: choices}}})
		if err != nil {
			return "", err
		}
		if len(answers) != 1 || answers[0].QuestionID != id || len(answers[0].SelectedIDs) != 1 || answers[0].FillInText != "" {
			return "", errors.New("image browser authentication received an invalid selection")
		}
		selected := answers[0].SelectedIDs[0]
		switch {
		case selected == "cancel":
			return "", question.ErrCancelled
		case selected == "previous" && start > 0:
			start -= 2
		case selected == "next" && end < len(profiles):
			start = end
		default:
			for _, profile := range profiles[start:end] {
				if profile.ID == selected {
					return selected, nil
				}
			}
			return "", errors.New("selected image browser profile is unavailable")
		}
	}
}
