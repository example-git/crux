//go:build embedded_mitmproxy && (darwin || linux)

package trafficcapture

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func init() {
	captureTestCommands["__capture-tls-environment"] = func(arguments []string) int {
		_ = os.Setenv("CRUX_TLS_TEST_CONFIG", arguments[0])
		source := strings.TrimSuffix(embeddedWorker, "asyncio.run(run_worker())\n") + `
configuration = json.loads(Path(os.environ["CRUX_TLS_TEST_CONFIG"]).read_text())
environment = target_environment(configuration["base"], "http://127.0.0.1:1", Path(configuration["certificate"]), Path(configuration["bundle"]), configuration["unset"])
for name in PROXY_VARIABLES:
    environment.pop(name, None)
write_json(Path(configuration["output"]), environment)
`
		if err := runEmbeddedPython(source); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
}

func captureTLSFixtureEnvironment(t *testing.T, base map[string]string, unset []string) map[string]string {
	t.Helper()
	directory := t.TempDir()
	certificate, bundle := filepath.Join(directory, "certificate.pem"), filepath.Join(directory, "bundle.pem")
	require.NoError(t, os.WriteFile(certificate, nil, 0o600))
	require.NoError(t, os.WriteFile(bundle, nil, 0o600))
	output := filepath.Join(directory, "environment.json")
	configuration := filepath.Join(directory, "input.json")
	require.NoError(t, writePrivateJSON(configuration, map[string]any{"base": base, "unset": unset, "certificate": certificate, "bundle": bundle, "output": output}))
	executable, err := os.Executable()
	require.NoError(t, err)
	command := exec.CommandContext(t.Context(), executable, "__capture-tls-environment", configuration)
	log, err := command.CombinedOutput()
	require.NoError(t, err, string(log))
	data, err := os.ReadFile(output)
	require.NoError(t, err)
	var environment map[string]string
	require.NoError(t, json.Unmarshal(data, &environment))
	return environment
}

func TestCaptureTLSRelaxationIsScopedToCapturedClients(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if strings.HasSuffix(request.URL.Path, "/info/refs") {
			response.Header().Set("Content-Type", "text/plain")
			response.WriteHeader(http.StatusOK)
			return
		}
		_, _ = response.Write([]byte("captured-tls"))
	}))
	defer server.Close()
	base := environmentMap(os.Environ())
	for _, key := range []string{"NODE_TLS_REJECT_UNAUTHORIZED", "NODE_EXTRA_CA_CERTS", "GIT_SSL_NO_VERIFY", "GIT_SSL_CAINFO", "SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE", "PYTHONHTTPSVERIFY", "PYTHONPATH", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		delete(base, key)
	}
	base["GIT_CONFIG_NOSYSTEM"] = "1"
	base["GIT_CONFIG_GLOBAL"] = os.DevNull
	captured := captureTLSFixtureEnvironment(t, base, nil)
	for _, scenario := range []struct {
		name, executable string
		arguments        []string
	}{
		{"python", "python3", []string{"-c", "import urllib.request,sys; print(urllib.request.urlopen(sys.argv[1],timeout=5).read().decode())", server.URL}},
		{"node", "node", []string{"-e", "require('https').get(process.argv[1],r=>{r.resume();r.on('end',()=>process.exit(r.statusCode===200?0:2))}).on('error',e=>{console.error(e.message);process.exit(1)})", server.URL}},
		{"git", "git", []string{"-c", "http.version=HTTP/1.1", "ls-remote", server.URL + "/repo.git"}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			executable, err := exec.LookPath(scenario.executable)
			if err != nil {
				t.Skipf("runtime unavailable: %s", scenario.executable)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			uncaptured := exec.CommandContext(ctx, executable, scenario.arguments...)
			uncaptured.Env = environmentList(base)
			output, err := uncaptured.CombinedOutput()
			require.Error(t, err, "uncaptured runtime unexpectedly accepted the fixture certificate: %s", output)
			command := exec.CommandContext(ctx, executable, scenario.arguments...)
			command.Env = environmentList(captured)
			output, err = command.CombinedOutput()
			require.NoError(t, err, string(output))
		})
	}
	_, changed := base["NODE_TLS_REJECT_UNAUTHORIZED"]
	require.False(t, changed)
}

func TestCapturePythonTLSHookChainsCustomizationAndHonorsUnsets(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is unavailable")
	}
	custom := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(custom, "sitecustomize.py"), []byte("import os\nos.environ['CAPTURE_TEST_CUSTOMIZATION']='preserved'\n"), 0o600))
	base := map[string]string{"PATH": os.Getenv("PATH"), "PYTHONPATH": custom, "CAPTURE_REMOVE": "private"}
	environment := captureTLSFixtureEnvironment(t, base, []string{"CAPTURE_REMOVE", "GIT_SSL_NO_VERIFY"})
	require.NotContains(t, environment, "CAPTURE_REMOVE")
	require.NotContains(t, environment, "GIT_SSL_NO_VERIFY")
	require.Contains(t, environment["PYTHONPATH"], custom)
	command := exec.CommandContext(t.Context(), python, "-c", "import os,ssl; print(os.environ['CAPTURE_TEST_CUSTOMIZATION']); print(ssl._create_default_https_context().verify_mode)")
	command.Env = environmentList(environment)
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
	require.True(t, bytes.Contains(output, []byte("preserved\n0\n")), string(output))
	removed := captureTLSFixtureEnvironment(t, base, []string{"PYTHONPATH", "NODE_TLS_REJECT_UNAUTHORIZED"})
	require.NotContains(t, removed, "PYTHONPATH")
	require.NotContains(t, removed, "NODE_TLS_REJECT_UNAUTHORIZED")
}
