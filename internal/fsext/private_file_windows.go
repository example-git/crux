package fsext

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func ValidatePrivateFile(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("private file is not regular")
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	descriptor, err := windows.GetSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	insecure := errors.New("private file has an insecure ACL")
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

func OpenSharedRead(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil
}
