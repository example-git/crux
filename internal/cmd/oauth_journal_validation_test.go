package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"
)

func executeOAuthJournalValidationCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	// Execute the production registered command, preserving global Cobra state
	// so subsequent CLI tests are independent of these invocations.
	type savedFlag struct {
		flag    *pflag.Flag
		value   string
		changed bool
	}
	var saved []savedFlag
	type savedContext struct {
		command *cobra.Command
		ctx     context.Context
	}
	var contexts []savedContext
	seen := map[*pflag.Flag]bool{}
	for _, command := range []*cobra.Command{rootCmd, accountsCmd, pendingOAuthJournalCmd, retireOAuthJournalCmd} {
		contexts = append(contexts, savedContext{command, command.Context()})
		command.SetContext(t.Context())
		for _, flags := range []*pflag.FlagSet{command.Flags(), command.PersistentFlags()} {
			flags.VisitAll(func(flag *pflag.Flag) {
				if !seen[flag] {
					seen[flag] = true
					saved = append(saved, savedFlag{flag, flag.Value.String(), flag.Changed})
				}
			})
		}
	}
	previousOut, previousErr := rootCmd.OutOrStdout(), rootCmd.ErrOrStderr()
	defer func() {
		for _, saved := range contexts {
			saved.command.SetContext(saved.ctx)
		}
		for _, s := range saved {
			if s.flag.Value.String() != s.value {
				_ = s.flag.Value.Set(s.value)
			}
			s.flag.Changed = s.changed
		}
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(previousOut)
		rootCmd.SetErr(previousErr)
	}()
	var output bytes.Buffer
	rootCmd.SetOut(&output)
	rootCmd.SetErr(&output)
	rootCmd.SetArgs(args)
	err := rootCmd.ExecuteContext(t.Context())
	return output.String(), err
}

func TestOAuthJournalValidationCLIRealProducerRemovedProvider(t *testing.T) {
	for _, state := range []string{"not-started", "unknown", "token-result-recorded"} {
		t.Run(state, func(t *testing.T) {
			var calls atomic.Int32
			host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if state == "unknown" {
					http.Error(w, `{"error":"synthetic-rejection"}`, 400)
					return
				}
				_, _ = w.Write([]byte(`{"access_token":"cli-journal-access","refresh_token":"cli-journal-refresh","expires_in":120}`))
			}))
			defer host.Close()
			rootEnv := t.TempDir()
			t.Setenv("AI_CLI_DIR", filepath.Join(rootEnv, "accounts"))
			previousClient, previousTransport := http.DefaultClient, http.DefaultTransport
			http.DefaultClient = host.Client()
			http.DefaultTransport = host.Client().Transport
			t.Cleanup(func() { http.DefaultClient = previousClient; http.DefaultTransport = previousTransport })
			store, root, values := newCLIOAuthStore(t, host, "hosted-paste")
			owner, ok := store.RuntimeSnapshot().ProviderOwner("example-responses")
			require.True(t, ok)
			before, err := store.CaptureAuthentication(t.Context())
			require.NoError(t, err)
			key := config.AuthenticationJournalKey{Kind: config.AuthenticationJournalOAuth, WorkspaceID: "original-cli-workspace", OperationID: strings.Repeat("d", 32)}
			ctx := config.ContextWithAuthenticationOperation(t.Context(), key)
			prepared, err := store.PrepareOAuthLogin(ctx, before, owner)
			require.NoError(t, err)
			if state != "not-started" {
				code, err := store.PrepareOAuthCodeChallenge(ctx, prepared, 0)
				require.NoError(t, err)
				defer code.Close()
				uri, err := url.Parse(code.AuthorizationURL())
				require.NoError(t, err)
				input := url.Values{"code": {"synthetic-code"}, "state": {uri.Query().Get("state")}}.Encode()
				_, err = store.ExchangeOAuthCode(ctx, code, input)
				if state == "unknown" {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
				}
			}
			global := filepath.Join(values["CRUX_GLOBAL_DATA"], "crux.json")
			workspacePath := filepath.Join(root, "workspace", "crux.json")
			// No currently loadable provider survives. The command must not read
			// malformed config or execute a shell config to find journal scope.
			marker := filepath.Join(root, "must-not-execute")
			require.NoError(t, os.WriteFile(global, []byte("not valid configuration"), 0600))
			require.NoError(t, os.WriteFile(filepath.Join(root, ".cruxrc"), []byte("#!/bin/sh\ntouch "+marker+"\n"), 0700))
			require.NoError(t, os.RemoveAll(providerplugin.DefaultPaths(values["CRUX_GLOBAL_DATA"], values["CRUX_CACHE_DIR"]).Bundles))
			flags := []string{"--cwd", root, "--global-config-data", global, "--workspace-config", workspacePath}
			args := append([]string{"accounts", "pending-oauth", "--json"}, flags...)
			output, err := executeOAuthJournalValidationCLI(t, args...)
			require.NoError(t, err)
			var listed []config.OAuthLoginJournalMetadata
			require.NoError(t, json.Unmarshal([]byte(output), &listed))
			require.Len(t, listed, 1)
			require.False(t, listed[0].Abandoned)
			want := state
			if state == "unknown" {
				want = "exchange-outcome-unknown"
			}
			require.Equal(t, want, listed[0].State)
			require.NotContains(t, output, "cli-journal-access")
			require.NotContains(t, output, owner.AccountNamespace)
			retire := append([]string{"accounts", "retire-oauth", key.WorkspaceID, key.OperationID}, flags...)
			output, err = executeOAuthJournalValidationCLI(t, retire...)
			if state == "token-result-recorded" {
				require.Error(t, err)
				retire = append(retire, "--discard-recorded-token")
				output, err = executeOAuthJournalValidationCLI(t, retire...)
				require.Contains(t, output, "not unknown")
			}
			require.NoError(t, err)
			_, err = executeOAuthJournalValidationCLI(t, retire...)
			require.NoError(t, err, "exact CLI replay with a fresh scope handle")
			_, err = os.Stat(marker)
			require.ErrorIs(t, err, os.ErrNotExist)
			data, err := os.ReadFile(global)
			require.NoError(t, err)
			require.Equal(t, "not valid configuration", string(data))
			if state == "not-started" {
				require.Zero(t, calls.Load())
			} else {
				require.EqualValues(t, 1, calls.Load())
			}
		})
	}
}

func TestOAuthJournalValidationCLIExplicitScopeAndAuthority(t *testing.T) {
	root := t.TempDir()
	global := filepath.Join(root, "global.json")
	for _, name := range []string{"pending-oauth", "retire-oauth"} {
		command, _, err := rootCmd.Find([]string{"accounts", name})
		require.NoError(t, err)
		require.Equal(t, name, command.Name())
	}
	_, err := executeOAuthJournalValidationCLI(t, "accounts", "pending-oauth", "--cwd", root, "--global-config-data", global)
	require.Error(t, err)
	output, err := executeOAuthJournalValidationCLI(t, "accounts", "pending-oauth", "--json", "--cwd", root, "--global-config-data", global, "--workspace-config", "")
	require.NoError(t, err)
	require.JSONEq(t, `[]`, output)
	for _, selector := range []string{"--connection", "--host", "--data-dir"} {
		_, err = executeOAuthJournalValidationCLI(t, "accounts", "pending-oauth", "--cwd", root, "--global-config-data", global, "--workspace-config", "", selector, "unavailable")
		require.ErrorContains(t, err, "local-only")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = config.OpenOAuthLoginJournalScope(ctx, global, "", root)
	require.ErrorIs(t, err, context.Canceled)
}
