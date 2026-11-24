//go:build integration && migratedocker && unix

package integration

// Moves from the official postgres Docker images, versions 12 to 17, and
// from a Supabase-like database. Needs Docker; run on the Linux test box
// (docs/TESTING.md):
//
//	go test -tags=integration,migratedocker -count=1 -run M8Docker ./test/integration/
//
// Containers are named coyote-test-migrate-* and removed at the end.

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type dockerPG struct {
	t     *testing.T
	name  string
	image string
	port  int
}

// startDocker runs image as container coyote-test-migrate-<tag>, with
// logical change records on, reachable on 127.0.0.1 only.
func startDocker(t *testing.T, image, tag string) *dockerPG {
	t.Helper()
	d := &dockerPG{t: t, name: "coyote-test-migrate-" + tag, image: image, port: freePort(t)}
	_ = exec.Command("docker", "rm", "-f", d.name).Run()
	out, err := exec.Command("docker", "run", "-d", "--name", d.name,
		"-e", "POSTGRES_PASSWORD="+srcPassword, "-p", fmt.Sprintf("127.0.0.1:%d:5432", d.port),
		image, "-c", "wal_level=logical", "-c", "max_replication_slots=10", "-c", "fsync=off").CombinedOutput()
	if err != nil {
		t.Fatalf("docker run %s: %v\n%s", image, err, out)
	}
	t.Cleanup(func() {
		if out, err := exec.Command("docker", "rm", "-f", "-v", d.name).CombinedOutput(); err != nil {
			t.Logf("docker rm %s: %v\n%s", d.name, err, out)
		}
	})
	// The image starts a temporary server for its setup first: wait for the
	// setup to finish, then for the real server.
	waitFor(t, 3*time.Minute, image+" to start", func() bool {
		logs, _ := exec.Command("docker", "logs", d.name).CombinedOutput()
		if !strings.Contains(string(logs), "PostgreSQL init process complete") {
			return false
		}
		c, err := tryConnect(d.url("postgres"))
		if err != nil {
			return false
		}
		_ = c.Close(context.Background())
		return true
	})
	return d
}

func (d *dockerPG) url(db string) string {
	return fmt.Sprintf("postgresql://postgres:%s@127.0.0.1:%d/%s?sslmode=disable", srcPassword, d.port, db)
}

