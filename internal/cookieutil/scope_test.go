package cookieutil

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCookieDomainsRejectBroadOrMalformedScope(t *testing.T) {
	for _, domains := range [][]string{nil, {"com"}, {"co.uk"}, {"*.example.com"}, {"example.com%"}, {"https://example.com"}, {".example.com"}, {"example.com/"}} {
		require.Error(t, ValidateDomains(domains))
	}
	domains := []string{"provider.example"}
	require.NoError(t, ValidateDomains(domains))
	for _, host := range []string{"provider.example", ".provider.example", "app.provider.example"} {
		require.True(t, MatchesDomain(host, domains))
	}
	for _, host := range []string{"evilprovider.example", "provider.example.evil", "example"} {
		require.False(t, MatchesDomain(host, domains))
	}
}

func TestScopedJarsIsolateRequestAndResponseCookies(t *testing.T) {
	allowed, err := url.Parse("https://allowed.example/")
	require.NoError(t, err)
	denied, err := url.Parse("https://denied.example/")
	require.NoError(t, err)
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	jar.SetCookies(allowed, []*http.Cookie{{Name: "session", Value: "initial"}})
	jar.SetCookies(denied, []*http.Cookie{{Name: "session", Value: "private"}})
	scoped := ScopedJars{Jars: map[string]http.CookieJar{"owner": jar}, Allowed: func(target *url.URL, id string) bool {
		return id == "owner" && target.Host == allowed.Host
	}}
	for _, target := range []*url.URL{allowed, denied} {
		request := &http.Request{URL: target, Header: http.Header{}}
		used := map[string]bool{}
		scoped.Add(request, used)
		if target == allowed {
			require.Equal(t, "session=initial", request.Header.Get("Cookie"))
			require.True(t, used["owner"])
		} else {
			require.Empty(t, request.Header.Get("Cookie"))
			require.Empty(t, used)
		}
		scoped.Store(target, []*http.Cookie{{Name: "session", Value: "replacement"}})
	}
	require.Equal(t, "replacement", jar.Cookies(allowed)[0].Value)
	require.Equal(t, "private", jar.Cookies(denied)[0].Value)
}
