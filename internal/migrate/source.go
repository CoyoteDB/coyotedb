package migrate

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Oldest and newest source versions accepted. CoyoteDB runs PostgreSQL 18;
// pg_dump 18 reads every version in this range.
const (
	minSourceVersion = 120000
	maxSourceMajor   = 18
)

// source is what the preflight learned about the database being moved.
type source struct {
	params      connParams
	versionNum  int
	version     string
	database    string
	user        string
	size        int64
	encoding    string
	collate     string // the database's default text sort rule
	provider    string // "libc", "icu", "builtin" (PG 15+), else ""
	superuser   bool
	bypassRLS   bool
	host        string // "Supabase", "Amazon RDS", ... or ""
	schemas     []string
	extensions  []extension
	tables      []table // ordinary tables (relkind r), in the copied schemas
	unreadable  []string
	rlsBlocked  []string
	collations  []collationUse
	policies    []policy
	largeObjs   int64
	otherOwners []string
	live        liveInfo // read only for a live copy
}

// liveInfo is what a live copy (logical replication) needs from the source.
type liveInfo struct {
	walLevel    string
	replication bool     // the login may stream changes
	freeSlots   int      // replication slots still free
	noIdentity  []string // tables whose updates and deletes cannot be published
	notOwned    []string // tables the login does not own (a publication needs owners)
}

type extension struct {
	Name, Version, Schema string
}

type table struct {
	OID     uint32
	Schema  string
	Name    string
	Size    int64   // heap + TOAST, bytes
	Rows    float64 // planner estimate (-1: never analysed)
	Columns []column
	PK      []string // primary key columns, in order
	Owned   bool     // the source login owns it (or is a member of the owner)
	// PKPlain: every primary-key column has a type without a sort rule
	// (integers, uuid, dates...), so ORDER BY gives the same order on
	// both sides whatever the collation.
	PKPlain bool
}

type column struct {
	Name  string
	Money bool // money prints by the server's locale: compared as numeric
}

type collationUse struct {
	Name, Schema, Example string
}

// policy is a row-level security policy and the roles it is for (none: for
// everyone).
type policy struct {
	Schema, Table, Name string
	Roles               []string
}

// tocName is the policy's name in the dump's table of contents.
func (p policy) tocName() string { return p.Table + " " + p.Name }

func (t table) qualified() string {
	return pgx.Identifier{t.Schema, t.Name}.Sanitize()
}

func (t table) label() string { return t.Schema + "." + t.Name }

// systemSchema: schemas pg_dump never copies.
const userSchemaFilter = `n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'`