func (d *dockerPG) conn(db string) *pgx.Conn {
	d.t.Helper()
	c, err := tryConnect(d.url(db))
	if err != nil {
		d.t.Fatal(err)
	}
	d.t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

func (d *dockerPG) exec(db string, qs ...string) {
	d.t.Helper()
	c := d.conn(db)
	for _, q := range qs {
		mustExec(d.t, c, q)
	}
}

func dockerCluster(t *testing.T) (a, b, w *cnode) {
	t.Helper()
	root := t.TempDir()
	a, b = newCNode(t, root, "node-a"), newCNode(t, root, "node-b")
	w = newWitness(t, root, "wit")
	t.Cleanup(func() { stopAll(a, b, w) })
	a.bootstrap()
	b.join(a)
	w.join(a)
	syncFull(t, a, b)
	a.connInfo()
	return a, b, w
}

func shopFingerprints(t *testing.T, c *pgx.Conn, extra ...string) map[string]string {
	t.Helper()
	want := map[string]string{}
	for _, tbl := range append(extra, shopTables...) {
		want[tbl] = fingerprint(t, c, tbl)
	}
	return want
}

// Every source version from 12 to 17: a one-time move into database vNN,
// and a live copy (with the app writing during it) into database lNN,
// both checked on the copy node against the source.
func TestM8DockerVersions(t *testing.T) {
	a, b, _ := dockerCluster(t)
	for _, v := range []int{12, 13, 14, 15, 16, 17} {
		t.Run(fmt.Sprintf("pg%d", v), func(t *testing.T) {
			d := startDocker(t, fmt.Sprintf("postgres:%d", v), fmt.Sprintf("pg%d", v))
			d.exec("postgres", "CREATE DATABASE shop")
			d.exec("shop", shopSQL(50)...)
			d.exec("shop", `ALTER TABLE events_2025 REPLICA IDENTITY FULL`, `ALTER TABLE events_2026 REPLICA IDENTITY FULL`,
				`CREATE TABLE ticks (id bigserial PRIMARY KEY, n int NOT NULL)`)
			sc := d.conn("shop")

			// One-time move.
			db := fmt.Sprintf("v%d", v)
			out, err := migrateCmd(t, a, "--database", db, d.url("shop"))
			if err != nil {
				t.Fatalf("migrate: %v\n%s", err, out)
			}
			t.Logf("PostgreSQL %d, one-time move:\n%s", v, out)
			if !strings.Contains(out, fmt.Sprintf("PostgreSQL %d.", v)) || !strings.Contains(out, "6 tables match the source") {
				t.Fatalf("output:\n%s", out)
			}
			sameAsSourceIn(t, b, db, shopFingerprints(t, sc, "ticks"))

			// Live copy while the app writes, then the cutover.
			ldb := fmt.Sprintf("l%d", v)
			out, err = migrateCmd(t, a, "--live", "--database", ldb, d.url("shop"))
			if err != nil {
				t.Fatalf("migrate --live: %v\n%s", err, out)
			}
			for i := range 200 {
				mustExec(t, sc, `INSERT INTO ticks (n) VALUES ($1)`, i)
				if i%20 == 0 {
					mustExec(t, sc, `UPDATE customers SET name = name || '+' WHERE id = $1`, 1+i)
					mustExec(t, sc, `DELETE FROM orders WHERE id = $1`, 1+i)
				}
			}
			out, err = migrateCmd(t, a, "--cutover", "--yes", "--database", ldb)
			if err != nil {
				t.Fatalf("migrate --cutover: %v\n%s", err, out)
			}
			t.Logf("PostgreSQL %d, live copy cutover:\n%s", v, out)
			if !strings.Contains(out, "6 tables match the source") {
				t.Fatalf("output:\n%s", out)
			}
			sameAsSourceIn(t, b, ldb, shopFingerprints(t, sc, "ticks"))
			app, err := tryConnect(appURLFrom(t, out))
			if err != nil {
				t.Fatal(err)
			}
			defer app.Close(context.Background())
			if id, top := queryInt(t, app, `INSERT INTO ticks (n) VALUES (0) RETURNING id`), queryInt(t, sc, `SELECT max(id) FROM ticks`); id <= top {
				t.Fatalf("ticks id %d not past the source's %d", id, top)
			}
			if n := queryInt(t, sc, `SELECT count(*) FROM pg_replication_slots`); n != 0 {
				t.Fatalf("%d replication slots left on the source", n)
			}

			// --cancel removes a live copy from both sides.
			cdb := fmt.Sprintf("c%d", v)
			if out, err := migrateCmd(t, a, "--live", "--database", cdb, d.url("shop")); err != nil {
				t.Fatalf("migrate --live: %v\n%s", err, out)
			}
			out, err = migrateCmd(t, a, "--cancel", "--database", cdb)
			if err != nil || !strings.Contains(out, "stopped and removed") {
				t.Fatalf("--cancel: %v\n%s", err, out)
			}
			if n := queryInt(t, sc, `SELECT count(*) FROM pg_replication_slots`) + queryInt(t, sc, `SELECT count(*) FROM pg_publication`); n != 0 {
				t.Fatalf("%d slots or publications left on the source after --cancel", n)
			}
			if n := queryInt(t, app, `SELECT count(*) FROM pg_database WHERE datname LIKE 'c%'`); n != 0 {
				t.Fatalf("%d databases left after --cancel", n)
			}
		})
	}
}

// supabaseSQL is a small Supabase-shaped database: its roles (which do not
// exist on CoyoteDB), its auth and storage schemas, and an app table with
// row-level security policies for those roles and a key into auth.users.
var supabaseSQL = []string{
	`CREATE ROLE anon NOLOGIN`, `CREATE ROLE authenticated NOLOGIN`, `CREATE ROLE service_role NOLOGIN BYPASSRLS`,
	`CREATE ROLE supabase_admin NOLOGIN`, `CREATE ROLE supabase_auth_admin NOLOGIN`, `CREATE ROLE supabase_storage_admin NOLOGIN`,
	`CREATE EXTENSION pgcrypto`,
	`CREATE SCHEMA auth AUTHORIZATION supabase_auth_admin`,
	`CREATE TABLE auth.users (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), email text UNIQUE, encrypted_password text, created_at timestamptz DEFAULT now())`,
	`CREATE FUNCTION auth.uid() RETURNS uuid LANGUAGE sql STABLE AS
	   $$SELECT nullif(current_setting('request.jwt.claim.sub', true), '')::uuid$$`,
	`CREATE SCHEMA storage AUTHORIZATION supabase_storage_admin`,
	`CREATE TABLE storage.buckets (id text PRIMARY KEY, name text NOT NULL, public bool DEFAULT false)`,
	`CREATE TABLE storage.objects (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), bucket_id text REFERENCES storage.buckets,
	   name text, owner uuid REFERENCES auth.users, metadata jsonb)`,
	`CREATE TABLE public.profiles (id uuid PRIMARY KEY REFERENCES auth.users ON DELETE CASCADE,
	   username text UNIQUE, avatar_url text, updated_at timestamptz DEFAULT now())`,
	`CREATE TABLE public.notes (id bigint GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
	   owner uuid NOT NULL DEFAULT auth.uid(), body text NOT NULL)`,
	`ALTER TABLE public.profiles ENABLE ROW LEVEL SECURITY`,
	`ALTER TABLE public.notes ENABLE ROW LEVEL SECURITY`,
	`CREATE POLICY "profiles are public" ON public.profiles FOR SELECT USING (true)`,
	`CREATE POLICY "own profile" ON public.profiles FOR UPDATE TO authenticated USING (auth.uid() = id)`,
	`CREATE POLICY "own notes" ON public.notes FOR ALL TO authenticated USING (owner = auth.uid()) WITH CHECK (owner = auth.uid())`,
	`GRANT USAGE ON SCHEMA public TO anon, authenticated, service_role`,
	`GRANT ALL ON ALL TABLES IN SCHEMA public TO anon, authenticated, service_role`,
	`INSERT INTO auth.users (email) SELECT 'user' || g || '@example.com' FROM generate_series(1, 300) g`,
	`INSERT INTO public.profiles (id, username) SELECT id, split_part(email, '@', 1) FROM auth.users`,
	`INSERT INTO public.notes (owner, body) SELECT id, 'note for ' || email FROM auth.users, generate_series(1, 3)`,
	`INSERT INTO storage.buckets VALUES ('avatars', 'avatars', true)`,
	`INSERT INTO storage.objects (bucket_id, name, owner) SELECT 'avatars', id || '.png', id FROM auth.users`,
}

// A Supabase-shaped source: Supabase's own machinery is left out, the
// app's users (auth) and file records (storage) come across with the app's
// tables, and the policies for Supabase's roles, which the cluster does not
// have, are left out up front and listed (row-level security stays on).
func TestM8DockerSupabase(t *testing.T) {
	a, b, _ := dockerCluster(t)
	d := startDocker(t, "postgres:15", "supabase")
	d.exec("postgres", supabaseSQL...)
	d.exec("postgres", `CREATE SCHEMA realtime`, `CREATE TABLE realtime.subscription (id bigint PRIMARY KEY, entity regclass)`)
	sc := d.conn("postgres")

	out, err := migrateCmd(t, a, "--dry-run", d.url("postgres"))
	t.Logf("dry run:\n%s", out)
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	for _, want := range []string{"(Supabase)", "schema realtime", "Supabase Storage",
		`policies "own notes" on public.notes, "own profile" on public.profiles (for role authenticated`} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry run does not mention %q:\n%s", want, out)
		}
	}

	out, err = migrateCmd(t, a, d.url("postgres"))
	t.Logf("move:\n%s", out)
	if err != nil {
		t.Fatalf("move: %v\n%s", err, out)
	}
	if !strings.Contains(out, "5 tables match the source") || strings.Contains(out, "could not be created") {
		t.Fatalf("output:\n%s", out)
	}
	want := map[string]string{}
	for _, tbl := range []string{"auth.users", "storage.buckets", "storage.objects", "public.profiles", "public.notes"} {
		want[tbl] = fingerprint(t, sc, tbl)
	}
	sameAsSource(t, b, want)
	app := nodeDB(t, b, "app")
	if n := queryInt(t, app, `SELECT count(*) FROM pg_namespace WHERE nspname = 'realtime'`); n != 0 {
		t.Fatal("Supabase's realtime schema came across")
	}
	if n := queryInt(t, app, `SELECT count(*) FROM pg_class WHERE relname IN ('profiles', 'notes') AND relrowsecurity`); n != 2 {
		t.Fatalf("row-level security on %d of 2 tables", n)
	}
	if n := queryInt(t, app, `SELECT count(*) FROM pg_policies`); n != 1 {
		t.Fatalf("%d policies; want only the one for everyone", n)
	}
	// The app's tables still point at its users, and auth.uid() works.
	if n := queryInt(t, app, `SELECT count(*) FROM pg_constraint WHERE conname = 'profiles_id_fkey'`); n != 1 {
		t.Fatal("profiles lost its key into auth.users")
	}
	appC, err := tryConnect(appURLFrom(t, out))
	if err != nil {
		t.Fatal(err)
	}
	defer appC.Close(context.Background())
	mustExec(t, appC, `SELECT set_config('request.jwt.claim.sub', (SELECT id::text FROM auth.users LIMIT 1), false)`)
	if n := queryInt(t, appC, `WITH x AS (INSERT INTO public.notes (body) VALUES ('new') RETURNING owner) SELECT count(*) FROM x WHERE owner IS NOT NULL`); n != 1 {
		t.Fatal("DEFAULT auth.uid() did not fill the owner")
	}
}

