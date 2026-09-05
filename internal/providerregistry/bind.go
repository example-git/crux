package providerregistry

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/manifestflow"
	"github.com/example-git/crux/internal/providerplugin/manifest"
)

func BindRegistrationConfiguration(registration Registration, configuration map[string]any) (Registration, error) {
	registration = registration.Clone()
	if registration.Manifest == nil || registration.OAuth == nil || len(registration.Manifest.Capabilities.OAuth) == 0 {
		return registration, nil
	}
	if compatibility := registration.Manifest.Capabilities.Compatibility; compatibility != nil && slices.Contains(compatibility.Delegates, "oauth") {
		return registration, nil
	}
	var flow *manifest.OAuthFlow
	for i := range registration.Manifest.Capabilities.OAuth {
		candidate := &registration.Manifest.Capabilities.OAuth[i]
		if candidate.ID == registration.OAuth.FlowID {
			flow = candidate
			break
		}
	}
	if flow == nil {
		return Registration{}, fmt.Errorf("provider %q OAuth flow %q is unavailable", registration.ProviderID, registration.OAuth.FlowID)
	}
	credentials := map[string]string{}
	for _, credential := range registration.Manifest.Capabilities.Credentials {
		if credential.ConfigProperty == "" {
			continue
		}
		value, ok := configuration[credential.ConfigProperty]
		if !ok {
			return Registration{}, fmt.Errorf("provider %q credential %q requires configuration property %q", registration.ProviderID, credential.ID, credential.ConfigProperty)
		}
		text, ok := value.(string)
		if !ok || text == "" {
			return Registration{}, fmt.Errorf("provider %q credential %q configuration property %q must be a non-empty string", registration.ProviderID, credential.ID, credential.ConfigProperty)
		}
		credentials[credential.ID] = text
	}
	if err := validateOAuthBindings(registration.ProviderID, *flow, configuration, credentials); err != nil {
		return Registration{}, err
	}
	capability, err := manifestOAuthCapability(*registration.Manifest, *flow, manifestflow.Bindings{
		Configuration: maps.Clone(configuration),
		Credentials:   credentials,
	})
	if err != nil {
		return Registration{}, err
	}
	registration.OAuth = capability
	return registration, nil
}

func validateOAuthBindings(providerID string, flow manifest.OAuthFlow, configuration map[string]any, credentials map[string]string) error {
	var validate func(manifest.Template) error
	validate = func(value manifest.Template) error {
		switch value.Kind {
		case "config":
			if _, ok := configuration[value.Ref]; !ok {
				return fmt.Errorf("provider %q OAuth configuration property %q is unavailable", providerID, value.Ref)
			}
		case "credential":
			if credentials[value.Ref] == "" {
				return fmt.Errorf("provider %q OAuth credential %q is unavailable", providerID, value.Ref)
			}
		case "concat":
			for _, part := range value.Parts {
				if err := validate(part); err != nil {
					return err
				}
			}
		}
		return nil
	}
	validateHeaders := func(rules []manifest.HeaderRule) error {
		for _, rule := range rules {
			if rule.Value != nil {
				if err := validate(*rule.Value); err != nil {
					return err
				}
			}
		}
		return nil
	}
	validateFields := func(rules []manifest.FieldRule) error {
		for _, rule := range rules {
			if err := validate(rule.Value); err != nil {
				return err
			}
		}
		return nil
	}
	if err := validate(flow.ClientID); err != nil {
		return err
	}
	if flow.ClientSecret != nil {
		if err := validate(*flow.ClientSecret); err != nil {
			return err
		}
	}
	for _, rule := range flow.AuthorizationParams {
		if err := validate(rule.Value); err != nil {
			return err
		}
	}
	if err := validateFields(flow.TokenRequest.Code); err != nil {
		return err
	}
	if err := validateFields(flow.TokenRequest.Refresh); err != nil {
		return err
	}
	if err := validateHeaders(flow.TokenRequest.Headers); err != nil {
		return err
	}
	if flow.DeviceCode != nil {
		if err := validateFields(flow.DeviceCode.Request); err != nil {
			return err
		}
		if err := validateFields(flow.DeviceCode.Poll); err != nil {
			return err
		}
		if err := validateHeaders(flow.DeviceCode.Headers); err != nil {
			return err
		}
	}
	return nil
}

func manifestOAuthCapability(value manifest.Manifest, flow manifest.OAuthFlow, bindings ...manifestflow.Bindings) (*OAuthCapability, error) {
	adapter := LoginBrowser
	switch flow.Redirect.Mode {
	case "hosted-paste":
		adapter = LoginHostedPaste
	case "device-code":
		adapter = LoginDeviceCode
	}
	executor, err := manifestflow.New(value, flow, bindings...)
	if err != nil {
		return nil, fmt.Errorf("compile provider %q OAuth flow: %w", value.Provider.ID, err)
	}
	capability := &OAuthCapability{
		Adapter: adapter,
		FlowID:  flow.ID,
		Authorize: func(ctx context.Context, open OpenURL, read ReadCode) (*oauth.Token, error) {
			return executor.Authorize(ctx, open, read)
		},
		Refresh: executor.Refresh,
	}
	if flow.Redirect.Mode != "device-code" {
		return capability, nil
	}
	capability.Authorize = nil
	capability.RequestDeviceCode = func(ctx context.Context) (*DeviceAuthorization, error) {
		authorization, err := executor.RequestDeviceCode(ctx)
		if err != nil {
			return nil, err
		}
		return &DeviceAuthorization{UserCode: authorization.UserCode, VerificationURL: authorization.VerificationURL, State: authorization}, nil
	}
	capability.PollDeviceCode = func(ctx context.Context, authorization *DeviceAuthorization) (*oauth.Token, error) {
		if authorization == nil {
			return nil, fmt.Errorf("device authorization is missing")
		}
		state, ok := authorization.State.(*manifestflow.DeviceAuthorization)
		if !ok {
			return nil, fmt.Errorf("device authorization state is invalid")
		}
		return executor.PollDeviceCode(ctx, state)
	}
	return capability, nil
}
