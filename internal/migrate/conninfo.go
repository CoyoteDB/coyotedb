// Package migrate moves an existing PostgreSQL database into a CoyoteDB
// cluster (`coyote migrate`, M8, DECISIONS.md D47).
//
// The copy is pg_dump (directory format, parallel, one snapshot) followed
// by pg_restore as the application role through the cluster's client port,
// into a staging database that is switched into place only after every
// table has been checked against the source. Nothing here talks to
// Postgres' internals or the cluster's state directly: the copy reaches the
// other nodes through normal replication.
package migrate

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// connParams is a libpq connection string split into keywords, in the
// order they were given. The password is kept apart so it never lands on a
// command line (it goes to tools through PGPASSWORD).
type connParams struct {
	keys     []string
	vals     map[string]string
	password string
}

func (p *connParams) set(k, v string) {
	if p.vals == nil {
		p.vals = map[string]string{}
	}
	if _, ok := p.vals[k]; !ok {
		p.keys = append(p.keys, k)
	}
	p.vals[k] = v
}

func (p *connParams) get(k string) string { return p.vals[k] }

// parseConnString accepts both libpq forms: a URI
// (postgresql://user:pass@host:port,host2/db?sslmode=require) and
// keyword=value pairs (host=... dbname=... password='...').
func parseConnString(s string) (connParams, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return connParams{}, errors.New("the connection string is empty")
	}
	var p connParams
	var err error
	if strings.HasPrefix(s, "postgresql://") || strings.HasPrefix(s, "postgres://") {
		err = parseURI(s, &p)
	} else {
		err = parseKeywords(s, &p)
	}
	if err != nil {
		return connParams{}, err
	}
	if pw, ok := p.vals["password"]; ok {
		p.password = pw
		delete(p.vals, "password")
		p.keys = removeString(p.keys, "password")
	}
	return p, nil
}

func removeString(xs []string, x string) []string {
	out := xs[:0]
	for _, v := range xs {
		if v != x {
			out = append(out, v)
		}
	}
	return out
}

func parseURI(s string, p *connParams) error {
	_, rest, _ := strings.Cut(s, "://")
	// Split off the query first: a password may contain '/' or '?' only
	// when percent-encoded, as libpq requires.
	query := ""
	if i := strings.IndexByte(rest, '?'); i >= 0 {
		rest, query = rest[:i], rest[i+1:]
	}
	path := ""
	hostPart := rest
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		hostPart, path = rest[:i], rest[i+1:]
	}
	if i := strings.LastIndexByte(hostPart, '@'); i >= 0 {
		userinfo := hostPart[:i]
		hostPart = hostPart[i+1:]
		user, pass, hasPass := strings.Cut(userinfo, ":")
		u, err := url.PathUnescape(user)
		if err != nil {
			return fmt.Errorf("the user name in the connection string is not valid: %w", err)
		}
		if u != "" {
			p.set("user", u)
		}
		if hasPass {
			pw, err := url.PathUnescape(pass)
			if err != nil {
				return errors.New("the password in the connection string is not valid percent-encoding")
			}
			p.set("password", pw)
		}
	}
	if hostPart != "" {
		var hosts, ports []string
		anyPort := false
		for _, hp := range strings.Split(hostPart, ",") {
			h, port, err := splitHostPort(hp)
			if err != nil {
				return err
			}
			hosts = append(hosts, h)
			ports = append(ports, port)
			if port != "" {
				anyPort = true
			}
		}
		p.set("host", strings.Join(hosts, ","))
		if anyPort {
			if len(ports) > 1 && allSame(ports) {
				p.set("port", ports[0])
			} else {
				p.set("port", strings.Join(ports, ","))
			}
		}
	}
	if path != "" {
		db, err := url.PathUnescape(path)
		if err != nil {
			return fmt.Errorf("the database name in the connection string is not valid: %w", err)
		}
		p.set("dbname", db)
	}
	if query != "" {
		for _, kv := range strings.Split(query, "&") {
			if kv == "" {
				continue
			}
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				return fmt.Errorf("the connection string option %q has no value", k)
			}
			k, err1 := url.QueryUnescape(k)
			v, err2 := url.PathUnescape(v)
			if err1 != nil || err2 != nil {
				return fmt.Errorf("the connection string option %q is not valid percent-encoding", k)
			}
			if k == "ssl" && v == "true" { // libpq accepts this JDBC-style spelling
				k, v = "sslmode", "require"
			}
			p.set(k, v)
		}
	}
	return nil
}

