# Security

## Supported versions

| Version | Security fixes |
|---|---|
| The newest 0.1.x release | Yes |
| Older 0.1.x releases | No: update to the newest 0.1.x release |

Fixes are made as a new patch release (0.1.1, 0.1.2, ...), not as changes
to an old one. `CHANGELOG.md` lists every release, the PostgreSQL version
it carries, and what it fixes.

## Reporting a vulnerability

Please report it privately through GitHub's private vulnerability
reporting: open https://github.com/CoyoteDB/coyotedb/security and choose
"Report a vulnerability". Do not open a public issue for it.

Helpful to include: the CoyoteDB version (`coyote version`), how you run it
(Docker image tag, or built from source, and the operating system), what
an attacker can do, and the steps to show it.

We answer in that private report, agree on a fix and when to publish it,
and credit you in the release notes if you want to be named.

A problem in PostgreSQL itself, not in how CoyoteDB runs it, belongs with
the PostgreSQL project: https://www.postgresql.org/support/security/

## What we commit to

These are our aims, not guarantees.

- **PostgreSQL minor releases.** PostgreSQL publishes a minor release
  (18.7, 18.8, ...) every three months, and sooner when a security problem
  needs it. These include its security fixes. For each one we make a
  CoyoteDB patch release that carries it, aiming for within 7 days of the
  PostgreSQL release. CoyoteDB uses prebuilt PostgreSQL programs
  (theseus-rs/postgresql-binaries) whose checksums are pinned in our source
  code, so the release follows once those builds exist for every platform
  we ship and our tests pass with them.
- **Problems in CoyoteDB itself.** A serious problem (security, or a risk
  to saved data) gets a patch release as soon as it is fixed and tested.
  The release notes say what was wrong and who is affected.
- **Everything else in the program.** Every release is built with the
  newest patch release of Go, on base images refreshed to their latest
  security updates, and is checked for known vulnerabilities in the Go code
  (`govulncheck`) and in the control panel's packages (`npm audit`). The
  check is `scripts/security-check.sh`.

## Updating

A patch release updates a running cluster one computer at a time, with the
database up and no saved write lost: the copies first, then a planned
switch-over, then the old main computer. Step by step for Docker:
`docker/README.md`, "Updating to a new version".

## PostgreSQL major versions

CoyoteDB runs PostgreSQL 18. PostgreSQL 19 (and later major versions) is
not supported yet. A move to a new major version changes the database files
and is not part of a patch release.
