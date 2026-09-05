package providertransport

import (
	"errors"
	"maps"
	"net/url"
	"slices"
	"strings"

	"github.com/example-git/crux/internal/cookieutil"
)

type imageScopedValue struct {
	value       any
	credentials []string
}

func (imageScopedValue) MarshalJSON() ([]byte, error) {
	return nil, errors.New("image credential-derived values cannot be persisted")
}

type ImageUploadReference struct {
	Identifier  string   `json:"identifier"`
	Credentials []string `json:"credentials,omitempty"`
}

func ImageUploadReferenceFromValue(value any) (ImageUploadReference, error) {
	evaluation := imageEvaluation{remaining: 100000}
	identifier, ok := evaluation.unwrap(value).(string)
	if !ok {
		return ImageUploadReference{}, errors.New("persistent image upload result must be an opaque identifier")
	}
	return ImageUploadReference{Identifier: identifier, Credentials: slices.Sorted(maps.Keys(evaluation.credentials))}, nil
}

func (h *ImageWorkflowHost) ScopeUploadIdentifier(value ImageUploadReference) (any, error) {
	for _, id := range value.Credentials {
		found := false
		for _, credential := range h.Manifest.Credentials {
			found = found || credential.ID == id
		}
		if !found {
			return nil, errors.New("cached image upload has an unavailable credential owner")
		}
	}
	return imageScopedValue{value: value.Identifier, credentials: slices.Clone(value.Credentials)}, nil
}

func ImageWorkflowValue(value any) any {
	if scoped, ok := value.(imageScopedValue); ok {
		return scoped.value
	}
	return value
}

func (e *imageEvaluation) unwrap(value any) any {
	for {
		scoped, ok := value.(imageScopedValue)
		if !ok {
			return value
		}
		if e.credentials == nil {
			e.credentials = map[string]bool{}
		}
		for _, id := range scoped.credentials {
			e.credentials[id] = true
		}
		value = scoped.value
	}
}

func (e *imageEvaluation) materialize(value any) (any, error) {
	e.remaining--
	if e.remaining < 0 {
		return nil, errors.New("image expression evaluation limit exceeded")
	}
	value = e.unwrap(value)
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			resolved, err := e.materialize(item)
			if err != nil {
				return nil, err
			}
			result[key] = resolved
		}
		return result, nil
	case []any:
		result := make([]any, len(typed))
		for index, item := range typed {
			resolved, err := e.materialize(item)
			if err != nil {
				return nil, err
			}
			result[index] = resolved
		}
		return result, nil
	default:
		return value, nil
	}
}

func (e *imageEvaluation) scoped(value any) any {
	if len(e.credentials) == 0 {
		return value
	}
	return imageScopedValue{value: value, credentials: slices.Sorted(maps.Keys(e.credentials))}
}

func imageOriginMatches(target, base *url.URL, subdomains bool) bool {
	return target.Scheme == "https" && target.User == nil && target.Port() == base.Port() &&
		(strings.EqualFold(target.Hostname(), base.Hostname()) || subdomains && strings.HasSuffix(strings.ToLower(target.Hostname()), "."+strings.ToLower(base.Hostname())))
}

func (h *ImageWorkflowHost) credentialAllowed(target *url.URL, id string) bool {
	for _, origin := range h.Manifest.Origins {
		base, err := url.Parse(origin.URL)
		if err == nil && imageOriginMatches(target, base, origin.Subdomains) && slices.Contains(origin.Credentials, id) {
			return true
		}
	}
	return false
}

func (h *ImageWorkflowHost) checkCredentials(target *url.URL, credentials map[string]bool) error {
	for id := range credentials {
		if !h.credentialAllowed(target, id) {
			return &imageBoundaryError{cause: errors.New("image credential is not authorized for request origin")}
		}
	}
	return nil
}

func (h *ImageWorkflowHost) credentialValues(values map[string]any) (map[string]any, error) {
	if h.Client != nil && h.Client.Jar != nil {
		return nil, errors.New("image workflow requires explicitly scoped cookie jars")
	}
	declared := map[string]string{}
	for _, credential := range h.Manifest.Credentials {
		declared[credential.ID] = credential.Source
	}
	credentials := make(map[string]any, len(h.Credentials))
	for id, value := range h.Credentials {
		if declared[id] == "" || declared[id] == "browser" {
			return nil, errors.New("image credential source does not match declaration")
		}
		credentials[id] = imageScopedValue{value: value, credentials: []string{id}}
	}
	for id, jar := range h.CookieJars {
		if declared[id] != "browser" || jar == nil {
			return nil, errors.New("image cookie source does not match declaration")
		}
	}
	result := imageCloneContext(values)
	result["credentials"] = credentials
	return result, nil
}

func (h *ImageWorkflowHost) scopedCookies() cookieutil.ScopedJars {
	return cookieutil.ScopedJars{Jars: h.CookieJars, Allowed: func(target *url.URL, id string) bool {
		if !h.credentialAllowed(target, id) {
			return false
		}
		for _, credential := range h.Manifest.Credentials {
			if credential.ID == id && credential.Source == "browser" {
				return cookieutil.MatchesDomain(target.Hostname(), credential.Domains)
			}
		}
		return false
	}}
}
