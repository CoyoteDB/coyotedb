//go:build windows

package pg

// postgres.exe refuses to run with administrative rights ("Execution of
// PostgreSQL by a user with administrative permissions is not permitted").
// An agent started from an elevated terminal ("Terminal (Admin)", or any
// SSH login of an administrator account, which Windows OpenSSH always
// elevates) has such rights, and Postgres would never start. pg_ctl and
// initdb solve this for themselves the same way we do here: they start
// the server with a restricted copy of their own token in which the
// Administrators and Power Users groups are deny-only and every privilege
// but SeChangeNotifyPrivilege is removed (PostgreSQL's
// src/common/restricted_token.c). Postgres' own admin check
// (pgwin32_is_admin: CheckTokenMembership on those two groups) then
// passes, because deny-only groups do not count as membership.
//
// One more step, also from PostgreSQL (AddUserToTokenDacl in
// src/common/exec.c): an elevated token's default DACL, which Windows puts
// on every pipe, process and thread the program creates, grants only
// Administrators and SYSTEM. With Administrators deny-only, postgres.exe
// could not use its own pipes or child processes ("could not execute
// command ... -V: Permission denied" at startup, seen on a Windows test PC). So the
// token's user is added to its default DACL.
//
// Only Postgres programs get the restricted token. The agent and its
// watchdog keep their rights, so they can always stop Postgres.

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modadvapi32               = windows.NewLazySystemDLL("advapi32.dll")
	procCreateRestrictedToken = modadvapi32.NewProc("CreateRestrictedToken")
)

const (
	disableMaxPrivilege = 0x1      // DISABLE_MAX_PRIVILEGE
	fileAllAccess       = 0x1F01FF // FILE_ALL_ACCESS (what GENERIC_ALL maps to on files)
)

// adminGroups are the groups Postgres refuses to run as (pgwin32_is_admin).
var adminGroups = []windows.WELL_KNOWN_SID_TYPE{windows.WinBuiltinAdministratorsSid, windows.WinBuiltinPowerUsersSid}

// runningAsAdmin reports whether this process's token holds an enabled
// Administrators or Power Users group: the test postgres.exe makes. A
// non-elevated administrator (UAC split token) has them deny-only.
func runningAsAdmin() (bool, error) {
	for _, g := range adminGroups {
		sid, err := windows.CreateWellKnownSid(g)
		if err != nil {
			return false, err
		}
		// Token 0: the calling thread's effective token, as postgres does.
		member, err := windows.Token(0).IsMember(sid)
		if err != nil {
			return false, fmt.Errorf("check group membership: %w", err)
		}
		if member {
			return true, nil
		}
	}
	return false, nil
}

// postgresToken returns the token Postgres programs must run with: 0 (the
// agent's own) when the agent has no administrative rights, else a
// restricted primary token as described above. The caller owns it.
func postgresToken() (windows.Token, error) {
	admin, err := runningAsAdmin()
	if err != nil || !admin {
		return 0, err
	}
	var own windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(),
		windows.TOKEN_DUPLICATE|windows.TOKEN_ASSIGN_PRIMARY|windows.TOKEN_QUERY|windows.TOKEN_ADJUST_DEFAULT, &own); err != nil {
		return 0, fmt.Errorf("open own token: %w", err)
	}
	defer own.Close()
	drop := make([]windows.SIDAndAttributes, 0, len(adminGroups))
	for _, g := range adminGroups {
		sid, err := windows.CreateWellKnownSid(g)
		if err != nil {
			return 0, err
		}
		drop = append(drop, windows.SIDAndAttributes{Sid: sid})
	}
	var restricted windows.Token
	r, _, e := procCreateRestrictedToken.Call(
		uintptr(own), disableMaxPrivilege,
		uintptr(len(drop)), uintptr(unsafe.Pointer(&drop[0])),
		0, 0, // privileges to delete: all but SeChangeNotify via the flag
		0, 0, // restricting SIDs: none
		uintptr(unsafe.Pointer(&restricted)))
	if r == 0 {
		return 0, fmt.Errorf("create restricted token for postgres: %w", e)
	}
	if err := addUserToDefaultDACL(restricted); err != nil {
		restricted.Close()
		return 0, err
	}
	return restricted, nil
}

// tokenDefaultDACL is TOKEN_DEFAULT_DACL.
type tokenDefaultDACL struct{ DefaultDacl *windows.ACL }

// addUserToDefaultDACL grants the token's own user full access in the
// token's default DACL, keeping the entries already there.
func addUserToDefaultDACL(t windows.Token) error {
	var n uint32
	err := windows.GetTokenInformation(t, windows.TokenDefaultDacl, nil, 0, &n)
	if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) {
		return fmt.Errorf("read token default DACL size: %w", err)
	}
	buf := make([]byte, n)
	if err := windows.GetTokenInformation(t, windows.TokenDefaultDacl, &buf[0], n, &n); err != nil {
		return fmt.Errorf("read token default DACL: %w", err)
	}
	old := (*tokenDefaultDACL)(unsafe.Pointer(&buf[0])).DefaultDacl
	user, err := t.GetTokenUser()
	if err != nil {
		return fmt.Errorf("read token user: %w", err)
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid),
		},
	}}, old)
	if err != nil {
		return fmt.Errorf("build token default DACL: %w", err)
	}
	d := tokenDefaultDACL{DefaultDacl: acl}
	if err := windows.SetTokenInformation(t, windows.TokenDefaultDacl,
		(*byte)(unsafe.Pointer(&d)), uint32(unsafe.Sizeof(d))); err != nil {
		return fmt.Errorf("set token default DACL: %w", err)
	}
	return nil
}

// grantUserDataDir makes sure the restricted token's user can change every
// file in the data folder, also the ones the elevated agent writes. In a
// folder that inherits C:\ProgramData's ACL (Users: read only, CREATOR
// OWNER: full), a file the elevated agent creates is owned by
// Administrators, which the restricted token has deny-only: pg_rewind then
// failed with "could not open target file …coyote.conf: Permission denied"
// and the node fell back to a full re-clone (seen on a Windows test PC). One explicit,
// inheritable full-control entry for the user on the data folder fixes
// that; it is added once (Windows passes it down to existing files then),
// so later starts do not walk the whole tree again.
func grantUserDataDir(dir string, t windows.Token) error {
	user, err := t.GetTokenUser()
	if err != nil {
		return fmt.Errorf("read token user: %w", err)
	}
	sid := user.User.Sid
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read the ACL of %s: %w", dir, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("read the ACL of %s: %w", dir, err)
	}
	if hasInheritableFullControl(dacl, sid) {
		return nil
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: fileAllAccess, // concrete rights, so hasInheritableFullControl finds it next start
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}}, dacl)
	if err != nil {
		return fmt.Errorf("build the ACL of %s: %w", dir, err)
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		return fmt.Errorf("give %s full access to %s: %w", sid, dir, err)
	}
	return nil
}

// hasInheritableFullControl reports whether acl has an explicit allow entry
// for sid with full file access that folders and files inherit.
func hasInheritableFullControl(acl *windows.ACL, sid *windows.SID) bool {
	if acl == nil {
		return false
	}
	const inherit = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE
	for i := range uint32(acl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &ace); err != nil {
			return false
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE ||
			ace.Header.AceFlags&windows.INHERITED_ACE != 0 ||
			ace.Header.AceFlags&inherit != inherit ||
			ace.Mask&fileAllAccess != fileAllAccess {
			continue
		}
		if (*windows.SID)(unsafe.Pointer(&ace.SidStart)).Equals(sid) {
			return true
		}
	}
	return false
}
