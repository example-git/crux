package connection

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConfirmAuthorizationRejectsSubstitutedOrMalformedHTTPSProof(t *testing.T) {
	for _, mode := range []string{"valid", "principal", "server", "version", "duplicate", "unknown", "missing", "null", "trailing", "oversized", "redirect", "health"} {
		t.Run(mode, func(t *testing.T) {
			setConnectionRoot(t, t.TempDir())
			serverCode, err := EnsureServerIdentity(t.Context())
			require.NoError(t, err)
			saved, clientCode, err := Add(t.Context(), "proof-client", "tcp://127.0.0.1:9443", serverCode)
			require.NoError(t, err)
			require.NoError(t, AuthorizeClient(t.Context(), saved.Name, clientCode))
			tlsConfig, authority, err := ServerTLSConfigWithAuthorization(t.Context())
			require.NoError(t, err)
			var requests atomic.Int32
			hs := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				require.Equal(t, AuthorizationProofPath, r.URL.Path)
				proof, err := authority.Proof(r.Context(), *r.TLS)
				require.NoError(t, err)
				switch mode {
				case "principal":
					proof.Principal = strings.Repeat("0", 64)
				case "server":
					proof.ServerFingerprint = strings.Repeat("0", 64)
				case "version":
					proof.Version++
				case "redirect":
					http.Redirect(w, r, "https://"+r.Host+"/elsewhere", http.StatusTemporaryRedirect)
					return
				case "health":
					_, _ = w.Write([]byte(`{"status":"ok"}`))
					return
				case "oversized":
					_, _ = w.Write([]byte(strings.Repeat(" ", 4097)))
					return
				case "null":
					_, _ = w.Write([]byte(`{"version":1,"principal":null,"server_fingerprint":null}`))
					return
				}
				body, err := json.Marshal(proof)
				require.NoError(t, err)
				switch mode {
				case "duplicate":
					body = []byte(strings.TrimSuffix(string(body), "}") + `,"principal":"` + proof.Principal + `"}`)
				case "unknown":
					body = []byte(strings.TrimSuffix(string(body), "}") + `,"extra":true}`)
				case "missing":
					body = []byte(`{"version":1,"principal":"` + proof.Principal + `"}`)
				case "trailing":
					body = append(body, []byte(`{}`)...)
				}
				_, _ = w.Write(body)
			}))
			hs.TLS = tlsConfig
			hs.StartTLS()
			t.Cleanup(hs.Close)
			saved.Address = "tcp://" + strings.TrimPrefix(hs.URL, "https://")
			proof, err := ConfirmAuthorization(t.Context(), saved)
			if mode == "valid" {
				require.NoError(t, err)
				require.NoError(t, proof.ValidateFor(saved))
			} else {
				require.ErrorIs(t, err, ErrClientAuthorization)
			}
			require.EqualValues(t, 1, requests.Load(), "no redirect or health fallback may execute")
		})
	}
}
