package agent

import (
	"context"
	"net/http"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

type errorMappingModel struct {
	generateErr error
	streamErr   error
}

func (m errorMappingModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, m.generateErr
}

func (m errorMappingModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	if m.streamErr == nil {
		return nil, nil
	}
	return func(yield func(fantasy.StreamPart) bool) {
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: m.streamErr})
	}, nil
}

func (m errorMappingModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, m.generateErr
}

func (m errorMappingModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, m.streamErr
}

func (errorMappingModel) Provider() string { return "test" }
func (errorMappingModel) Model() string    { return "test" }

func TestLanguageModelErrorMappingsApplyToImmediateAndStreamErrors(t *testing.T) {
	registration := providerregistry.Registration{Errors: []manifest.ErrorMapping{{
		Class: "authentication", Statuses: []int{http.StatusForbidden}, Title: "Authentication required",
	}}}
	immediate := &fantasy.ProviderError{StatusCode: http.StatusForbidden}
	streamed := &fantasy.ProviderError{StatusCode: http.StatusForbidden}
	model := mapLanguageModelErrors(errorMappingModel{generateErr: immediate, streamErr: streamed}, registration)

	_, err := model.Generate(t.Context(), fantasy.Call{})
	require.Error(t, err)
	require.True(t, immediate.AuthError)
	require.Equal(t, "Authentication required", immediate.Title)

	stream, err := model.Stream(t.Context(), fantasy.Call{})
	require.NoError(t, err)
	parts := make([]fantasy.StreamPart, 0, 1)
	stream(func(part fantasy.StreamPart) bool {
		parts = append(parts, part)
		return true
	})
	require.Len(t, parts, 1)
	require.True(t, streamed.AuthError)
	require.Equal(t, "Authentication required", streamed.Title)

	objectErr := &fantasy.ProviderError{StatusCode: http.StatusForbidden}
	objectModel := mapLanguageModelErrors(errorMappingModel{generateErr: objectErr}, registration)
	_, err = objectModel.GenerateObject(t.Context(), fantasy.ObjectCall{})
	require.Error(t, err)
	require.True(t, objectErr.AuthError)

	objectStreamErr := &fantasy.ProviderError{StatusCode: http.StatusForbidden}
	objectStreamModel := mapLanguageModelErrors(errorMappingModel{streamErr: objectStreamErr}, registration)
	_, err = objectStreamModel.StreamObject(t.Context(), fantasy.ObjectCall{})
	require.Error(t, err)
	require.True(t, objectStreamErr.AuthError)
}
