# Moving an existing database into CoyoteDB

`coyote migrate` copies a PostgreSQL database you already have (Supabase,
Neon, Amazon RDS, Heroku, another server, a laptop) into your CoyoteDB
cluster. The old database is only read, never changed. Decision: D47.

## The short version

On any computer of the cluster:

```bash
coyote migrate --dry-run
coyote migrate
```

Each asks for the connection string of the old database
(`postgresql://user:password@host:5432/dbname`); paste it and press Enter.
The first command checks everything and copies nothing. The second does the
move and prints the connection string your app uses from now on (the same
as `coyote conninfo`).

For scripts, put the connection string in `COYOTE_MIGRATE_SOURCE`, or pipe
it in:

```bash
COYOTE_MIGRATE_SOURCE="postgresql://user:password@host:5432/dbname" coyote migrate
```

Either way the password never shows in the list of running programs. Giving
the string as an argument (`coyote migrate "postgresql://..."`) also works,
but then other users of the computer can see it while the command runs, and
it lands in your shell history; the command says so.

## What it does

1. **Checks first** (also what `--dry-run` prints): the source's PostgreSQL
   version (12 to 18), size, number of tables and rows, text encoding and
   sort order, the extensions it uses (and whether CoyoteDB has them), that
   the login can read every table (including row-level security), and that
   the cluster's database is empty.
2. **Copies out** with `pg_dump` (several tables at a time) from one
   consistent snapshot, so the copy is a single moment of the source even
   if your app keeps writing to it.
3. **Copies in** with `pg_restore` as the cluster's app role, through the
   cluster's own client port to the main node. The other nodes get the data
   by normal replication. It goes into a database named `app__migrating`
   first, so an unfinished copy never looks like the real one.
4. **Checks the copy**: every table's row count, and a checksum of every row
   (tables over 256 MB: the first and last 5000 rows by primary key). Then
   it moves every sequence past the highest id in use, so your app's next
   insert never collides.
5. **Switches in**: only when everything matched does `app__migrating`
   become `app`.

Everything in the copy belongs to the app role. Users, roles and
permissions of the old server are not copied (hosted services do not let
anyone copy them); the app role can do everything in its database.

## Options

| Option | What it does |
|---|---|
| `--dry-run` | Check and report only. |
| `--database NAME` | Copy into another database of the cluster (created if missing). Default `app`. |
| `--replace` | The target database is not empty: delete its contents and put the copy in its place. Asks you to type the database name first. |
| `--yes` | Answer the questions with yes (scripts). |
| `--keep-going` | Switch the copy in even if some non-table parts failed (for example a trigger that calls something missing). They are listed. Every table must still match. |
| `--skip-extension NAME` | Leave an extension out (repeatable). Anything that uses it fails to copy. |
| `--exclude-schema NAME` | Leave a schema out (repeatable). |
| `--jobs N` | Tables copied at the same time (default up to 4). |
| `--work-dir DIR` | Where the temporary copy is kept during the move (default: the system's temporary folder). It is deleted afterwards. |
| `--data-dir DIR` | Which node to talk to, if not the default folder. |
| `--live` | Start a live copy (below). |
| `--status`, `--cutover`, `--cancel` | Follow, finish or stop a live copy. |

## Live copy: move with only a short stop

A normal move is one moment of the old database: your app should stop
writing while it runs. A live copy keeps the cluster in step with the old
database while your app keeps running, and the stop at the end is only
seconds.

On the **main node's computer** (`coyote status` names it):

```bash
coyote migrate --live          # copies the tables, then follows every change
coyote migrate --status        # how far along, how far behind
# when ready: stop your app (and workers, cron jobs), then
coyote migrate --cutover       # last changes, check, sequences, switch in
```

`--cutover` waits for the last changes, compares every table with the old
database (counts and row checksums), copies the sequences and moves them
past the highest id in use, removes what the live copy put on the old
database, and only then switches the copy in. If anything does not match
(usually: something still writes to the old database) nothing is switched
and the live copy keeps running; stop the writer and run `--cutover`
again. `--cancel` stops and removes a live copy; the cluster's database is
not touched.

What it needs from the old database, all checked before anything starts:

- **Logical replication on:** `wal_level = logical`. Amazon RDS:
  `rds.logical_replication = 1` in the parameter group. Neon: turn on
  logical replication in the project settings. Supabase has it on. Own
  server: set it and restart.
- **A login that may replicate** (the `REPLICATION` right; on most hosts
  the main `postgres` user) and **owns the tables**, and one free
  replication slot.
- **A primary key on every table** (or `REPLICA IDENTITY FULL`): without
  one, the old database would refuse your app's updates and deletes once
  the live copy starts.
- **No large objects** (use the normal move).
- **The main node must reach the old database** over the network itself
  (the cluster's PostgreSQL connects to it).

While a live copy runs:

- **The old database's connection string is kept on the cluster** (inside
  the live copy's subscription, readable only by the cluster's
  administrator) until `--cutover` or `--cancel` removes it.
- **Changes to tables' definitions are not copied** (`ALTER TABLE`, new
  tables). Make none until the cutover.
