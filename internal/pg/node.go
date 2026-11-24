package pg

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"coyotedb.com/coyote/internal/durable"
)

// Superuser is the Postgres superuser created by initdb.
const Superuser = "postgres"

// Node is one machine's state directory. Layout:
//
//	<state>/pgdata/              Postgres data directory (0700)
//	<state>/superuser.password   generated superuser password (0600)
//	<state>/node.json            small facts for other commands (port, build)
//	<state>/coyote.lock          held by the running agent (and its watchdog)
//	<state>/logs/                agent, watchdog and Postgres logs
type Node struct {
	StateDir string // absolute
}

// NewNode returns a Node for dir (made absolute).
func NewNode(dir string) (Node, error) {
	if dir == "" {
		return Node{}, errors.New("no data dir")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Node{}, err
	}
	return Node{StateDir: abs}, nil
}

func (n Node) PGData() string { return filepath.Join(n.StateDir, "pgdata") }
func (n Node) LogDir() string { return filepath.Join(n.StateDir, "logs") }

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// TLSFiles are the Postgres server certificate and key (PEM) the agent
// writes for client TLS. They live outside pgdata so a base backup or a
// rewind never copies them to another node.
func (n Node) TLSFiles() (cert, key string) {
	d := filepath.Join(n.StateDir, "pgtls")
	return filepath.Join(d, "server.crt"), filepath.Join(d, "server.key")
}
func (n Node) PasswordFile() string { return filepath.Join(n.StateDir, "superuser.password") }
func (n Node) LockPath() string     { return filepath.Join(n.StateDir, "coyote.lock") }
func (n Node) InfoPath() string     { return filepath.Join(n.StateDir, "node.json") }
func (n Node) initTmp() string      { return filepath.Join(n.StateDir, "pgdata.init-tmp") }

// Prepare creates the state and log directories with private permissions.
func (n Node) Prepare() error {
	if err := checkPostgresPath(n.StateDir); err != nil { // Windows only (confenc_windows.go)
		return err
	}
	if err := os.MkdirAll(n.StateDir, 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	if err := protectDataDir(n.StateDir); err != nil { // Windows: private ACL (dataacl_windows.go)
		return err
	}
	if err := os.MkdirAll(n.LogDir(), 0o700); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}
	return nil
}

// Initialized reports whether pgdata holds an initialised cluster.
func (n Node) Initialized() bool {
	_, err := os.Stat(filepath.Join(n.PGData(), "PG_VERSION"))
	return err == nil
}

// InitDB creates the Postgres cluster if it does not exist yet. It reports
// whether it created one.
//
// initdb runs into a temporary folder that is renamed into place only on
// success, so a crash mid-initdb never leaves a half-made pgdata behind.
func (n Node) InitDB(ctx context.Context, inst Install, log *slog.Logger) (bool, error) {
	if n.Initialized() {
		v, err := os.ReadFile(filepath.Join(n.PGData(), "PG_VERSION"))
		if err != nil {
			return false, err
		}
		if got := strings.TrimSpace(string(v)); got != MajorVersion {
			return false, fmt.Errorf("data dir was made by PostgreSQL %s, this program runs %s", got, MajorVersion)
		}
		return false, nil
	}
	if _, err := os.Stat(n.PGData()); err == nil {
		return false, fmt.Errorf("%s exists but is not a Postgres data dir; move it away first", n.PGData())
	}
	if isRoot() {
		return false, errors.New("Postgres will not run as root/Administrator; run as a normal user")
	}
	tmp := n.initTmp()
	if err := os.RemoveAll(tmp); err != nil {
		return false, fmt.Errorf("remove leftover %s: %w", tmp, err)
	}

	pw, err := generatePassword()
	if err != nil {
		return false, err
	}
	if err := writeFileAtomic(n.PasswordFile(), []byte(pw+"\n"), 0o600); err != nil {
		return false, fmt.Errorf("write password file: %w", err)
	}

	args := []string{
		"-D", tmp,
		"-U", Superuser,
		"--pwfile=" + n.PasswordFile(),
		"--auth=scram-sha-256",
		"--encoding=UTF8",
		// Safety rule 1: builtin provider, never libc collation. --locale=C
		// keeps the remaining LC_* settings platform independent too.
		"--locale=C",
		"--locale-provider=builtin",
		"--builtin-locale=C.UTF-8",
		"--data-checksums",
	}
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, inst.Bin("initdb"), args...)
	cmd.Env = cleanEnv()
	cmd.Stdout = &out
	cmd.Stderr = &out
	log.Info("running initdb", "bin", inst.Bin("initdb"), "args", args)
	err = cmd.Run()
	_ = os.WriteFile(filepath.Join(n.LogDir(), "initdb.log"), out.Bytes(), 0o600)
	if err != nil {
		_ = os.RemoveAll(tmp)
		return false, fmt.Errorf("initdb failed: %w\n%s", err, tail(out.String(), 20))
	}
	// initdb synced the new data dir; the rename must be on disk too, or a
	// power cut can leave the data under the temporary name.
	if err := durable.Rename(tmp, n.PGData()); err != nil {
		return false, fmt.Errorf("move new data dir into place: %w", err)
	}
	return true, nil
}

// Password reads the superuser password.
func (n Node) Password() (string, error) {
	b, err := os.ReadFile(n.PasswordFile())
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// NodeInfo is persisted so other commands (conninfo, status) know the port.
type NodeInfo struct {
	Name      string `json:"name,omitempty"`
	Port      int    `json:"port"`
	PGVersion string `json:"pg_version"`
	Target    string `json:"target"`
}

// WriteInfo persists NodeInfo.
func (n Node) WriteInfo(i NodeInfo) error {
	b, err := json.MarshalIndent(i, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(n.InfoPath(), append(b, '\n'), 0o600)
}

// ReadInfo loads NodeInfo.
func (n Node) ReadInfo() (NodeInfo, error) {
	var i NodeInfo
	b, err := os.ReadFile(n.InfoPath())
	if errors.Is(err, fs.ErrNotExist) {
		return i, fmt.Errorf("no node found in %s (run `up` first)", n.StateDir)
	}
	if err != nil {
		return i, err
	}
	return i, json.Unmarshal(b, &i)
}

// ConnString returns a libpq URL for the local Postgres. The password is
// included only when withPassword is true.
func ConnString(port int, password string, withPassword bool) string {
	u := url.URL{
		Scheme:   "postgresql",
		Host:     net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		Path:     "/postgres",
		RawQuery: "sslmode=disable",
	}
	if withPassword {
		u.User = url.UserPassword(Superuser, password)
	} else {
		u.User = url.User(Superuser)
	}
	return u.String()
}

// GeneratePassword returns a new random password (32 URL-safe characters).
func GeneratePassword() (string, error) { return generatePassword() }

func generatePassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// cleanEnv returns the environment minus libpq/Postgres variables that could
// redirect our tools at another server or data dir.
func cleanEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(strings.ToUpper(k), "PG") {
			continue
		}
		env = append(env, kv)
	}
	return env
}

// tail returns the last n lines of s.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func isRoot() bool { return os.Geteuid() == 0 }