// inspectSource reads everything the preflight needs from the source.
// excluded lists schemas that will not be copied.
func inspectSource(ctx context.Context, c *pgx.Conn, p connParams) (*source, error) {
	s := &source{params: p}
	if err := c.QueryRow(ctx, "SELECT current_setting('server_version_num')::int, current_setting('server_version')").Scan(&s.versionNum, &s.version); err != nil {
		return nil, fmt.Errorf("read the server version: %w", err)
	}
	if v, _, ok := strings.Cut(s.version, " "); ok { // "16.4 (Ubuntu 16.4-1)"
		s.version = v
	}
	if s.versionNum < minSourceVersion || s.versionNum/10000 > maxSourceMajor {
		return s, nil // the preflight explains; nothing else is read
	}
	q := `SELECT current_database()::text, current_user::text, pg_database_size(current_database()),
	             pg_encoding_to_char(d.encoding)::text, d.datcollate::text, %s
	        FROM pg_database d WHERE d.datname = current_database()`
	prov, loc := "''", "''"
	if s.versionNum >= 150000 {
		prov = `CASE d.datlocprovider WHEN 'c' THEN 'libc' WHEN 'i' THEN 'icu' WHEN 'b' THEN 'builtin' ELSE '' END`
		loc = "coalesce(d.daticulocale, '')"
	}
	if s.versionNum >= 170000 {
		loc = "coalesce(d.datlocale, '')"
	}
	var locale string
	if err := c.QueryRow(ctx, fmt.Sprintf(q, prov+", "+loc)).Scan(&s.database, &s.user, &s.size, &s.encoding, &s.collate, &s.provider, &locale); err != nil {
		return nil, fmt.Errorf("read the database settings: %w", err)
	}
	if s.provider == "icu" && locale != "" {
		s.collate = locale // ICU decides the sort order, not datcollate
	}
	if err := c.QueryRow(ctx, "SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user").Scan(&s.superuser, &s.bypassRLS); err != nil {
		return nil, fmt.Errorf("read the login's rights: %w", err)
	}
	var err error
	if s.schemas, err = textColumn(ctx, c, `SELECT n.nspname::text FROM pg_namespace n WHERE `+userSchemaFilter+` ORDER BY 1`); err != nil {
		return nil, err
	}
	rows, err := c.Query(ctx, `SELECT e.extname::text, e.extversion, n.nspname::text
	    FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace
	   WHERE e.extname <> 'plpgsql' ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("list extensions: %w", err)
	}
	s.extensions, err = pgx.CollectRows(rows, pgx.RowToStructByPos[extension])
	if err != nil {
		return nil, fmt.Errorf("list extensions: %w", err)
	}
	roles, err := textColumn(ctx, c, `SELECT rolname::text FROM pg_roles WHERE rolname IN ('supabase_admin', 'rds_superuser', 'azure_pg_admin', 'cloudsqlsuperuser', 'neon_superuser')`)
	if err != nil {
		return nil, err
	}
	s.host = detectHost(s.schemas, roles)
	if err := c.QueryRow(ctx, "SELECT count(*) FROM pg_largeobject_metadata").Scan(&s.largeObjs); err != nil {
		return nil, fmt.Errorf("count large objects: %w", err)
	}
	return s, nil
}

// inspectObjects reads the relations in the schemas that will be copied:
// readability, row-level security, sizes, columns and keys.
func (s *source) inspectObjects(ctx context.Context, c *pgx.Conn, excluded []string) error {
	ex := excluded
	if ex == nil {
		ex = []string{}
	}
	type rel struct {
		OID          uint32
		Schema, Name string
		Kind         string
		Size         int64
		Rows         float64
		Readable     bool
		RLS, Force   bool
		Owner        bool
		OwnerName    string
	}
	rows, err := c.Query(ctx, `SELECT c.oid, n.nspname::text, c.relname::text, c.relkind::text,
	        CASE WHEN c.relkind = 'r' THEN pg_table_size(c.oid) ELSE 0 END,
	        c.reltuples::float8, has_table_privilege(c.oid, 'SELECT'),
	        c.relrowsecurity, c.relforcerowsecurity, pg_has_role(c.relowner, 'USAGE'),
	        pg_get_userbyid(c.relowner)::text
	   FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	  WHERE c.relkind IN ('r', 'p', 'v', 'm', 'S') AND `+userSchemaFilter+`
	    AND n.nspname <> ALL ($1::text[])
	    AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_class'::regclass
	                     AND d.objid = c.oid AND d.deptype = 'e')
	  ORDER BY 2, 3`, ex)
	if err != nil {
		return fmt.Errorf("list tables: %w", err)
	}
	rels, err := pgx.CollectRows(rows, pgx.RowToStructByPos[rel])
	if err != nil {
		return fmt.Errorf("list tables: %w", err)
	}
	byOID := map[uint32]int{}
	owners := map[string]bool{}
	for _, r := range rels {
		label := r.Schema + "." + r.Name
		owners[r.OwnerName] = true
		if !r.Readable {
			s.unreadable = append(s.unreadable, label)
			continue
		}
		// pg_dump turns row-level security off and fails rather than copy
		// part of a table.
		if (r.Kind == "r" || r.Kind == "p") && r.RLS && !s.superuser && !s.bypassRLS && (!r.Owner || r.Force) {
			s.rlsBlocked = append(s.rlsBlocked, label)
		}
		if r.Kind == "r" {
			byOID[r.OID] = len(s.tables)
			s.tables = append(s.tables, table{OID: r.OID, Schema: r.Schema, Name: r.Name, Size: r.Size, Rows: r.Rows, Owned: r.Owner})
		}
	}
	delete(owners, s.user)
	for o := range owners {
		s.otherOwners = append(s.otherOwners, o)
	}
	slices.Sort(s.otherOwners)
	if len(s.tables) > 0 {
		oids := make([]uint32, 0, len(s.tables))
		for _, t := range s.tables {
			oids = append(oids, t.OID)
		}
		rows, err := c.Query(ctx, `SELECT a.attrelid, a.attname::text, a.atttypid = 'money'::regtype
		   FROM pg_attribute a WHERE a.attrelid = ANY ($1::oid[]) AND a.attnum > 0 AND NOT a.attisdropped
		  ORDER BY a.attrelid, a.attnum`, oids)
		if err != nil {
			return fmt.Errorf("list columns: %w", err)
		}
		var oid uint32
		var col column
		_, err = pgx.ForEachRow(rows, []any{&oid, &col.Name, &col.Money}, func() error {
			if i, ok := byOID[oid]; ok {
				s.tables[i].Columns = append(s.tables[i].Columns, col)
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("list columns: %w", err)
		}
		rows, err = c.Query(ctx, `SELECT i.indrelid, array_agg(a.attname::text ORDER BY k.ord), bool_and(t.typcollation = 0)
		   FROM pg_index i
		   CROSS JOIN LATERAL unnest(i.indkey::int2[]) WITH ORDINALITY AS k(attnum, ord)
		   JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
		   JOIN pg_type t ON t.oid = a.atttypid
		  WHERE i.indisprimary AND i.indrelid = ANY ($1::oid[])
		  GROUP BY i.indrelid`, oids)
		if err != nil {
			return fmt.Errorf("list primary keys: %w", err)
		}
		var pk []string
		var plain bool
		_, err = pgx.ForEachRow(rows, []any{&oid, &pk, &plain}, func() error {
			if i, ok := byOID[oid]; ok {
				s.tables[i].PK = slices.Clone(pk)
				s.tables[i].PKPlain = plain
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("list primary keys: %w", err)
		}
	}
	rows, err = c.Query(ctx, `SELECT DISTINCT ON (co.collname, cn.nspname) co.collname::text, cn.nspname::text,
	        n.nspname || '.' || c.relname || '.' || a.attname
	   FROM pg_attribute a
	   JOIN pg_class c ON c.oid = a.attrelid
	   JOIN pg_namespace n ON n.oid = c.relnamespace
	   JOIN pg_collation co ON co.oid = a.attcollation
	   JOIN pg_namespace cn ON cn.oid = co.collnamespace
	  WHERE a.attnum > 0 AND NOT a.attisdropped AND a.attcollation <> 0
	    AND co.collname NOT IN ('default', 'C', 'POSIX')
	    AND c.relkind IN ('r', 'p', 'v', 'm', 'c', 'f') AND `+userSchemaFilter+`
	    AND n.nspname <> ALL ($1::text[])
	  ORDER BY co.collname, cn.nspname, 3`, ex)
	if err != nil {
		return fmt.Errorf("list sort rules: %w", err)
	}
	s.collations, err = pgx.CollectRows(rows, pgx.RowToStructByPos[collationUse])
	if err != nil {
		return fmt.Errorf("list sort rules: %w", err)
	}
	rows, err = c.Query(ctx, `SELECT n.nspname::text, c.relname::text, p.polname::text,
	        coalesce((SELECT array_agg(r.rolname::text ORDER BY r.rolname) FROM pg_roles r WHERE r.oid = ANY (p.polroles)), '{}')
	   FROM pg_policy p JOIN pg_class c ON c.oid = p.polrelid JOIN pg_namespace n ON n.oid = c.relnamespace
	  WHERE `+userSchemaFilter+` AND n.nspname <> ALL ($1::text[])
	  ORDER BY 1, 2, 3`, ex)
	if err != nil {
		return fmt.Errorf("list row-level security policies: %w", err)
	}
	s.policies, err = pgx.CollectRows(rows, pgx.RowToStructByPos[policy])
	if err != nil {
		return fmt.Errorf("list row-level security policies: %w", err)
	}
	return nil
}

func textColumn(ctx context.Context, c *pgx.Conn, q string, args ...any) ([]string, error) {
	rows, err := c.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// detectHost names well-known hosted Postgres services from the schemas
// and roles they always create.
func detectHost(schemas, roles []string) string {
	has := func(xs []string, x string) bool { return slices.Contains(xs, x) }
	switch {
	case has(roles, "supabase_admin") && has(schemas, "auth") && has(schemas, "storage"):
		return "Supabase"
	case has(roles, "rds_superuser"):
		return "Amazon RDS"
	case has(roles, "azure_pg_admin"):
		return "Azure Database for PostgreSQL"
	case has(roles, "cloudsqlsuperuser"):
		return "Google Cloud SQL"
	case has(roles, "neon_superuser"):
		return "Neon"
	}
	return ""
}

// hostSchemas are schemas a hosted service creates for its own machinery.
// They are left out when that service is detected: they hold no data of
// the app's, and they depend on roles, extensions and background workers
// that exist only there.
var hostSchemas = map[string][]string{
	// auth (the app's users) and storage (its file records) are the app's
	// data and are copied; tables of the app often point into auth.users.
	"Supabase": {"realtime", "_realtime", "supabase_functions", "supabase_migrations",
		"graphql", "graphql_public", "vault", "pgsodium", "pgsodium_masks", "net", "_analytics", "pgbouncer"},
}

// leftOutExtensions are never copied, with the reason in plain words. They
// hold no data of the app's: statistics views, or plumbing of the host.
var leftOutExtensions = map[string]string{
	"pg_stat_statements": "query statistics only, holds none of your data",
	"pg_stat_monitor":    "query statistics only, holds none of your data",
	"pg_buffercache":     "a diagnostic view, holds none of your data",
	"pg_qualstats":       "query statistics only, holds none of your data",
	"pg_stat_kcache":     "query statistics only, holds none of your data",
	"pg_wait_sampling":   "query statistics only, holds none of your data",
	"hypopg":             "an index-planning aid, holds none of your data",
	"index_advisor":      "an index-planning aid, holds none of your data",
	"pg_graphql":         "part of Supabase's own API layer",
	"pgsodium":           "part of Supabase's own key store",
	"supabase_vault":     "part of Supabase's own secret store",
	"pg_net":             "part of Supabase's web-request feature",
	"supautils":          "part of Supabase's own management",
	"aws_commons":        "part of Amazon RDS",
	"aws_s3":             "part of Amazon RDS",
	"aws_lambda":         "part of Amazon RDS",
	"rds_tools":          "part of Amazon RDS",
	"neon":               "part of Neon",
	"neon_utils":         "part of Neon",
	"google_insights":    "part of Google Cloud SQL",
	"pgaudit":            "audit logging settings of the old server",
	"amcheck":            "CoyoteDB installs its own copy",
}

// inspectLive reads what logical replication needs: wal_level, the right to
// stream, a free slot, and for every table a replica identity (otherwise
// the source would refuse the app's UPDATE and DELETE once the table is
// published) and an owner the login can act for.
func (s *source) inspectLive(ctx context.Context, c *pgx.Conn) error {
	if err := c.QueryRow(ctx, "SELECT current_setting('wal_level')").Scan(&s.live.walLevel); err != nil {
		return fmt.Errorf("read wal_level: %w", err)
	}
	if err := c.QueryRow(ctx, `SELECT r.rolsuper OR r.rolreplication
	        OR EXISTS (SELECT 1 FROM pg_roles x WHERE x.rolname = 'rds_replication' AND pg_has_role(r.oid, x.oid, 'member'))
	   FROM pg_roles r WHERE r.rolname = current_user`).Scan(&s.live.replication); err != nil {
		return fmt.Errorf("read the login's replication right: %w", err)
	}
	if err := c.QueryRow(ctx, `SELECT current_setting('max_replication_slots')::int - (SELECT count(*) FROM pg_replication_slots)`).Scan(&s.live.freeSlots); err != nil {
		return fmt.Errorf("count replication slots: %w", err)
	}
	if len(s.tables) == 0 {
		return nil
	}
	oids := make([]uint32, 0, len(s.tables))
	for _, t := range s.tables {
		oids = append(oids, t.OID)
		if !t.Owned && !s.superuser {
			s.live.notOwned = append(s.live.notOwned, t.label())
		}
	}
	var err error
	s.live.noIdentity, err = textColumn(ctx, c, `SELECT n.nspname || '.' || c.relname
	   FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	  WHERE c.oid = ANY ($1::oid[])
	    AND (c.relreplident = 'n'
	     OR (c.relreplident = 'd' AND NOT EXISTS (SELECT 1 FROM pg_index i WHERE i.indrelid = c.oid AND i.indisprimary))
	     OR (c.relreplident = 'i' AND NOT EXISTS (SELECT 1 FROM pg_index i WHERE i.indrelid = c.oid AND i.indisreplident)))
	  ORDER BY 1`, oids)
	if err != nil {
		return fmt.Errorf("check replica identities: %w", err)
	}
	return nil
}
