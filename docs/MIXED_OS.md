# Mixed operating systems in one cluster

The goal: Windows, Linux and Mac data computers in one cluster. This
file is the plan and the evidence log. Decisions: D29, D37, D38 (Linux is
the recommended system for database nodes). Until the proof campaign
passes, mixing is opt-in only (`coyote join <code> --allow-mixed-os`) and
every check below must pass.

## The plan (approved 2026-11-20)

Keep the fast sync we have (Postgres physical replication), make it safe
across systems, prove it on real machines, and keep checking forever.

| Step | What | State |
|---|---|---|
| 0 | Go / no-go: can systems read and write each other's files? | **Mac ARM ↔ Linux x64: passed** (below). Windows: not yet. |
| 1 | Get the program running on Windows at all; repeat Step 0 with Windows | Not started |
| 2 | Replace "same OS" with a fingerprint + behaviour check at join, plus guards | **Built** (phase 1 + phase 2 wiring, 2026-11-21), opt-in with `join --allow-mixed-os` (D37), in verification |
| 3 | Proof campaign on real mixed machines, 72 h+, every direction | Not started |
| 4 | Enable by default with a permanent background integrity check | Not started |
| Fallback | Row-copy (logical replication) mode for pairs that fail | Not started |

Roll-out order: Linux ↔ Mac and Intel ↔ ARM first; Windows joins the mix only
after it passes on its own (different compiler, maths library, flush
behaviour).

## Conditions that must be enforced in code (from the design review)

1. **Join check = fingerprint + behaviour probe**, not OS name. Fingerprint:
   every layout field of `pg_controldata` (control and catalog version,
   alignment, block and segment sizes, WAL sizes, identifier length, index
   columns, TOAST and large-object chunk sizes, date/time storage, float8
   passing, checksum version, char signedness), exact Postgres version, build
   options that matter (lz4, zstd, ICU), hashes of `share/timezone` and
   `share/tsearch_data`, extension versions. Probe: a fixed set of
   calculations whose answers must match exactly.
2. **No OS or ICU sort rules.** Drop the libc collations imported at
   `initdb`; block and detect any later use. The built-in `C.UTF-8` needs
   neither.
3. **No platform-sensitive maths inside stored structures.** Floating-point
   functions from the system maths library (see the finding below) must not
   appear in index expressions, partition keys or generated columns. Scan the
   catalog at join and continuously.
4. **No user tablespaces; extension allow-list; `jit = off`** for uniform
   behaviour; `max_prepared_transactions = 0`.
5. **Per-node settings are regenerated, never copied** (`postgresql.conf`,
   `postgresql.auto.conf`, `dynamic_shared_memory_type`, `wal_sync_method`);
   fix data-folder permissions after any copy from a Windows node.
6. **Strict flush on macOS and Windows** (`wal_sync_method =
   fsync_writethrough`): the default there does not force the drive to write,
   so a copy could confirm a save and lose it in a power cut. This is a
   durability fix for every cluster, mixed or not. Measure the speed cost.
7. **Permanent background integrity check** on every cluster
   (`pg_amcheck`, checksums), reported in `status` and the dashboard.

## Do-not-ship list

Any integrity-checker finding; any index answer that differs from its table;
any crash or replay failure that only happens on one pair of systems; any
behaviour-probe mismatch inside the allowed feature set; anything that would
need a patched Postgres.

## Evidence log

### 2026-11-20 — Step 0, Mac ARM ↔ Linux x64: passed

Method: `test/mixed/step0/` (see its README). Postgres 18.7.0 pinned builds.
Machines: a Mac (Darwin arm64) and a Linux x86_64 container on a Windows PC.
Test database: ~188,000 rows across every common type, 31 indexes of every
kind (B-tree incl. text in 20+ scripts, hash, GIN, GiST, SP-GiST, BRIN,
exclusion constraint), hash and range partitions, lz4-compressed and large
values, a large object, a materialised view. About 400 MB packed.

| Check | Result |
|---|---|
| File-format fingerprint (15 fields), Mac vs Linux | Identical |
| Mac-made files opened on Linux: checksums | 0 bad |
| … Postgres started on the foreign files | Yes |
| … `pg_amcheck` (heap + every B-tree, parent check) | Clean |
| … index answers vs table scans, 23 checks | All equal |
| … then heavy writes on Linux through every index, re-verified | Clean, all equal, 0 bad checksums |
| Linux-made files opened on the Mac, same sequence | Clean, all equal, 0 bad checksums |
| Mac files → written on Linux → opened again on the Mac | Clean, all equal, 0 bad checksums |
| Content digests (every table, large object, sequence, view) | Identical on both systems for the same files |
| Behaviour digests: sorting, upper/lower-casing, regex, hashing, full-text search, trigrams, time-zone conversion, float printing, `numeric` maths | Identical |
| Behaviour digest: floating-point maths library (`exp`, `ln`, `power`, `sin`, `sqrt`, `cbrt` on float8) | **Different** between Mac and Linux |

