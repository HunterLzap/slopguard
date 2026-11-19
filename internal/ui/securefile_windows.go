//go:build windows

package ui

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// hardenSecretDir applies a protected DACL to state directories. os.Chmod
// cannot express Unix mode bits on Windows, so use the native ACL API instead.
func hardenSecretDir(path string) error {
	return setOwnerOnlyDACL(path)
}

// hardenSecretFile applies a protected DACL to token/backup files.
func hardenSecretFile(path string) error {
	return setOwnerOnlyDACL(path)
}

func setOwnerOnlyDACL(path string) error {
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("get current user: %w", err)
	}
	// SYSTEM and Administrators keep access for system maintenance; the
	// current user is the only non-privileged principal allowed. OICI lets the
	// directory DACL propagate to files created underneath it.
	sddl := fmt.Sprintf("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;%s)", tokenUser.User.Sid.String())
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return fmt.Errorf("build security descriptor: %w", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("read DACL: %w", err)
	}
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil,
	); err != nil {
		return fmt.Errorf("set DACL: %w", err)
	}
	return nil
}
