package paths

import (
	"path/filepath"
	"testing"
)

func TestDefaultDataDir(t *testing.T) {
	env := map[string]string{"ProgramData": `C:\ProgramData`}
	getenv := func(k string) string { return env[k] }
	home := func() (string, error) { return filepath.Join("home", "u"), nil }
	user := func() int { return 1000 }
	root := func() int { return 0 }

	cases := []struct {
		goos string
		euid func() int
		want string
	}{
		{"windows", user, filepath.Join(`C:\ProgramData`, "CoyoteDB")},
		{"darwin", user, filepath.Join("home", "u", "Library", "Application Support", "CoyoteDB")},
		{"linux", user, filepath.Join("home", "u", ".local", "share", "coyote")},
		{"linux", root, filepath.Join(string(filepath.Separator), "var", "lib", "coyote")},
		{"darwin", root, filepath.Join(string(filepath.Separator), "Library", "Application Support", "CoyoteDB")},
		{"windows", root, filepath.Join(`C:\ProgramData`, "CoyoteDB")},
	}
	for _, c := range cases {
		got, err := defaultDataDir(c.goos, getenv, home, c.euid)
		if err != nil || got != c.want {
			t.Errorf("%s: got %q, %v; want %q", c.goos, got, err, c.want)
		}
	}
	if _, err := defaultDataDir("windows", func(string) string { return "" }, home, user); err == nil {
		t.Error("windows without ProgramData should fail")
	}
}