**The one difference found is the one the review predicted:** results of
system maths-library functions on `float8` can differ in the last digit
between systems. Stored data is unaffected. It only matters if such a
function is used inside an index, a partition key or a generated column,
which is why condition 3 above is mandatory. Which exact functions differ is
still to be pinned down (Step 2 probe).

Limits of this test: it copies cold files; it does not exercise streaming
replication, `pg_basebackup`, `pg_rewind`, crash recovery or WAL written by
one system and replayed by another (WAL replay did not happen: both stops
were clean). Those are Step 3. One Linux flavour, one Mac, no Windows, no
Linux ARM, no Mac Intel yet.

A workload detail fixed after the run: the first version built timestamp
values using the machine's local time zone, so the *natively built* databases
differed between Mac and Linux in two digests. The cross-opened files, which
are what the test is about, matched exactly. `workload.sql` now sets UTC.

### 2026-11-22 — pgvector distances differ between machines: refused (D53)

50,000 pairs of 384-dim vectors, each distance printed exactly, same
pgvector 0.8.7 source and flags. Number of pairs whose result differs:

| Function | Mac arm64 vs Mac x86_64 | Linux x86_64 vs Mac arm64 | Linux x86_64 vs Mac x86_64 |
|---|---|---|---|
| L2 `<->` | 24,165 | 23,974 | 9,870 |
| inner product `<#>` | 39,278 | 38,742 | 25,205 |
| cosine `<=>` | 47,129 | 46,349 | 34,969 |
| L1 `<+>` | 21,074 | 21,074 | 0 |
| halfvec L2 | 44,155 | 24,803 | 44,269 |
| halfvec cosine | 48,758 | 47,508 | 49,030 |
| vector_norm | 21,600 | 22,650 | 26,700 |
| sparsevec L2 | 15,984 | 15,984 | 0 |

(Mac x86_64 ran under Rosetta on the Apple-silicon Mac; Linux x86_64 is
a mini PC whose processor has AVX2, FMA and F16C.)

The summation order follows the vector width each compiler picks, and
halfvec picks an F16C path by processor at run time. An HNSW or IVFFlat
index, or a stored `l2_normalize()` result, recomputed on another machine
is not bit-identical. So `vector` is on the audit's deny list and vector
indexes are caught by the float-maths rule. Proven by
`test/integration/ext_test.go` `TestExtVectorMixedGuard` (Mac, faked
second OS): join refused with an HNSW index (named), refused with only the
extension, accepted without it; in the mixed cluster HNSW, IVFFlat and a
halfvec expression index are refused as DDL and the audit reports the
extension.

## Step 2, phase 1: built 2026-11-21

In `internal/pg/`: `checks.go`, `fingerprint.go`, `probe.go`, `probe_sql.go`,
`portability.go`, `verify.go`, `guards_test.go`, `testdata/guards/`. Tests on
real Postgres: `test/mixed/guards/` (`-tags=integration`, 8 tests, passed 3×
on macOS arm64). Campaign driver: `test/mixed/campaign/` (`campaign peer` on
each node, `campaign run` drives switches along every ordered pair and
compares every row; not runnable end to end until phase 2).

- **Fingerprint:** 15 control-file fields + byte order (read from the file
  itself), build options, sha256 of timezone, timezonesets, tsearch and
  snowball folders, encoding, Unicode/ICU versions, builtin collations,
  extensions. OS and CPU are recorded, not compared.
- **Probe:** 178 read-only statements; 145 must match (float, numeric,
  integer, hash, time, format, text, fts, jsonb, types), 33 libm ones are
  informational. About 1 s; works on standbys.
- **Audit:** reads stored expression trees (pinned built-ins have no
  pg_depend rows). Finds libc/ICU collations, libm functions in indexes /
  partition keys / generated columns / checks, tablespaces, extensions off
  the allow-list, per-node settings. `DropImportedCollationsEverywhere`
  removes the ~3200 imported collations.
- **Extension allow-list:** plpgsql, amcheck, pg_stat_statements, pgcrypto,
  uuid-ossp, citext, pg_trgm, btree_gin, btree_gist, hstore, ltree, unaccent,
  tablefunc, intarray, pageinspect, pg_visibility, pg_buffercache,
  pgstattuple. Blocked: earthdistance, cube, plpython3u, xml2, file_fdw,
  postgres_fdw, dblink, bloom, fuzzystrmatch. Least certain: uuid-ossp,
  citext, ltree.
