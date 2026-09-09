package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAuthenticationBasisReloadKeepsPreparedConfigImmutable(t *testing.T) {
	for _, correction := range []string{"none", "notifications", "provider-owner"} {
		t.Run(correction, func(t *testing.T) {
			store, root, base := authenticationBasisStore(t, nil)
			switch correction {
			case "notifications":
				require.NoError(t, os.MkdirAll(filepath.Join(root, "config"), 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(root, "config", "crux.json"), []byte(`{"options":{"disable_notifications":true}}`), 0o600))
			case "provider-owner":
				authenticationBasisWriteField(t, filepath.Join(root, "crux.json"), []string{"providers", "added"}, `{"type":"openai-compat","base_url":"https://example.invalid/v1","api_key":"synthetic-added-key","models":[{"id":"added-model"}]}`)
			}

			var retained *Config
			var retainedBasis *authenticationLoadBasis
			var preparedJSON []byte
			var preparedModels AgentModelState
			var readerChanged bool
			var finish func()
			store.SetRuntimeGenerationPreparer(func(_ context.Context, snapshot RuntimeSnapshot) (RuntimeGenerationCandidate, error) {
				retained = snapshot.Config()
				retainedBasis = retained.authenticationBasis
				basisBefore := retainedBasis.clone()
				preparedModels = snapshot.AgentModelState()
				var err error
				preparedJSON, err = json.Marshal(retained)
				if err != nil {
					return RuntimeGenerationCandidate{}, err
				}
				started := make(chan struct{})
				stop := make(chan struct{})
				done := make(chan bool, 1)
				go func() {
					changed := false
					close(started)
					for {
						// Copy the complete struct, as prompt preparation does. The
						// reader remains active throughout corrections and migration.
						observed := *retained
						changed = changed || observed.authenticationBasis != retainedBasis || !reflect.DeepEqual(observed.authenticationBasis, basisBefore)
						_, _ = json.Marshal(&observed)
						select {
						case <-stop:
							done <- changed
							return
						default:
							runtime.Gosched()
						}
					}
				}()
				<-started
				var once sync.Once
				finish = func() {
					once.Do(func() {
						close(stop)
						readerChanged = <-done
					})
				}
				return RuntimeGenerationCandidate{Commit: finish, Abort: finish}, nil
			})
			t.Cleanup(func() {
				if finish != nil {
					finish()
				}
			})

			require.NoError(t, store.ReloadFromDisk(t.Context()))
			require.NotNil(t, retained)
			require.False(t, readerChanged, "preparation readers must never observe receipt attachment")
			require.Same(t, retainedBasis, retained.authenticationBasis)
			published := store.Config()
			require.Same(t, retained, published, "the accepted store and prepared runtime must publish the same configuration")
			retainedJSON, err := json.Marshal(retained)
			require.NoError(t, err)
			publishedJSON, err := json.Marshal(published)
			require.NoError(t, err)
			require.Equal(t, preparedJSON, retainedJSON)
			require.Equal(t, preparedJSON, publishedJSON)
			require.Equal(t, preparedModels, store.RuntimeSnapshot().AgentModelState())
			capture, layers := authenticationBasisCapture(t, store, base)
			require.NoError(t, capture.validateConfigBasis(layers, ""), "published receipts must match actual saved inputs")
			if correction == "provider-owner" {
				require.Contains(t, string(retainedBasis.sources[store.globalDataPath].raw), `"added"`)
				require.Contains(t, string(published.authenticationBasis.sources[store.globalDataPath].raw), `"added"`)
			}
		})
	}
}

