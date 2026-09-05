package cookieutil

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/publicsuffix"
)

func ValidateDomains(domains []string) error {
	if len(domains) == 0 || len(domains) > 64 {
		return errors.New("cookie domains must contain between 1 and 64 entries")
	}
	for _, domain := range domains {
		if domain == "" || domain != strings.ToLower(domain) || len(domain) > 253 {
			return errors.New("invalid cookie domain")
		}
		for _, label := range strings.Split(domain, ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return errors.New("invalid cookie domain")
			}
			for _, character := range label {
				if character != '-' && (character < 'a' || character > 'z') && (character < '0' || character > '9') {
					return errors.New("invalid cookie domain")
				}
			}
		}
		suffix, _ := publicsuffix.PublicSuffix(domain)
		if suffix == domain {
			return errors.New("cookie domain cannot be a public suffix")
		}
	}
	return nil
}

func MatchesDomain(host string, domains []string) bool {
	host = strings.TrimPrefix(strings.ToLower(host), ".")
	for _, domain := range domains {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

func domainQuery(column string, domains []string) (string, []any) {
	conditions := make([]string, 0, len(domains))
	args := make([]any, 0, len(domains)*3)
	for _, domain := range domains {
		conditions = append(conditions, "("+column+" = ? OR "+column+" = ? OR "+column+" LIKE ?)")
		args = append(args, domain, "."+domain, "%."+domain)
	}
	if len(conditions) == 0 {
		return "0", nil
	}
	return strings.Join(conditions, " OR "), args
}

type ScopedJars struct {
	Jars    map[string]http.CookieJar
	Allowed func(*url.URL, string) bool
}

func (s ScopedJars) Add(request *http.Request, used map[string]bool) {
	for id, jar := range s.Jars {
		if jar == nil || s.Allowed == nil || !s.Allowed(request.URL, id) {
			continue
		}
		cookies := jar.Cookies(request.URL)
		if len(cookies) > 0 {
			used[id] = true
		}
		for _, cookie := range cookies {
			request.AddCookie(cookie)
		}
	}
}

func (s ScopedJars) Store(target *url.URL, cookies []*http.Cookie) {
	for id, jar := range s.Jars {
		if jar != nil && s.Allowed != nil && s.Allowed(target, id) {
			jar.SetCookies(target, cookies)
		}
	}
}
