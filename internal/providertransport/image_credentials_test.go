package providertransport

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/require"
)

func TestImageCredentialOriginsFollowDerivedValues(t *testing.T) {
	var permitted, forbidden atomic.Int64
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forbidden.Add(1) }))
	defer other.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		permitted.Add(1)
		require.Equal(t, "Bearer synthetic-secret", r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, `{"session":"synthetic-session"}`)
	}))
	defer server.Close()
	header := manifest.ImageValue{Op: "concat", Args: []manifest.ImageValue{imageLiteral("Bearer "), {Ref: "/credentials/access"}}}
	request := func(address string, expression manifest.ImageValue) *manifest.ImageRequest {
		return &manifest.ImageRequest{Method: "GET", URL: imageLiteral(address), Headers: map[string]manifest.ImageValue{"Authorization": expression}, Encoding: "none", Response: "json", Phase: "setup", MaxBytes: 1024, TimeoutSeconds: 5}
	}
	host := ImageWorkflowHost{Client: server.Client(), ValidateOwner: func() error { return nil }, Credentials: map[string]any{"access": "synthetic-secret"}, Manifest: manifest.ImageManifest{
		Credentials: []manifest.ImageCredential{{ID: "access", Source: "environment", Environment: "SYNTHETIC_ACCESS"}},
		Origins:     []manifest.ImageOrigin{{URL: server.URL, Credentials: []string{"access"}}, {URL: other.URL}},
		Limits:      manifest.ImageLimits{ResponseBytes: 1024}, Workflows: map[string]manifest.ImageWorkflow{
			"session": {Steps: []manifest.ImageStep{{ID: "read", Request: request(server.URL, header)}}, Result: manifest.ImageValue{Ref: "/steps/read/body/session"}},
			"send":    {Steps: []manifest.ImageStep{{ID: "send", Request: request(other.URL, manifest.ImageValue{Ref: "/session"})}}, Result: imageLiteral(true)},
			"direct":  {Steps: []manifest.ImageStep{{ID: "send", Request: request(other.URL, header)}}, Result: imageLiteral(true)},
		},
	}}
	session, err := host.Execute(t.Context(), "session", nil)
	require.NoError(t, err)
	require.Equal(t, "synthetic-session", ImageWorkflowValue(session))
	_, err = json.Marshal(session)
	require.Error(t, err)
	_, err = host.Execute(t.Context(), "send", map[string]any{"session": session})
	require.Error(t, err)
	_, err = host.Execute(t.Context(), "direct", nil)
	require.Error(t, err)
	reference, err := ImageUploadReferenceFromValue(session)
	require.NoError(t, err)
	require.Equal(t, []string{"access"}, reference.Credentials)
	restored, err := host.ScopeUploadIdentifier(reference)
	require.NoError(t, err)
	_, err = host.Execute(t.Context(), "send", map[string]any{"session": restored})
	require.Error(t, err)
	reference.Credentials = []string{"unavailable"}
	_, err = host.ScopeUploadIdentifier(reference)
	require.Error(t, err)
	require.EqualValues(t, 1, permitted.Load())
	require.Zero(t, forbidden.Load())
}

func TestImageCookieOriginsAndRedirects(t *testing.T) {
	var forbidden atomic.Int64
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forbidden.Add(1) }))
	defer other.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "session=synthetic", r.Header.Get("Cookie"))
		http.Redirect(w, r, other.URL, http.StatusFound)
	}))
	defer server.Close()
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	address, err := url.Parse(server.URL)
	require.NoError(t, err)
	jar.SetCookies(address, []*http.Cookie{{Name: "session", Value: "synthetic", Secure: true}})
	host := ImageWorkflowHost{Client: server.Client(), CookieJars: map[string]http.CookieJar{"browser": jar}, ValidateOwner: func() error { return nil }, Manifest: manifest.ImageManifest{
		Credentials: []manifest.ImageCredential{{ID: "browser", Source: "browser", Domains: []string{address.Hostname()}}},
		Origins:     []manifest.ImageOrigin{{URL: server.URL, Credentials: []string{"browser"}}, {URL: other.URL}},
		Limits:      manifest.ImageLimits{ResponseBytes: 1024}, Workflows: map[string]manifest.ImageWorkflow{"read": {
			Steps: []manifest.ImageStep{{ID: "read", Request: &manifest.ImageRequest{Method: "GET", URL: imageLiteral(server.URL), Encoding: "none", Response: "text", Phase: "setup", MaxBytes: 1024, TimeoutSeconds: 5}}}, Result: imageLiteral(true),
		}},
	}}
	_, err = host.Execute(t.Context(), "read", nil)
	require.Error(t, err)
	require.Zero(t, forbidden.Load())
	host.Client.Jar = jar
	_, err = host.Execute(t.Context(), "read", nil)
	require.ErrorContains(t, err, "explicitly scoped")
}

func TestImageCookieResponseRejectsReplacedOwner(t *testing.T) {
	var replaced atomic.Bool
	stale := errors.New("synthetic owner replaced")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "session=initial", r.Header.Get("Cookie"))
		replaced.Store(true)
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "replacement"})
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()
	address, err := url.Parse(server.URL)
	require.NoError(t, err)
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	jar.SetCookies(address, []*http.Cookie{{Name: "session", Value: "initial"}})
	host := ImageWorkflowHost{Client: server.Client(), CookieJars: map[string]http.CookieJar{"browser": jar}, ValidateOwner: func() error {
		if replaced.Load() {
			return stale
		}
		return nil
	}, Manifest: manifest.ImageManifest{
		Credentials: []manifest.ImageCredential{{ID: "browser", Source: "browser", Domains: []string{address.Hostname()}}},
		Origins:     []manifest.ImageOrigin{{URL: server.URL, Credentials: []string{"browser"}}},
		Limits:      manifest.ImageLimits{ResponseBytes: 1024},
		Workflows:   map[string]manifest.ImageWorkflow{"read": {Steps: []manifest.ImageStep{{ID: "read", Request: &manifest.ImageRequest{Method: "GET", URL: imageLiteral(server.URL), Encoding: "none", Response: "text", Phase: "setup", MaxBytes: 1024, TimeoutSeconds: 5}}}, Result: imageLiteral(true)}},
	}}
	_, err = host.Execute(t.Context(), "read", nil)
	require.ErrorIs(t, err, stale)
	require.Equal(t, "initial", jar.Cookies(address)[0].Value)
}
