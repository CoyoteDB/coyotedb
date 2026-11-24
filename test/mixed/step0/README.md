# Step 0: can two systems share one set of Postgres files?

A quick go / no-go test for mixed-system clusters (decision D29, plan in
`docs/MIXED_OS.md`). It does not use the `coyote` program at all: it checks the
foundation the plan rests on.

1. `make.sh <label>` — on one system: `initdb` with CoyoteDB's exact settings
   (built-in `C.UTF-8` locale, checksums), load `workload.sql` (every common
   type and index kind, text in many scripts, partitions, large objects),
   verify, stop cleanly, pack the data folder.
2. Copy the pack to another system.
3. `open.sh <source-label> <this-label>` — there: unpack, check the file-format
   fingerprint and checksums, start Postgres on the foreign files, verify,
   then write heavily through every index (what a newly promoted main node
   would do), verify again, pack again.
4. Copy that pack back and `open.sh` it on the first system.

`verify` (in `lib.sh`) runs: `pg_amcheck --heapallindexed --parent-check
--rootdescend`, `gin_index_check`, `idxscan.sql` (index answers must equal
table-scan answers for index kinds amcheck cannot verify, plus hash-partition
routing), and `digest.sql` (content and behaviour digests that must be equal
on every system). `pg_checksums --check` runs offline.

`linux-run.sh` runs the Linux side in a throwaway container (`golang`
image, as root; it drops to a normal user). Needs `PGBIN` (the pinned Postgres `bin` dir) and `WORK`.

Results are recorded in `docs/MIXED_OS.md`.
