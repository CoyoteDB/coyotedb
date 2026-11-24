//go:build windows

package pg

// The data folder holds the superuser password, the cluster secrets and
// TLS keys. On Unix it is 0700 and those files 0600; Windows ignores mode
// bits, so a folder made by `coyote up` would just inherit its parent's
// ACL: under D:\ or C:\ProgramData that lets every local user read the
// superuser password (seen on a Windows test PC: Authenticated Users had Modify). So the
// data folder gets a protected ACL, inherited by everything in it: full
// control for SYSTEM, Administrators and the user running the agent, and
// nobody else. A folder that already has a protected ACL is left alone:
// `coyote service install` sets its own (SYSTEM, Administrators, the
// service's SID), and re-applying on every start would walk a big data
// folder each time.

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func protectDataDir(dir string) error {
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read the permissions of %s: %w", dir, err)
	}
	ctl, _, err := sd.Control()
	if err != nil {
		return fmt.Errorf("read the permissions of %s: %w", dir, err)
	}
	if ctl&windows.SE_DACL_PROTECTED != 0 {
		return nil
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("read this process's user: %w", err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}
	var entries []windows.EXPLICIT_ACCESS
	for _, s := range []struct {
		sid *windows.SID
		typ windows.TRUSTEE_TYPE
	}{{system, windows.TRUSTEE_IS_WELL_KNOWN_GROUP}, {admins, windows.TRUSTEE_IS_GROUP}, {user.User.Sid, windows.TRUSTEE_IS_USER}} {
		entries = append(entries, windows.EXPLICIT_ACCESS{
			AccessPermissions: fileAllAccess,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  s.typ,
				TrusteeValue: windows.TrusteeValueFromSID(s.sid),
			},
		})
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return fmt.Errorf("build the permissions of %s: %w", dir, err)
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		return fmt.Errorf("make %s private: %w", dir, err)
	}
	return nil
}