// A login that is not the owner of a table with row-level security would
// copy only the rows its policies show: refused before anything is copied.
func TestM8DockerRLSHidesRows(t *testing.T) {
	a, _, _ := dockerCluster(t)
	d := startDocker(t, "postgres:16", "rls")
	d.exec("postgres", "CREATE DATABASE crm")
	d.exec("crm",
		`CREATE TABLE deals (id int PRIMARY KEY, team text NOT NULL, amount numeric)`,
		`INSERT INTO deals SELECT g, 't' || (g % 3), g FROM generate_series(1, 300) g`,
		`ALTER TABLE deals ENABLE ROW LEVEL SECURITY`,
		`CREATE POLICY team0 ON deals FOR SELECT USING (team = 't0')`,
		`CREATE ROLE reader LOGIN PASSWORD '`+srcPassword+`'`,
		`GRANT SELECT ON deals TO reader`)
	u := strings.Replace(d.url("crm"), "postgres:", "reader:", 1)
	out, err := migrateCmd(t, a, u)
	t.Logf("%s", out)
	if err == nil || !strings.Contains(out, "Row-level security hides rows of public.deals") {
		t.Fatalf("expected a refusal: err=%v\n%s", err, out)
	}
	if n := userTables(t, a, "app"); n != 0 {
		t.Fatalf("app has %d tables after a refused move", n)
	}
}
