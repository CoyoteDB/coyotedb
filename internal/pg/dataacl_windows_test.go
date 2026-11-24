//go:build windows

package pg

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// A new data folder, and a file made in it afterwards, are readable only
// by SYSTEM, Administrators and the user running the agent.
func TestProtectDataDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "node")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := protectDataDir(dir); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(dir, "superuser.password")
	if err := os.WriteFile(secret, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	system, _ := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	admins, _ := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	for _, p := range []string{dir, secret} {
		sd, err := windows.GetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		dacl, _, err := sd.DACL()
		if err != nil {
			t.Fatal(err)
		}
		sids, err := aclEntries(dacl)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range sids {
			if !s.Equals(user.User.Sid) && !s.Equals(system) && !s.Equals(admins) {
				t.Errorf("%s: unexpected access for %s", p, s)
			}
		}
	}
	sd, _ := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if ctl, _, _ := sd.Control(); ctl&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("data folder DACL not protected from its parent")
	}
	// Already protected: left as it is.
	if err := protectDataDir(dir); err != nil {
		t.Fatal(err)
	}
}