func allSame(xs []string) bool {
	for _, x := range xs {
		if x != xs[0] {
			return false
		}
	}
	return true
}

func splitHostPort(hp string) (host, port string, err error) {
	hp, err = url.PathUnescape(hp)
	if err != nil {
		return "", "", fmt.Errorf("the host %q in the connection string is not valid", hp)
	}
	if strings.HasPrefix(hp, "[") {
		end := strings.IndexByte(hp, ']')
		if end < 0 {
			return "", "", fmt.Errorf("the host %q in the connection string is missing a closing ]", hp)
		}
		host, rest := hp[1:end], hp[end+1:]
		if rest == "" {
			return host, "", nil
		}
		if !strings.HasPrefix(rest, ":") {
			return "", "", fmt.Errorf("the host %q in the connection string is not valid", hp)
		}
		return host, rest[1:], nil
	}
	if i := strings.LastIndexByte(hp, ':'); i >= 0 {
		return hp[:i], hp[i+1:], nil
	}
	return hp, "", nil
}

// parseKeywords parses `key=value key2='quoted value'`.
func parseKeywords(s string, p *connParams) error {
	i := 0
	skip := func() {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
			i++
		}
	}
	for {
		skip()
		if i >= len(s) {
			return nil
		}
		start := i
		for i < len(s) && s[i] != '=' && s[i] != ' ' && s[i] != '\t' {
			i++
		}
		key := s[start:i]
		skip()
		if i >= len(s) || s[i] != '=' {
			return fmt.Errorf("the connection string %q is neither a postgresql:// address nor key=value pairs", short(s))
		}
		i++
		skip()
		var val strings.Builder
		if i < len(s) && s[i] == '\'' {
			i++
			closed := false
			for i < len(s) {
				c := s[i]
				if c == '\\' && i+1 < len(s) {
					val.WriteByte(s[i+1])
					i += 2
					continue
				}
				if c == '\'' {
					i++
					closed = true
					break
				}
				val.WriteByte(c)
				i++
			}
			if !closed {
				return fmt.Errorf("the value of %q in the connection string has no closing quote", key)
			}
		} else {
			for i < len(s) && s[i] != ' ' && s[i] != '\t' && s[i] != '\n' {
				if s[i] == '\\' && i+1 < len(s) {
					val.WriteByte(s[i+1])
					i += 2
					continue
				}
				val.WriteByte(s[i])
				i++
			}
		}
		if key == "" {
			return errors.New("the connection string has a value without a key")
		}
		p.set(key, val.String())
	}
}

// short keeps error messages from echoing a whole connection string (it may
// hold a password).
func short(s string) string {
	if len(s) > 12 {
		return s[:12] + "…"
	}
	return s
}

// quoteValue quotes a keyword value for a libpq keyword=value string.
func quoteValue(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `'`, `\'`)
	return "'" + v + "'"
}

// conninfo renders the parameters as keyword=value pairs, without the
// password. Safe to put on a command line.
func (p connParams) conninfo() string {
	var b strings.Builder
	for _, k := range p.keys {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(quoteValue(p.vals[k]))
	}
	return b.String()
}

// withPassword is conninfo plus the password, for in-process drivers only.
func (p connParams) withPassword() string {
	s := p.conninfo()
	if p.password != "" {
		s += " password=" + quoteValue(p.password)
	}
	return s
}

// describe names the server and database in plain words, never the password.
func (p connParams) describe() string {
	host := p.get("host")
	if host == "" {
		host = "this computer (local socket)"
	} else if first, _, ok := strings.Cut(host, ","); ok {
		host = first + " (and others)"
	}
	if port := p.get("port"); port != "" && !strings.Contains(port, ",") {
		host = net.JoinHostPort(host, port)
	}
	db := p.get("dbname")
	if db == "" {
		db = p.get("user")
	}
	if db == "" {
		return host
	}
	return host + ", database " + db
}
