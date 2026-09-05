package install

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

func validatePrivateAccess(path string, _ fs.FileInfo) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	insecure := fmt.Errorf("private compatibility path %q has an insecure ACL; expected no group or other access except SYSTEM and Administrators", path)
	if descriptor == nil {
		return insecure
	}
	allowed := func(sid *windows.SID) bool {
		return sid != nil && (sid.Equals(user.User.Sid) || sid.IsWellKnown(windows.WinLocalSystemSid) || sid.IsWellKnown(windows.WinBuiltinAdministratorsSid))
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return err
	}
	if !allowed(owner) {
		return insecure
	}
	acl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	if acl == nil {
		return insecure
	}
	userAccess := false
	for index := uint32(0); index < uint32(acl.AceCount); index++ {
		var entry *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, index, &entry); err != nil {
			return err
		}
		if entry.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if entry.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return insecure
		}
		sid := (*windows.SID)(unsafe.Pointer(&entry.SidStart))
		if !allowed(sid) {
			return insecure
		}
		if sid.Equals(user.User.Sid) && entry.Header.AceFlags&windows.INHERIT_ONLY_ACE == 0 && entry.Mask != 0 {
			userAccess = true
		}
	}
	if !userAccess {
		return insecure
	}
	return nil
}

func createPrivateDirectory(path string) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + user.User.Sid.String() + ")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attributes := windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: descriptor,
	}
	return windows.CreateDirectory(name, &attributes)
}
