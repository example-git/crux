package config

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAuthenticationScopeWriteStagesThenCommitsExactPrivateFile(t *testing.T) {
	for _, exists := range []bool{false, true} {
		t.Run(fmt.Sprint(exists), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			original := []byte(`{"sibling":{"large":9007199254740993},"api_key":"old"}`)
			if exists {
				require.NoError(t, os.WriteFile(path, original, 0o644))
			}
			before, err := readAuthenticationInput(t.Context(), path)
			require.NoError(t, err)
			data := []byte(`{"sibling":{"large":9007199254740993},"api_key":"synthetic-secret"}`)
			stage, err := stageAuthenticationScopeWrite(t.Context(), before, authenticationCredentialEdit{path: path, data: data})
			require.NoError(t, err)
			defer stage.Close()
			require.NoError(t, checkAuthenticationScopePreimage(t.Context(), before), "staging must not write destination")
			temporary := stage.temporary
			info, err := os.Stat(temporary)
			require.NoError(t, err)
			if runtime.GOOS != "windows" {
				require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
			}
			for _, value := range []any{stage, *stage} {
				_, err = json.Marshal(value)
				require.Error(t, err)
				for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
					text := fmt.Sprintf(format, value)
					require.NotContains(t, text, "synthetic-secret")
					require.NotContains(t, text, path)
				}
			}
			// The stage owns independent bytes, including exact large-number spelling.
			data[1] = 'x'
			after, written, err := stage.Commit(t.Context())
			require.NoError(t, err)
			require.True(t, written)
			require.Equal(t, `{"sibling":{"large":9007199254740993},"api_key":"synthetic-secret"}`, string(after.data))
			if runtime.GOOS != "windows" {
				require.Equal(t, os.FileMode(0o600), after.info.mode.Perm())
			}
			_, err = os.Stat(temporary)
			require.ErrorIs(t, err, os.ErrNotExist)
			_, written, err = stage.Commit(t.Context())
			require.Error(t, err)
			require.False(t, written)
			require.NoError(t, checkAuthenticationScopePreimage(t.Context(), after))
		})
	}
}

func TestAuthenticationScopeWriteRejectsReplacementAndCancellation(t *testing.T) {
	for _, mode := range []string{"different bytes", "same bytes new inode", "cancel", "closed"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			original := []byte(`{"old":true}`)
			require.NoError(t, os.WriteFile(path, original, 0o600))
			before, err := readAuthenticationInput(t.Context(), path)
			require.NoError(t, err)
			stage, err := stageAuthenticationScopeWrite(t.Context(), before, authenticationCredentialEdit{path: path, data: []byte(`{"new":true}`)})
			require.NoError(t, err)
			temporary := stage.temporary
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			want := original
			switch mode {
			case "different bytes":
				want = []byte(`{"foreign":true}`)
				require.NoError(t, os.WriteFile(path, want, 0o600))
			case "same bytes new inode":
				replacement := path + ".peer"
				require.NoError(t, os.WriteFile(replacement, original, 0o600))
				require.NoError(t, os.Rename(replacement, path))
			case "cancel":
				cancel()
			case "closed":
				stage.Close()
			}
			_, written, err := stage.Commit(ctx)
			require.Error(t, err)
			require.False(t, written)
			if mode == "cancel" {
				require.ErrorIs(t, err, context.Canceled)
			}
			actual, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, want, actual)
			stage.Close()
			stage.Close()
			_, err = os.Stat(temporary)
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestAuthenticationScopeWriteCanceledStageAndWrongPathDoNotCreateTemps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	before, err := readAuthenticationInput(t.Context(), path)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = stageAuthenticationScopeWrite(ctx, before, authenticationCredentialEdit{path: path, data: []byte(`{}`)})
	require.ErrorIs(t, err, context.Canceled)
	_, err = stageAuthenticationScopeWrite(t.Context(), before, authenticationCredentialEdit{path: path + ".other", data: []byte(`{}`)})
	require.Error(t, err)
	_, err = stageAuthenticationScopeWrite(t.Context(), before, authenticationCredentialEdit{path: path, data: []byte(`[]`)})
	require.Error(t, err)
	files, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	require.Empty(t, files)
}

func TestAuthenticationScopeWriteReportsChangedPostimageAsWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	before, err := readAuthenticationInput(t.Context(), path)
	require.NoError(t, err)
	stage, err := stageAuthenticationScopeWrite(t.Context(), before, authenticationCredentialEdit{path: path, data: []byte(`{"approved":true}`)})
	require.NoError(t, err)
	defer stage.Close()
	// An out-of-protocol writer changes the physical temporary after staging.
	// The rename occurs, but that file is not the prepared postimage and cannot
	// authorize runtime publication or a successful receipt.
	require.NoError(t, os.WriteFile(stage.temporary, []byte(`{"foreign":true}`), 0o600))
	after, written, err := stage.Commit(t.Context())
	require.ErrorIs(t, err, errAuthenticationInputsChanged)
	require.True(t, written)
	require.False(t, after.info.exists)
	actual, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, `{"foreign":true}`, string(actual), "no blind rollback may replace the written file")
}

func TestAuthenticationScopeWriteKeepsFirstDurableDeadline(t *testing.T) {
	for _, inherited := range []string{"none", "future", "expired"} {
		t.Run(inherited, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "scope.json")
			before, err := readAuthenticationInput(t.Context(), path)
			require.NoError(t, err)
			stage, err := stageAuthenticationScopeWrite(t.Context(), before, authenticationCredentialEdit{path: path, data: []byte(`{"saved":true}`)})
			require.NoError(t, err)
			defer stage.Close()
			var deadline time.Time
			if inherited == "future" {
				deadline = time.Now().Add(time.Minute)
			}
			if inherited == "expired" {
				deadline = time.Now().Add(-time.Second)
			}
			started := time.Now()
			_, written, err := stage.commit(t.Context(), deadline)
			if inherited == "expired" {
				require.ErrorIs(t, err, context.DeadlineExceeded)
				require.False(t, written)
				require.NoFileExists(t, path)
				require.True(t, stage.completionDeadline.IsZero())
				return
			}
			require.NoError(t, err)
			require.True(t, written)
			if inherited == "future" {
				require.Equal(t, deadline, stage.completionDeadline)
			} else {
				require.WithinRange(t, stage.completionDeadline, started.Add(authenticationCompletionTimeout), time.Now().Add(authenticationCompletionTimeout))
			}
		})
	}
}
