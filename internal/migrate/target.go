package migrate

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// MainNode is the cluster's main node as the local agent reports it. Epoch
// grows on every change of main node; a copy that saw two epochs is never
// switched in.
type MainNode struct {
	Name  string
	Epoch uint64
}

// Target is the cluster side, filled in by the command from the local agent.
type Target struct {
	// AppURL is the application connection string `coyote conninfo`
	// prints: every node's client port, TLS verified against the cluster
	// CA, the main node only (target_session_attrs=read-write).
	AppURL string
	// Password of the application role (kept out of URLs given to tools).
	Password string
	// Database receives the copy (default "app", the one AppURL names).
	Database string
	// Main reports the current main node.
	Main func(ctx context.Context) (MainNode, error)
	// Admin, if set, opens a superuser session to database db on this
	// machine's Postgres when this machine is the main node; it returns
	// (nil, nil) when it is not. Only used to create extensions the app
	// role may not create.
	Admin func(ctx context.Context, db string) (*pgx.Conn, error)
}

// dbURL is AppURL pointed at database db, without the password.
func (t Target) dbURL(db string) (string, error) {
	u, err := url.Parse(t.AppURL)
	if err != nil {
		return "", errors.New("the cluster's connection string could not be read")
	}
	if u.User != nil {
		u.User = url.User(u.User.Username())
	}
	u.Path = "/" + db
	u.RawPath = ""
	return u.String(), nil
}

// AppURLFor is the connection string the app uses for database db (with
// the password, as `coyote conninfo` prints it).
func (t Target) AppURLFor(db string) string {
	u, err := url.Parse(t.AppURL)
	if err != nil {
		return t.AppURL
	}
	u.Path = "/" + db
	u.RawPath = ""
	return u.String()
}

// connect opens a session as the application role on the main node,
// through the client port.
func (t Target) connect(ctx context.Context, db string) (*pgx.Conn, error) {
	s, err := t.dbURL(db)
	if err != nil {
		return nil, err
	}
	cfg, err := pgx.ParseConfig(s)
	if err != nil {
		return nil, fmt.Errorf("read the cluster's connection string: %w", err)
	}
	cfg.Password = t.Password
	cfg.RuntimeParams["application_name"] = "coyote-migrate"
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = 15 * time.Second
	}
	c, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to the cluster's main node (database %s): %w", db, cleanErr(err))
	}
	return c, nil
}

// availExt is an extension the cluster's Postgres can create.
type availExt struct {
	Superuser bool // only the administrator may create it
	Trusted   bool // a database owner may create it anyway
}

// targetInfo is what the preflight learned about the cluster side.
type targetInfo struct {
	user        string
	available   map[string]availExt
	roles       map[string]bool // every role on the cluster
	collations  map[string]bool // in pg_catalog
	exists      bool            // the target database
	owner       string
	staging     bool   // a leftover <db>__migrating
	stagingNote string // its comment (says whether a live copy runs there)
	replaced    bool   // a leftover <db>__replaced
	contents    []string
	more        int // objects beyond those in contents
}

func stagingName(db string) string  { return db + "__migrating" }
func replacedName(db string) string { return db + "__replaced" }

// maxDBName keeps <db>__migrating within Postgres' 63-byte name limit.
const maxDBName = 63 - len("__migrating")

