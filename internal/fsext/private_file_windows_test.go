package fsext

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestValidatePrivateFileACL(t *testing.T) {
	t.Parallel()

	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	require.NoError(t, err)
	for _, broad := range []bool{false, true} {
		name := "private"
		if broad {
			name = "everyone"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "private.json")
			require.NoError(t, os.WriteFile(path, []byte("{}"), 0o600))
			sddl := "D:P(A;;FA;;;" + user.User.Sid.String() + ")(A;;FA;;;SY)(A;;FA;;;BA)"
			if broad {
				sddl += "(A;;FR;;;WD)"
			}
			descriptor, err := windows.SecurityDescriptorFromString(sddl)
			require.NoError(t, err)
			acl, _, err := descriptor.DACL()
			require.NoError(t, err)
			require.NoError(t, windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil))
			file, err := OpenSharedRead(path)
			require.NoError(t, err)
			defer file.Close()
			if broad {
				require.Error(t, ValidatePrivateFile(file))
			} else {
				require.NoError(t, ValidatePrivateFile(file))
			}
		})
	}
}

func TestOpenSharedReadAllowsReplacement(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "snapshot.json")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o600))
	file, err := OpenSharedRead(path)
	require.NoError(t, err)
	defer file.Close()
	before, err := file.Stat()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path+".new", []byte("new"), 0o600))
	require.NoError(t, os.Rename(path, path+".previous"))
	require.NoError(t, os.Rename(path+".new", path))
	after, err := os.Stat(path)
	require.NoError(t, err)
	require.False(t, os.SameFile(before, after))
}
