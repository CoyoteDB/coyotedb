package agent

import (
	"context"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/control"
	"coyotedb.com/coyote/internal/pg"
)

// ConnInfo is how applications connect: the application role, over TLS,
// through any data node's client port (the proxy sends them to the main
// node). It holds a password: the control API is local and token-protected,
// and the CLI prints it only on request.
type ConnInfo struct {
	URL      string   `json:"url"`            // postgresql://app:…@h1:5432,h2:5432/app?…
	Hosts    []string `json:"hosts"`          // host:port, this node first
	User     string   `json:"user"`           // pg.AppUser
	Database string   `json:"database"`       // pg.AppDatabase
	Password string   `json:"password"`       //
	CAFile   string   `json:"ca_file"`        // the cluster CA certificate (PEM) on this machine
	Note     string   `json:"note,omitempty"` // e.g. "the application role is being set up"
	// CANote: a plain warning about CAFile's path (Windows, a folder name
	// psql/libpq cannot open there; D50).
	CANote  string `json:"ca_note,omitempty"`
	Pending bool   `json:"pending,omitempty"` // no password yet
}

// caFile is the cluster CA certificate, written so clients on this machine
// can verify the servers (sslrootcert). It is public.
func (a *Agent) caFile() string { return filepath.Join(a.node.StateDir, "ca.crt") }

func (a *Agent) writeCAFile(st cluster.State) error {
	if len(st.CA.CertDER) == 0 {
		return nil
	}
	want := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: st.CA.CertDER})
	if b, err := os.ReadFile(a.caFile()); err == nil && string(b) == string(want) {
		return nil
	}
	return pg.WriteFileDurable(a.caFile(), want, 0o644)
}

// clientHosts lists every data node's addresses with its client port, this
// node first (nearest), then by name.
func clientHosts(st cluster.State, self string) []string {
	ids := make([]string, 0, len(st.Members))
	for id, m := range st.Members {
		if m.Role == cluster.RoleData && m.ClientPort > 0 {
			ids = append(ids, id)
		}
	}
	slices.SortFunc(ids, func(x, y string) int {
		switch {
		case x == self:
			return -1
		case y == self:
			return 1
		}
		return strings.Compare(st.Members[x].Name, st.Members[y].Name)
	})
	var out []string
	for _, id := range ids {
		m := st.Members[id]
		for _, ad := range m.Addresses {
			if h, _, err := net.SplitHostPort(ad); err == nil && h != "" {
				hp := net.JoinHostPort(h, strconv.Itoa(m.ClientPort))
				if !slices.Contains(out, hp) {
					out = append(out, hp)
				}
			}
		}
	}
	return out
}

// AppURL builds the libpq URL: every host, TLS with the server verified
// against the cluster CA, and target_session_attrs=read-write so a client
// whose first host is unreachable moves on to the next.
func AppURL(hosts []string, password, caFile string) string {
	u := url.URL{Scheme: "postgresql", User: url.UserPassword(pg.AppUser, password), Path: "/" + pg.AppDatabase}
	u.Host = strings.Join(hosts, ",")
	// libpq decodes only %XX in a URI, never '+', so spaces (a data folder
	// under "Application Support") must be %20.
	esc := func(v string) string { return strings.ReplaceAll(url.QueryEscape(v), "+", "%20") }
	u.RawQuery = "sslmode=verify-full&sslrootcert=" + esc(caFile) + "&target_session_attrs=read-write&connect_timeout=10"
	return u.String()
}

func (a *Agent) connInfo() ConnInfo {
	st := a.cn.State()
	ci := ConnInfo{Hosts: clientHosts(st, a.cn.ID()), User: pg.AppUser, Database: pg.AppDatabase, CAFile: a.caFile()}
	ci.CANote = caPathNote(runtime.GOOS, ci.CAFile)
	if st.PGSecrets == nil || st.PGSecrets.App == "" {
		ci.Pending = true
		ci.Note = "the application role is not set up yet; it is created once the main node is running"
		return ci
	}
	ci.Password = st.PGSecrets.App
	ci.URL = AppURL(ci.Hosts, ci.Password, ci.CAFile)
	return ci
}

// caPathNote: on Windows, psql and other programs built on libpq open the
// sslrootcert file with the system's narrow (code page) file functions, so
// a path with letters outside plain ASCII may not open there ("root
// certificate file ... does not exist", seen on Windows). Programs not
// built on libpq (Node, Java, .NET, Go) read it fine. The URL keeps the
// true path (rewriting it would break those); the person is told what to
// do instead.
func caPathNote(goos, caFile string) string {
	if goos != "windows" {
		return ""
	}
	for _, r := range caFile {
		if r > 0x7e {
			return fmt.Sprintf("The certificate's folder name has letters that psql and other libpq-based programs on Windows may not open. "+
				"If they say the root certificate file does not exist, copy %s to a folder with a plain name (for example C:\\coyote\\ca.crt) and set sslrootcert to that path.", caFile)
		}
	}
	return ""
}

func (a *Agent) apiConnInfo(w http.ResponseWriter, r *http.Request) {
	control.WriteJSON(w, http.StatusOK, a.connInfo())
}

// apiNewAppPassword gives the application role a new random password. The
// main node applies it within a few seconds; existing sessions stay up.
func (a *Agent) apiNewAppPassword(w http.ResponseWriter, r *http.Request) {
	if st := a.cn.State(); st.PGSecrets == nil {
		control.WriteError(w, http.StatusConflict, "not_ready", "the cluster has no database yet")
		return
	}
	pw, err := pg.GeneratePassword()
	if err != nil {
		apiErr(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if _, err := a.cn.Apply(ctx, &cluster.SetAppPassword{Password: pw}); err != nil {
		apiErr(w, fmt.Errorf("could not change the password (the cluster must have a leader): %w", err))
		return
	}
	// Committed; this node's copy of the state may lag by a moment.
	ci := a.connInfo()
	ci.Pending, ci.Note, ci.Password = false, "", pw
	ci.CANote = caPathNote(runtime.GOOS, ci.CAFile)
	ci.URL = AppURL(ci.Hosts, pw, ci.CAFile)
	control.WriteJSON(w, http.StatusOK, ci)
}
