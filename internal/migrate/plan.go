package migrate

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"

	"coyotedb.com/coyote/internal/pg"
)

// plan is the preflight's verdict: what is copied, what is left out, and
// what stops the move. Pure: built from what was read, so it is unit-tested.
type plan struct {
	excludedSchemas []string    // pg_dump --exclude-schema
	skipExtensions  []string    // pg_dump --exclude-extension
	adminExtensions []extension // created by the administrator before the restore
	summary         [][2]string // label, text
	leftOut         []string
	// dropPolicies: policies (by their table-of-contents name) for roles
	// the cluster does not have; they could not be created.
	dropPolicies map[string]bool
	notes        []string
	blockers     []string
	replacing    bool // the target holds something and --replace was given
}

// planInput is everything the verdict depends on.
type planInput struct {
	src        *source
	tgt        *targetInfo
	database   string
	mainName   string
	replace    bool
	userSkip   []string // --skip-extension
	userExcl   []string // --exclude-schema
	adminReady bool     // this machine is the main node and has the administrator login
	freeBytes  int64    // free space where the dump goes (-1: unknown)
	workDir    string
	live       bool // a live copy (logical replication) is being started
}

// excludedSchemas: the user's --exclude-schema plus the hosted service's
// own schemas, limited to those that exist. Unknown names are reported.
func excludedSchemas(src *source, user []string) (out []string, unknown []string) {
	add := func(s string) {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	for _, s := range user {
		if slices.Contains(src.schemas, s) {
			add(s)
		} else {
			unknown = append(unknown, s)
		}
	}
	for _, s := range hostSchemas[src.host] {
		if slices.Contains(src.schemas, s) {
			add(s)
		}
	}
	return out, unknown
}

func buildPlan(in planInput) plan {
	var p plan
	src, tgt := in.src, in.tgt
	block := func(f string, a ...any) { p.blockers = append(p.blockers, fmt.Sprintf(f, a...)) }
	note := func(f string, a ...any) { p.notes = append(p.notes, fmt.Sprintf(f, a...)) }
	row := func(k, f string, a ...any) { p.summary = append(p.summary, [2]string{k, fmt.Sprintf(f, a...)}) }

	where := "PostgreSQL " + src.version + " at " + src.params.describe()
	if src.host != "" {
		where += " (" + src.host + ")"
	}
	row("Source", "%s", where)
	switch {
	case src.versionNum < minSourceVersion:
		block("The source runs PostgreSQL %s. CoyoteDB moves databases from PostgreSQL 12 or newer: upgrade the source first (most hosts have a button for it).", src.version)
		return p
	case src.versionNum/10000 > maxSourceMajor:
		block("The source runs PostgreSQL %s, which is newer than CoyoteDB's PostgreSQL %d. A newer database cannot be moved into an older one.", src.version, maxSourceMajor)
		return p
	}
	var excl, unknown []string
	excl, unknown = excludedSchemas(src, in.userExcl)
	p.excludedSchemas = excl
	for _, s := range unknown {
		note("There is no schema %q on the source; --exclude-schema %s did nothing.", s, s)
	}

	// Size.
	rows := 0.0
	for _, t := range src.tables {
		if t.Rows > 0 {
			rows += t.Rows
		}
	}
	sz := fmt.Sprintf("%s, %d %s", humanBytes(src.size), len(src.tables), plural(len(src.tables), "table", "tables"))
	if rows > 0 {
		sz += ", about " + humanCount(rows) + " rows"
	}
	if src.largeObjs > 0 {
		sz += fmt.Sprintf(", %d large %s", src.largeObjs, plural(int(src.largeObjs), "object", "objects"))
	}
	row("Size", "%s", sz)

	// Text: encoding and sort order.
	text := src.encoding
	switch src.encoding {
	case "UTF8":
	case "SQL_ASCII":
		note("The source stores text without a declared encoding (SQL_ASCII). CoyoteDB stores UTF-8: any text that is not valid UTF-8 stops the copy with an error naming the table.")
	default:
		text += " (converted to UTF-8)"
	}
	if !plainSortRule(src.collate, src.provider) {
		text += "; sorted by " + src.collate + " on the source"
		note("Text on the source sorts by %s. CoyoteDB sorts text by character code (C.UTF-8) on every node, so copies on different computers stay identical. Your data is copied exactly; ORDER BY on text can return a different order (for example capital letters first).", src.collate)
	}
	row("Text", "%s", text)

	// Extensions.
	skip := map[string]bool{}
	for _, e := range in.userSkip {
		skip[e] = true
	}
	var ready, admin []string
	for _, e := range src.extensions {
		if why, ok := leftOutExtensions[e.Name]; ok {
			p.skipExtensions = append(p.skipExtensions, e.Name)
			if e.Name != "amcheck" {
				p.leftOut = append(p.leftOut, fmt.Sprintf("extension %s (%s)", e.Name, why))
			}
			continue
		}
		if skip[e.Name] {
			p.skipExtensions = append(p.skipExtensions, e.Name)
			p.leftOut = append(p.leftOut, fmt.Sprintf("extension %s (--skip-extension; anything that uses it will fail to copy)", e.Name))
			continue
		}
		if slices.Contains(excl, e.Schema) {
			block("Extension %s lives in schema %s, which is left out. Leave the extension out too with --skip-extension %s, or keep the schema.", e.Name, e.Schema, e.Name)
			continue
		}
		av, ok := tgt.available[e.Name]
		switch {
		case !ok && slices.Contains(pg.PinnedExtensionNames(), e.Name):
			block("Extension %s (version %s) comes with CoyoteDB but is not installed on the cluster's computers yet, so tables and functions that use it cannot be moved. Every database computer needs it: start each node with %s set to where the extension files are. If your app does not use it, leave it out with --skip-extension %s.", e.Name, e.Version, pg.ExtensionsFromEnv, e.Name)
		case !ok:
			block("Extension %s (version %s) is not part of CoyoteDB's PostgreSQL build, so tables and functions that use it cannot be moved. If your app does not use it, leave it out with --skip-extension %s.", e.Name, e.Version, e.Name)
		case av.Superuser && !av.Trusted:
			if in.adminReady {
				p.adminExtensions = append(p.adminExtensions, e)
				admin = append(admin, e.Name)
			} else {
				main := in.mainName
				if main == "" {
					main = "the main node"
				}
				block("Extension %s can only be created by the database administrator, who logs in only on the main node's own computer. Run this command on %s, or leave the extension out with --skip-extension %s.", e.Name, main, e.Name)
			}
		default:
			ready = append(ready, e.Name)
		}
	}
	for e := range skip {
		if !slices.ContainsFunc(src.extensions, func(x extension) bool { return x.Name == e }) {
			note("The source has no extension %q; --skip-extension %s did nothing.", e, e)
		}
	}
	switch {
	case len(ready) == 0 && len(admin) == 0:
		row("Extensions", "none needed")
	default:
		s := strings.Join(append(slices.Clone(ready), admin...), ", ")
		if len(admin) > 0 {
			s += " (" + strings.Join(admin, ", ") + " created by the administrator on this computer)"
		}
		row("Extensions", "%s", s)
	}

	// Schemas left out.
	if len(excl) > 0 {
		why := "--exclude-schema"
		if src.host != "" && len(hostSchemas[src.host]) > 0 {
			why = src.host + "'s own machinery (realtime, functions, API, key store): it holds none of your data"
			if len(in.userExcl) > 0 {
				why += ", and --exclude-schema"
			}
		}
		p.leftOut = append(p.leftOut, fmt.Sprintf("%s %s (%s)", plural(len(excl), "schema", "schemas"), strings.Join(excl, ", "), why))
	}

	if src.host == "Supabase" && (slices.Contains(src.schemas, "auth") || slices.Contains(src.schemas, "storage")) {
		note("Supabase: your users (schema auth) and file records (schema storage) are copied as plain tables. The files themselves stay in Supabase Storage, and signing in needs a sign-in service of your own: Supabase's does not come along.")
	}

	// Policies for roles that exist only on the source (Supabase's anon and
	// authenticated, for example). On the cluster the app logs in as the
	// tables' owner, whom policies do not limit; a table whose policies are
	// all left out still lets nobody else in (row-level security stays on).
	missing := map[string]bool{}
	var dropped []string
	for _, pol := range src.policies {
		for _, r := range pol.Roles {
			if r != "public" && !tgt.roles[r] {
				missing[r] = true
				if p.dropPolicies == nil {
					p.dropPolicies = map[string]bool{}
				}
				if !p.dropPolicies[pol.Schema+"\x00"+pol.tocName()] {
					p.dropPolicies[pol.Schema+"\x00"+pol.tocName()] = true
					dropped = append(dropped, fmt.Sprintf("%q on %s.%s", pol.Name, pol.Schema, pol.Table))
				}
			}
		}
	}
	if len(dropped) > 0 {
		roles := slices.Sorted(maps.Keys(missing))
		p.leftOut = append(p.leftOut, fmt.Sprintf("row-level security %s %s (for %s %s, which %s only on the source; row-level security itself stays on, and your app logs in as the tables' owner)",
			plural(len(dropped), "policy", "policies"), listSome(dropped, 4), plural(len(roles), "role", "roles"), strings.Join(roles, ", "), plural(len(roles), "exists", "exist")))
	}

	// Access on the source.
	if len(src.unreadable) > 0 {
		block("The source login (%s) cannot read %s. Use the owner's login, or one with read access to everything (on most hosts: the main \"postgres\" user).", src.user, listSome(src.unreadable, 5))
	}
	if len(src.rlsBlocked) > 0 {
		block("Row-level security hides rows of %s from the source login (%s), so a full copy is impossible with it. Use the tables' owner or a login with BYPASSRLS (on Supabase: the \"postgres\" user on the direct connection).", listSome(src.rlsBlocked, 5), src.user)
	}
	// Sort rules used by columns.
	for _, c := range src.collations {
		if c.Schema == "pg_catalog" && !tgt.collations[c.Name] {
			block("Column %s uses the sort rule %q, which CoyoteDB does not keep (it sorts text one way on every node). Change such columns to the default rule on the source first, for example: ALTER TABLE ... ALTER COLUMN ... TYPE text COLLATE \"default\".", c.Example, c.Name)
		}
	}
	if len(src.otherOwners) > 0 {
		row("Owner", "everything will belong to the app role (on the source: %s)", strings.Join(append([]string{src.user}, src.otherOwners...), ", "))
	} else {
		row("Owner", "everything will belong to the app role")
	}

	// Target.
	into := "database " + in.database + " on the cluster"
	if in.mainName != "" {
		into += ", main node " + in.mainName
	}
	switch {
	case tgt.exists && tgt.owner != tgt.user:
		block("Database %s on the cluster belongs to %s, not to the app role, so it cannot be replaced by this command. Pick another name with --database.", in.database, tgt.owner)
	case len(tgt.contents) > 0:
		what := strings.Join(tgt.contents, ", ")
		if tgt.more > 0 {
			what += fmt.Sprintf(" and %d more", tgt.more)
		}
		if in.replace {
			p.replacing = true
			into += " (its current contents will be deleted: " + what + ")"
		} else {
			block("Database %s on the cluster is not empty (%s). Nothing was copied. To replace its contents with the copy, run the command again with --replace.", in.database, what)
		}
	case tgt.exists:
		into += " (empty)"
	default:
		into += " (will be created)"
	}
	row("Into", "%s", into)
	if len(in.database) > maxDBName {
		block("The database name %q is too long (at most %d characters).", in.database, maxDBName)
	}

	if tgt.staging && strings.HasPrefix(tgt.stagingNote, liveComment) {
		block("A live copy into database %s is running (see `coyote migrate --status`). Finish it with `coyote migrate --cutover`, or stop it with `coyote migrate --cancel`.", in.database)
	}
	if tgt.staging && isCutoverNote(tgt.stagingNote) {
		block("A cutover into database %s stopped partway, after the copy was checked. Finish it with `coyote migrate --cutover`, or stop it with `coyote migrate --cancel`.", in.database)
	}
	if in.live {
		liveChecks(in, block, note)
	}
	if in.freeBytes >= 0 && src.size > 0 && in.freeBytes < src.size/2 {
		note("The copy is staged in %s, which has %s free; the source is %s. A compressed copy is usually smaller, but if it runs out of room, point --work-dir at a folder with more space.", in.workDir, humanBytes(in.freeBytes), humanBytes(src.size))
	}
	return p
}

// plainSortRule: the source already sorts text by character code.
func plainSortRule(collate, provider string) bool {
	if provider == "builtin" {
		return true
	}
	switch strings.ToLower(collate) {
	case "c", "posix", "c.utf8", "c.utf-8", "ucs_basic":
		return true
	}
	return false
}

func listSome(xs []string, n int) string {
	if len(xs) <= n {
		return strings.Join(xs, ", ")
	}
	return strings.Join(xs[:n], ", ") + fmt.Sprintf(" and %d more", len(xs)-n)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d bytes", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func humanCount(f float64) string {
	switch {
	case f >= 1e9:
		return fmt.Sprintf("%.1f billion", f/1e9)
	case f >= 1e6:
		return fmt.Sprintf("%.1f million", f/1e6)
	case f >= 1e4:
		return fmt.Sprintf("%d thousand", int64(math.Round(f/1e3)))
	}
	return fmt.Sprintf("%d", int64(math.Round(f)))
}

// liveChecks: what a live copy needs beyond a one-time copy.
func liveChecks(in planInput, block, note func(string, ...any)) {
	src, tgt := in.src, in.tgt
	l := src.live
	if l.walLevel != "logical" {
		block("Live copying needs the source to keep logical change records (wal_level = logical; it is %s). On Amazon RDS set rds.logical_replication = 1 in the parameter group; on Neon turn on logical replication in the project settings; on your own server set wal_level = logical and restart it. Or use the normal move (without --live).", l.walLevel)
	}
	if !l.replication {
		block("The source login (%s) may not stream changes. Use a login with the REPLICATION right (on most hosts: the main \"postgres\" user), or the normal move.", src.user)
	}
	if l.freeSlots < 1 {
		block("The source has no free replication slot (max_replication_slots is used up). Free one, or raise max_replication_slots.")
	}
	if len(l.noIdentity) > 0 {
		block("%s %s no primary key. Once published, the source would refuse your app's updates and deletes on them. Add a primary key (or ALTER TABLE ... REPLICA IDENTITY FULL), or use the normal move.", plural(len(l.noIdentity), "Table", "Tables"), listSome(l.noIdentity, 5)+plural(len(l.noIdentity), " has", " have"))
	}
	if len(l.notOwned) > 0 {
		block("The source login (%s) does not own %s, and only an owner can publish a table's changes. Use the owner's login.", src.user, listSome(l.notOwned, 5))
	}
	if src.largeObjs > 0 {
		block("The source has %d large objects, which live copying cannot carry. Use the normal move.", src.largeObjs)
	}
	if len(src.tables) == 0 {
		block("The source has no tables to copy live.")
	}
	if !in.adminReady {
		main := in.mainName
		if main == "" {
			main = "the main node"
		}
		block("A live copy is set up and finished by the database administrator, who logs in only on the main node's own computer. Run this command on %s.", main)
	}
	note("Live copy: changes keep flowing from the source until you run `coyote migrate --cutover`. Changes to tables' definitions (ALTER TABLE, new tables) are not copied: make them on both sides, or not at all until the cutover. Sequences are copied at the cutover.")
	_ = tgt
}