func inspectTarget(ctx context.Context, t Target, admin *pgx.Conn) (*targetInfo, error) {
	ti := &targetInfo{available: map[string]availExt{}, collations: map[string]bool{}, roles: map[string]bool{}}
	if err := admin.QueryRow(ctx, "SELECT current_user::text").Scan(&ti.user); err != nil {
		return nil, err
	}
	rows, err := admin.Query(ctx, `SELECT v.name::text, v.superuser, v.trusted
	   FROM pg_available_extension_versions v
	   JOIN pg_available_extensions e ON e.name = v.name AND e.default_version = v.version`)
	if err != nil {
		return nil, fmt.Errorf("list the cluster's extensions: %w", err)
	}
	var name string
	var ae availExt
	if _, err := pgx.ForEachRow(rows, []any{&name, &ae.Superuser, &ae.Trusted}, func() error {
		ti.available[name] = ae
		return nil
	}); err != nil {
		return nil, fmt.Errorf("list the cluster's extensions: %w", err)
	}
	colls, err := textColumn(ctx, admin, `SELECT collname::text FROM pg_collation WHERE collnamespace = 'pg_catalog'::regnamespace`)
	if err != nil {
		return nil, fmt.Errorf("list the cluster's sort rules: %w", err)
	}
	for _, c := range colls {
		ti.collations[c] = true
	}
	roles, err := textColumn(ctx, admin, `SELECT rolname::text FROM pg_roles`)
	if err != nil {
		return nil, fmt.Errorf("list the cluster's roles: %w", err)
	}
	for _, r := range roles {
		ti.roles[r] = true
	}
	dbs := map[string]string{}
	if err := admin.QueryRow(ctx, `SELECT coalesce(shobj_description(oid, 'pg_database'), '') FROM pg_database WHERE datname = $1`,
		stagingName(t.Database)).Scan(&ti.stagingNote); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("read the cluster's databases: %w", err)
	}
	rows, err = admin.Query(ctx, `SELECT datname::text, pg_get_userbyid(datdba)::text FROM pg_database WHERE datname IN ($1, $2, $3)`,
		t.Database, stagingName(t.Database), replacedName(t.Database))
	if err != nil {
		return nil, fmt.Errorf("list the cluster's databases: %w", err)
	}
	var owner string
	if _, err := pgx.ForEachRow(rows, []any{&name, &owner}, func() error {
		dbs[name] = owner
		return nil
	}); err != nil {
		return nil, fmt.Errorf("list the cluster's databases: %w", err)
	}
	ti.owner, ti.exists = dbs[t.Database]
	_, ti.staging = dbs[stagingName(t.Database)]
	_, ti.replaced = dbs[replacedName(t.Database)]
	look := ""
	switch {
	case ti.exists:
		look = t.Database
	case ti.replaced:
		// An earlier run stopped between the two renames: the old
		// contents are under the other name and are put back first.
		look = replacedName(t.Database)
		ti.owner = dbs[look]
	}
	if look != "" && ti.owner == ti.user {
		c, err := t.connect(ctx, look)
		if err != nil {
			return nil, err
		}
		defer c.Close(context.WithoutCancel(ctx))
		if ti.contents, ti.more, err = userObjects(ctx, c); err != nil {
			return nil, fmt.Errorf("look inside database %s: %w", look, err)
		}
	}
	return ti, nil
}

// userObjects lists what a database holds beyond a fresh one: tables,
// views, sequences, functions, types, schemas other than public, and
// extensions other than the ones CoyoteDB installs itself.
func userObjects(ctx context.Context, c *pgx.Conn) ([]string, int, error) {
	const q = `
	WITH objs AS (
	  SELECT CASE c.relkind WHEN 'v' THEN 'view ' WHEN 'm' THEN 'view ' WHEN 'S' THEN 'sequence ' ELSE 'table ' END
	         || n.nspname || '.' || c.relname AS what
	    FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	   WHERE c.relkind IN ('r', 'p', 'v', 'm', 'S', 'f') AND ` + userSchemaFilter + `
	     AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e')
	  UNION ALL
	  SELECT 'function ' || n.nspname || '.' || p.proname
	    FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
	   WHERE ` + userSchemaFilter + `
	     AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e')
	  UNION ALL
	  SELECT 'type ' || n.nspname || '.' || t.typname
	    FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
	   WHERE ` + userSchemaFilter + ` AND t.typtype IN ('e', 'd', 'r', 'm')
	     AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_type'::regclass AND d.objid = t.oid AND d.deptype = 'e')
	  UNION ALL
	  SELECT 'schema ' || n.nspname FROM pg_namespace n
	   WHERE ` + userSchemaFilter + ` AND n.nspname <> 'public'
	     AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_namespace'::regclass AND d.objid = n.oid AND d.deptype = 'e')
	  UNION ALL
	  SELECT 'extension ' || extname FROM pg_extension WHERE extname NOT IN ('plpgsql', 'amcheck')
	  UNION ALL
	  SELECT 'large objects' WHERE EXISTS (SELECT 1 FROM pg_largeobject_metadata)
	)
	SELECT what, count(*) OVER () FROM objs ORDER BY what ~ '^table' DESC, what LIMIT 5`
	rows, err := c.Query(ctx, q)
	if err != nil {
		return nil, 0, err
	}
	var out []string
	total := 0
	var what string
	var n int
	if _, err := pgx.ForEachRow(rows, []any{&what, &n}, func() error {
		out = append(out, what)
		total = n
		return nil
	}); err != nil {
		return nil, 0, err
	}
	return out, total - len(out), nil
}

// cleanErr drops the noise pgx adds around a server error (the SQLSTATE
// and "ERROR:"), keeping the error itself reachable with errors.As.
func cleanErr(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return plainErr{msg: pe.Message, err: err}
	}
	return err
}

type plainErr struct {
	msg string
	err error
}

func (e plainErr) Error() string { return e.msg }
func (e plainErr) Unwrap() error { return e.err }

func sqlState(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}