- **Verify:** Quick / Standard / Deep; amcheck heap, btree, gin; index scan
  vs table scan for gist, spgist, brin, hash (simple protocol + EXPLAIN,
  because a cached plan once made this check unable to fail); partition
  routing; checksum counters; wraps `pg_amcheck`, offline `pg_checksums`.

Open questions from phase 1: ICU version treated as blocking (may be too
strict); degrees()/radians() blocked in the audit but must-match in the
probe (pick one rule); only run on macOS arm64; the other targets'
pg_controldata samples in the tests are synthesised.

## Step 2, phase 2: wiring — built 2026-11-21

Done as listed below, with these differences:

- Opt-in: `coyote join <code> --allow-mixed-os` turns `Settings.MixedOS` on
  when the first mixed join is accepted.
- Item 4: the scratch check runs for every data join (it is cheap); live
  re-measure after each start; drift handling (refused update → the copy
  stops, leaves the sync set, `needs_reclone`).
- Item 5 lives in the agent (every primary start), not in `InitDB`, so it
  also covers existing clusters.
- Item 6: nothing to change (0 is Postgres' default; the audit reports
  any other value).
- Rolling upgrades: `caps` RPC gate, `JoinProtocol`, exit on a halted
  state machine.
- Tests: `internal/cluster/join_mixed_test.go` (state machine table, real
  Raft join, old joiner refused), `internal/pg/fingerprint_explain_test.go`,
  `test/integration/mixed_test.go` (tag `mixed`: join, failover and switch
  both ways, guard failing after failover, drifted copy never promoted,
  join racing a failover; one machine with `COYOTE_TEST_FAKE_OS`).
  Run: `go test -tags 'integration mixed' -count=1 -run TestMixed ./test/integration/`.
- **ICU (D48).** The downloaded builds ship ICU 18.0 (macOS arm64) and 15.0
  (Linux x86_64). Mixed clusters therefore sort only with Postgres' built-in
  provider: non-built-in collations are dropped in every database and
  refused by the `coyote_guard` event triggers; ICU versions are no longer
  compared, the built-in Unicode version is.
- **Float maths (D49).** The macOS arm64 build fuses multiply-add; Linux
  x86_64 does not, so `corr()`, `regr_*`, `covar_*` and geometric distances
  can differ in the last bit. Stored data is shipped as bits and never
  recomputed by a copy, so the float and geometry probe groups are only
  reported. Structures that would recompute float or geometric maths on a
  new main node (expression/partial indexes, CHECK and exclusion
  constraints, stored generated columns, partition keys, geometric GiST /
  SP-GiST / BRIN indexes) are refused at DDL time, found by the quick
  check, and refuse a mixed join. Plain B-tree, hash and BRIN indexes on
  float columns are allowed (sorting and hashing raw values is the same
  everywhere; checked on both machines).
- **What users see.** In a mixed cluster, results of decimal maths like
  `corr()` may differ in the last digits after a failover. The stored data
  never changes.
- **Later option (B).** Our own Postgres builds with `-ffp-contract=off`
  (and one ICU) on every platform would make float maths must-match again
  and lift the D49 restrictions. Not planned yet.
- **Real Mac <-> Linux machine run (2026-11-21, after D48 + D49): works.** Mixed
  join, failover macOS -> Linux and Linux -> macOS, each old main node
  rewinding back as a copy across the OS boundary; data, text and float
  index order and float hashes identical on both.

The original list:

1. `internal/cluster/state.go` `Platform`: add `Fingerprint` hash and
   `ProbeDigest`; OS/Arch for display only.
2. `internal/cluster/fsm.go` `checkPlatform`: accept a data member when its
   fingerprint hash and probe digest equal the existing data members';
   rejection carries the plain-words compare text. Update `fsm_test.go`,
   `cluster_test.go`.
3. `internal/cluster/join.go`: joiner sends fingerprint + probe; leader
   checks before the Raft add.
4. `internal/agent/agent.go` at start: `pg.CollectFingerprint`; probe a
   scratch Postgres (temporary initdb, under the guard, thrown away) so a
   joiner is checked before cloning.
5. `internal/pg/node.go` `InitDB`: after first start run
   `DropImportedCollationsEverywhere`, `CREATE EXTENSION amcheck` in
   template1, postgres, app.
6. `internal/pg/config.go`: `max_prepared_transactions = 0`.
7. Agent scheduling: audit every 10 min, quick verify on a slower timer, on
   every data node; a blocking finding blocks new mixed joins and
   cross-system switch-over.
8. `internal/agent/status.go`: show fingerprint, last audit/verify, blocking
   findings in plain words.
9. Roll out in two stages (plan): Linux ↔ Mac first; Windows joins the mix
   only after Windows passes on its own.