- **If the cluster's main node changes** (it died or was switched), the
  live copy carries on from the new main node by itself, from the last
  change it had; nothing is lost or applied twice. It can take up to a
  minute to reconnect. `--status` and `--cutover` must then be run on the
  new main node's computer; run anywhere else they say which one.

## If something goes wrong

The cluster's database is changed only in the very last step. Any failure
before that leaves it as it was, and you can run the same command again.

- **The main node changes during the copy** (it was stopped or died): the
  command stops with "Nothing was switched in". Wait until `coyote status`
  shows a healthy main node and run the same command again (add
  `--replace --yes` if it says the database is not empty).
- **Ctrl-C:** stops at once; nothing is switched in. Run it again.
- **`--cutover` stopped partway** (terminal closed, SSH dropped, Ctrl-C):
  run `--cutover` again; it carries on from where it stopped. Once the
  copy has been checked, the cluster keeps a record of that check; the
  rerun first makes sure the copy has not changed since, and only then
  finishes. If the switch had already finished, it says the move is done.
  `--cancel` works at every point too: it removes the copy and leaves the
  cluster's database as it was.
- **A table does not match:** listed, nothing switched in.
- **Some objects fail** (a policy, a trigger, an event trigger): listed,
  nothing switched in, unless you add `--keep-going`.

A log of every run is in the node's `logs` folder (`migrate-<time>.log`).

## Limits, plainly

- **Your app should stop writing during a normal move**, or accept that
  writes made after the copy started are not in the copy. The copy is one
  moment of the source. A live copy (above) avoids that.
- **Text sort order.** CoyoteDB sorts text by character code on every node
  (so copies on different computers stay identical). If the source sorted
  by a language (`en_US.UTF-8`, ICU), the data is the same but `ORDER BY`
  on text can come out in a different order, for example capital letters
  first. Columns that name a language sort rule explicitly
  (`COLLATE "en-x-icu"`) are refused before anything is copied.
- **Extensions** must exist in CoyoteDB's PostgreSQL build. `vector`
  (pgvector 0.8.7) comes with CoyoteDB but is installed only when each node
  is started with `COYOTE_EXTENSIONS_FROM` set to the folder or address
  holding the pgvector archives (D53); otherwise the check says so. Vector
  columns and HNSW / IVFFlat indexes move like any other. Not available:
  PostGIS, `pg_cron`, `pgjwt` and others outside the standard PostgreSQL
  set. The check names them before anything is copied. A cluster that mixes
  kinds of computers cannot have pgvector (D53).
  Statistics-only extensions (`pg_stat_statements` and similar) and the
  host's own plumbing (Supabase's `pg_graphql`, `pgsodium`, `pg_net`,
  vault; RDS's `aws_*`) are left out and listed.
- **Extensions only the administrator may create** (for example
  `postgres_fdw`): run the command on the main node's computer.
- **Supabase:** your users (schema `auth`) and file records (schema
  `storage`) are copied as plain tables, so tables that point at
  `auth.users` and defaults like `auth.uid()` keep working. Supabase's own
  machinery (`realtime`, `supabase_functions`, `graphql`, `vault`,
  `pgsodium`, `net` and the like) is left out and listed. The files
  themselves stay in Supabase Storage, and signing in needs a sign-in
  service of your own. Security policies for Supabase's roles
  (`authenticated`, `anon`) are left out and listed: those roles do not
  exist on the cluster, your app logs in as the tables' owner, and
  row-level security stays on. Use the direct connection string (port
  5432), not the pooler (port 6543).
- **Large objects** are copied and counted; publications, subscriptions,
  security labels and tablespaces are not copied.
- **Source version** 12 to 18 (12 to 17 tested from the official Docker images, 18 from the bundled build). Older: upgrade it first.
- **Space:** the temporary copy needs room in `--work-dir` (compressed,
  usually much smaller than the database).

## Moving out

CoyoteDB is plain PostgreSQL. `pg_dump` against `coyote conninfo`'s string
works with any PostgreSQL 18 tools.
