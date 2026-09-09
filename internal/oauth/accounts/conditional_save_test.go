package accounts

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConditionalImportAccountSave(t *testing.T) {
	for _, mode := range []string{"empty", "unchanged", "replacement", "identical", "active-replacement", "switch-back", "empty-logout", "remove-absent", "unrelated-inactive", "other-provider"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("AI_CLI_DIR", t.TempDir())
			ctx := t.Context()
			base := Entry{ID: "default", AccessToken: "synthetic-before"}
			if mode != "empty" && mode != "empty-logout" && mode != "remove-absent" {
				require.NoError(t, Save(ctx, "fixture", base))
			}
			if mode == "active-replacement" {
				require.NoError(t, Save(ctx, "fixture", Entry{ID: "active-other", AccessToken: "synthetic-active"}))
			}
			before, err := CaptureSaveState(ctx, "fixture", "default")
			require.NoError(t, err)
			switch mode {
			case "replacement":
				require.NoError(t, Save(ctx, "fixture", Entry{ID: "default", AccessToken: "synthetic-manual"}))
			case "identical":
				require.NoError(t, Save(ctx, "fixture", base))
			case "active-replacement":
				require.NoError(t, Save(ctx, "fixture", Entry{ID: "active-other", AccessToken: "synthetic-active"}))
			case "switch-back":
				require.NoError(t, Save(ctx, "fixture", Entry{ID: "other", AccessToken: "synthetic-other"}))
				require.NoError(t, SetActive(ctx, "fixture", "default"))
			case "empty-logout":
				require.NoError(t, RemoveProvider(ctx, "fixture"))
			case "remove-absent":
				require.NoError(t, Remove(ctx, "fixture", "default"))
			case "unrelated-inactive":
				require.NoError(t, SaveWithoutActivating(ctx, "fixture", Entry{ID: "other", AccessToken: "synthetic-other"}))
			case "other-provider":
				require.NoError(t, Save(ctx, "independent", Entry{ID: "default", AccessToken: "synthetic-other"}))
			}
			path, err := dbPath()
			require.NoError(t, err)
			diskBefore, readErr := os.ReadFile(path)
			require.True(t, readErr == nil || os.IsNotExist(readErr))
			err = before.SaveForOwner(ctx, Entry{ID: "default", AccessToken: "synthetic-imported"}, func() error { return nil })
			if mode == "empty" || mode == "unchanged" || mode == "unrelated-inactive" || mode == "other-provider" {
				require.NoError(t, err)
				entry, err := Active(ctx, "fixture")
				require.NoError(t, err)
				require.Equal(t, "synthetic-imported", entry.AccessToken)
			} else {
				require.ErrorIs(t, err, ErrCredentialChanged)
				diskAfter, readErr := os.ReadFile(path)
				require.True(t, readErr == nil || os.IsNotExist(readErr))
				require.Equal(t, diskBefore, diskAfter)
			}
		})
	}
}

func TestConditionalAccountSaveRequiresCapturedIdentityAndOwner(t *testing.T) {
	t.Setenv("AI_CLI_DIR", t.TempDir())
	before, err := CaptureSaveState(t.Context(), "fixture", "default")
	require.NoError(t, err)
	require.Error(t, before.SaveForOwner(t.Context(), Entry{ID: "different"}, func() error { return nil }))
	require.Error(t, before.SaveForOwner(t.Context(), Entry{ID: "default"}, nil))
	require.ErrorIs(t, before.SaveForOwner(t.Context(), Entry{ID: "default"}, func() error { return context.Canceled }), context.Canceled)
	entries, err := List(t.Context(), "fixture")
	require.NoError(t, err)
	require.Empty(t, entries)
}