func TestAuthenticationBasisReloadPreservesPeerBeforeMigration(t *testing.T) {
	for _, pendingOtherOwner := range []bool{false, true} {
		t.Run(fmt.Sprintf("other-pending-owner=%t", pendingOtherOwner), func(t *testing.T) {
			store, root, _ := authenticationBasisStore(t, nil)
			before := store.Config()
			globalBefore, err := os.ReadFile(store.globalDataPath)
			require.NoError(t, err)
			provider := `{"type":"openai-compat","base_url":"https://example.invalid/v1","api_key":"synthetic-added-key","models":[{"id":"added-model"}]}`
			authenticationBasisWriteField(t, filepath.Join(root, "crux.json"), []string{"providers", "added"}, provider)
			if pendingOtherOwner {
				authenticationBasisWriteField(t, filepath.Join(root, "crux.json"), []string{"providers", "other"}, provider)
			}

			var prepared *Config
			var preparedBasis *authenticationLoadBasis
			var committed, aborted bool
			var peer []byte
			migrationWrites := 0
			oldWriter := writeProviderMigrationConfig
			writeProviderMigrationConfig = func(path string, data []byte, mode os.FileMode) error {
				migrationWrites++
				return oldWriter(path, data, mode)
			}
			t.Cleanup(func() { writeProviderMigrationConfig = oldWriter })
			store.SetRuntimeGenerationPreparer(func(_ context.Context, snapshot RuntimeSnapshot) (RuntimeGenerationCandidate, error) {
				prepared = snapshot.Config()
				preparedBasis = prepared.authenticationBasis.clone()
				require.Contains(t, string(preparedBasis.sources[store.globalDataPath].raw), `"added"`, "the intended receipt must be complete before preparation")
				// A peer completes one intended field after preparation. Neither
				// observation nor pending work authorizes rolling back that edit.
				authenticationBasisWriteField(t, store.globalDataPath, []string{"providers", "added", "owner"}, `{"type":"custom","construction":"openai-compat"}`)
				peer, err = os.ReadFile(store.globalDataPath)
				require.NoError(t, err)
				return RuntimeGenerationCandidate{
					Commit: func() { committed = true },
					Abort:  func() { aborted = true },
				}, nil
			})

			err = store.ReloadFromDisk(t.Context())
			require.ErrorContains(t, err, "changed after correction")
			require.Zero(t, migrationWrites, "peer drift must abort before owner migration")
			require.False(t, committed)
			require.True(t, aborted)
			require.Same(t, before, store.Config())
			require.Equal(t, preparedBasis, prepared.authenticationBasis, "failed publication must leave the prepared basis immutable")
			globalAfter, err := os.ReadFile(store.globalDataPath)
			require.NoError(t, err)
			require.NotEqual(t, globalBefore, peer)
			require.Equal(t, peer, globalAfter, "an unwritten peer edit is never rollback-owned")
		})
	}
}

func TestStartupAndReloadCorrectionsPreservePeerWrites(t *testing.T) {
	for _, mode := range []string{"startup", "reload"} {
		for _, failure := range []string{"later-source-change", "post-write-peer-and-failure"} {
			t.Run(mode+"/"+failure, func(t *testing.T) {
				store, root, base := authenticationBasisStore(t, nil)
				before := store.Config()
				sourcePath := filepath.Join(root, "config", "crux.json")
				require.NoError(t, os.MkdirAll(filepath.Dir(sourcePath), 0o700))
				sourceOriginal := []byte(`{"options":{"disable_notifications":true},"sentinel":"source"}`)
				require.NoError(t, os.WriteFile(sourcePath, sourceOriginal, 0o600))
				dataOriginal, err := os.ReadFile(store.globalDataPath)
				require.NoError(t, err)
				var peer []byte
				var committed, aborted bool
				store.SetRuntimeGenerationPreparer(func(context.Context, RuntimeSnapshot) (RuntimeGenerationCandidate, error) {
					return RuntimeGenerationCandidate{Commit: func() { committed = true }, Abort: func() { aborted = true }}, nil
				})
				oldWriter := writeStartupConfigFile
				writes := []string{}
				writeStartupConfigFile = func(path string, data []byte, mode os.FileMode) error {
					writes = append(writes, path)
					if path == store.globalDataPath {
						return errors.New("injected later correction failure")
					}
					if err := oldWriter(path, data, mode); err != nil {
						return err
					}
					peerPath, original := store.globalDataPath, dataOriginal
					if failure == "post-write-peer-and-failure" {
						peerPath, original = sourcePath, data
					}
					var err error
					peer, err = runtimeControlChangeField(original, []string{"peer"}, json.RawMessage(`true`), false)
					if err != nil {
						return err
					}
					return os.WriteFile(peerPath, peer, 0o600)
				}
				t.Cleanup(func() { writeStartupConfigFile = oldWriter })

				if mode == "startup" {
					var loaded *ConfigStore
					loaded, err = LoadIsolated(root, before.Options.DataDirectory, false, base)
					require.Nil(t, loaded)
				} else {
					err = store.ReloadFromDisk(t.Context())
					require.True(t, aborted)
					require.False(t, committed)
				}
				require.Error(t, err)
				require.Same(t, before, store.Config())
				sourceAfter, readErr := os.ReadFile(sourcePath)
				require.NoError(t, readErr)
				dataAfter, readErr := os.ReadFile(store.globalDataPath)
				require.NoError(t, readErr)
				if failure == "later-source-change" {
					require.ErrorContains(t, err, "config source changed before correction")
					require.Equal(t, []string{sourcePath}, writes, "later-path CAS must fail before invoking its writer")
					require.Equal(t, sourceOriginal, sourceAfter, "roll back the successful own correction")
					require.Equal(t, peer, dataAfter, "preserve the unwritten later-path peer edit")
				} else {
					require.ErrorContains(t, err, "injected later correction failure")
					require.Equal(t, []string{sourcePath, store.globalDataPath}, writes)
					require.Equal(t, peer, sourceAfter, "do not roll back over a later peer edit")
					require.Equal(t, dataOriginal, dataAfter)
				}
			})
		}
	}
}
