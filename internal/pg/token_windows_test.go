//go:build windows

package pg

import (
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The token Postgres gets must pass postgres.exe's own admin check
// (CheckTokenMembership on Administrators and Power Users) when the agent
// is elevated, and must be the agent's own token otherwise.
func TestPostgresTokenDropsAdminRights(t *testing.T) {
	admin, err := runningAsAdmin()
	if err != nil {
		t.Fatal(err)
	}
	tok, err := postgresToken()
	if err != nil {
		t.Fatal(err)
	}
	if !admin {
		if tok != 0 {
			tok.Close()
			t.Fatal("not elevated, but got a restricted token")
		}
		t.Log("test process has no administrative rights; restricted-token path needs an elevated run")
		return
	}
	if tok == 0 {
		t.Fatal("elevated, but no restricted token")
	}
	defer tok.Close()
	// CheckTokenMembership needs an impersonation token.
	var imp windows.Token
	if err := windows.DuplicateTokenEx(tok, windows.TOKEN_QUERY|windows.TOKEN_IMPERSONATE, nil,
		windows.SecurityIdentification, windows.TokenImpersonation, &imp); err != nil {
		t.Fatal(err)
	}
	defer imp.Close()
	for _, g := range adminGroups {
		sid, err := windows.CreateWellKnownSid(g)
		if err != nil {
			t.Fatal(err)
		}
		member, err := imp.IsMember(sid)
		if err != nil {
			t.Fatal(err)
		}
		if member {
			t.Fatalf("restricted token is still a member of %s", sid)
		}
	}
	groups, err := tok.GetTokenGroups()
	if err != nil {
		t.Fatal(err)
	}
	adm, _ := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	found := false
	for _, g := range groups.AllGroups() {
		if g.Sid.Equals(adm) {
			found = true
			if g.Attributes&windows.SE_GROUP_USE_FOR_DENY_ONLY == 0 {
				t.Fatalf("Administrators is not deny-only (attributes %#x)", g.Attributes)
			}
		}
	}
	if !found {
		t.Fatal("Administrators group missing from the restricted token")
	}
}

// The restricted token's default DACL must grant its own user access, or
// postgres.exe cannot use its own pipes and child processes.
func TestPostgresTokenDefaultDACLHasUser(t *testing.T) {
	tok, err := postgresToken()
	if err != nil {
		t.Fatal(err)
	}
	if tok == 0 {
		t.Log("not elevated: Postgres runs with the agent's own token; nothing to check")
		return
	}
	defer tok.Close()
	var n uint32
	_ = windows.GetTokenInformation(tok, windows.TokenDefaultDacl, nil, 0, &n)
	buf := make([]byte, n)
	if err := windows.GetTokenInformation(tok, windows.TokenDefaultDacl, &buf[0], n, &n); err != nil {
		t.Fatal(err)
	}
	acl := (*tokenDefaultDACL)(unsafe.Pointer(&buf[0])).DefaultDacl
	user, err := tok.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := aclEntries(acl)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Equals(user.User.Sid) {
			return
		}
	}
	t.Fatalf("default DACL does not name the token user %s", user.User.Sid)
}

// aclEntries lists the SIDs of an ACL's allow entries.
func aclEntries(acl *windows.ACL) ([]*windows.SID, error) {
	var out []*windows.SID
	for i := uint32(0); ; i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &ace); err != nil {
			if len(out) > 0 {
				return out, nil
			}
			return nil, err
		}
		out = append(out, (*windows.SID)(unsafe.Pointer(&ace.SidStart)))
	}
}

// The elevated agent's data folder gets one inheritable full-control entry
// for the restricted token's user, added once.
func TestGrantUserDataDir(t *testing.T) {
	tok, err := postgresToken()
	if err != nil {
		t.Fatal(err)
	}
	if tok == 0 {
		t.Log("not elevated: no grant is made; nothing to check")
		return
	}
	defer tok.Close()
	dir := t.TempDir()
	user, err := tok.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if err := grantUserDataDir(dir, tok); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		dacl, _, err := sd.DACL()
		if err != nil {
			t.Fatal(err)
		}
		if !hasInheritableFullControl(dacl, user.User.Sid) {
			t.Fatal("no inheritable full-control entry for the user")
		}
		return int(dacl.AceCount)
	}
	n := count()
	if err := grantUserDataDir(dir, tok); err != nil {
		t.Fatal(err)
	}
	if count() != n {
		t.Fatal("second grant changed the ACL")
	}
}
