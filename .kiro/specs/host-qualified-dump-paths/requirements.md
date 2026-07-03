# Requirements — Host-qualified dump paths

## Introduction

pg-autodump currently names each backup artifact from the **database name alone**
(`<dbname>.dump`, or `<dbname>.<UTC>.dump` when `DUMP_KEEP>1`). Two distinct databases
that share a name on different servers therefore collide on one artifact: with
`DUMP_KEEP=1` the second worker's atomic commit overwrites the first server's backup
every run; with `DUMP_KEEP>1` retention prunes the shared `<dbname>.` prefix across both
servers. For a backup tool this is silent data loss (tracked as code-review finding
**h-f3**, severity high).

The validator already treats `(host, port, dbname)` as a database's identity — the
duplicate check keys on exactly that tuple (`internal/spec.ParseSpecs`). This feature
makes the **artifact path** honor the same identity by qualifying every dump with the
server it came from, so two databases that share a name on different `host:port`
endpoints can never map to the same file.

This is a **behavior-changing** feature: it alters the published on-disk layout that
external collectors (Kopia/Restic/Borg/rsync) consume, so it requires a deliberate
rollout and a migration note.

It also adds **IPv6 host support** to `DB_SPECS` (bracketed `[addr]:port` syntax), so
pg-autodump can back up databases in IPv4-only, IPv6-only, and dual-stack environments. The
artifact-path scheme accommodates IPv6 addresses safely — canonicalized and encoded into a
filesystem-portable, collision-free path component (see Requirement 10 and Design).

## Requirements

### Requirement 1 — Every reachable database maps to a unique artifact path

**User story:** As an operator backing up many PostgreSQL servers, I want each database's
dump to land at a path unique to that database, so that no backup is ever silently
overwritten by another database that happens to share its name.

**Acceptance criteria:**

1. WHEN two valid specs share a `dbname` but differ in `host` and/or `port`, THEN the
   system SHALL write their dumps to two distinct paths.
2. WHEN two valid specs share `dbname` and `port` but differ in `host`, THEN the system
   SHALL write their dumps to two distinct paths.
3. WHEN two valid specs share `host` and `dbname` but differ in `port` (e.g. two
   PostgreSQL containers on one host published on `:5432` and `:5433`), THEN the system
   SHALL write their dumps to two distinct paths.
4. The mapping from `(host, port, dbname)` to artifact path SHALL be injective: no two
   distinct identities ever produce the same path.

### Requirement 2 — Per-server subdirectory layout

**User story:** As an operator, I want each server's dumps grouped under a readable,
self-evident folder, so I can see at a glance which server a backup came from.

**Acceptance criteria:**

1. The system SHALL place each database's artifact under a per-server subdirectory of
   `DUMP_DIR` named `<host>_<port>`.
2. WITHIN that subdirectory the artifact SHALL keep the existing filename shape:
   `<dbname>.dump` when `DUMP_KEEP=1`, and `<dbname>.<UTC>.dump` when `DUMP_KEEP>1`.
3. The subdirectory name SHALL use the **resolved** port (see Requirement 3), not the raw
   token.
4. The `user` field SHALL NOT appear in the path — the authenticating role is not part of
   a backup's identity (consistent with the existing duplicate key, which excludes it).

Example layout:

```
/dumps/
  db1.example.com_5432/authentik.dump
  db2.example.com_5432/authentik.dump        # same dbname, different host — no collision
  apphost_5432/app.dump
  apphost_5433/app.dump                       # same host, second container — no collision
```

### Requirement 3 — Default port is resolved before path construction

**User story:** As an operator who omits the port for the common `:5432` case, I want the
path to be well-formed and consistent with the explicit-port form.

**Acceptance criteria:**

1. WHEN a spec omits the port (3-field `host:dbname:user`), THEN the system SHALL use the
   resolved default port (5432) in the subdirectory name.
2. A spec written `host:dbname:user` and the same spec written `host:5432:dbname:user`
   SHALL resolve to the **same** subdirectory and the **same** artifact path (they
   address the same server).

### Requirement 4 — Retention prunes per server, never across servers

**User story:** As an operator using `DUMP_KEEP>1`, I want "keep the N newest per
database" to mean per actual database, not per database-name-across-all-servers.

**Acceptance criteria:**

1. WHEN `DUMP_KEEP>1`, THEN retention SHALL consider only the timestamped dumps within a
   single server's subdirectory when deciding what to prune.
2. Pruning one server's dumps SHALL NOT remove or count any other server's dumps, even
   when they share a database name.
3. The stable `<dbname>.dump` (a `DUMP_KEEP=1` artifact) SHALL never be pruned (unchanged
   from current behavior, now scoped within the subdirectory).

### Requirement 5 — Subdirectory creation and failure handling

**User story:** As an operator, I want a dump to fail loudly and per-database if its
target directory cannot be created, never silently or by corrupting another path.

**Acceptance criteria:**

1. BEFORE staging a dump, the system SHALL ensure the server subdirectory exists.
2. WHEN the subdirectory cannot be created, THEN the system SHALL return a per-database
   failure Result with a dedicated `mkdir_failed` reason and a detail naming the directory
   and the underlying OS error (so the operator can distinguish a destination-directory
   problem from a dump failure or a rename failure), and SHALL NOT affect other databases
   in the run.
3. Concurrent workers targeting the same server subdirectory SHALL be safe (idempotent
   creation).
4. The subdirectory SHALL be created with restrictive permissions consistent with the
   unprivileged, non-root runtime (no broader than `0700`).

### Requirement 6 — Path-traversal safety is preserved

**User story:** As a security-conscious operator, I want operator-supplied `DB_SPECS`
values to remain unable to write outside `DUMP_DIR`.

**Acceptance criteria:**

1. The system SHALL construct artifact paths only from validated `host`/`dbname` values
   (existing grammar: host `[a-zA-Z0-9_.-]` no `..`; dbname `[a-zA-Z0-9_-]` no `..`; no
   `/` in either), so a constructed path can never escape `DUMP_DIR`.
2. No new input SHALL reach a filesystem path without passing the existing `spec`
   validation.
3. IPv6 literal hosts are supported via the bracketed `[addr]:port` syntax
   (Requirement 10); their address is canonicalized and encoded into a filesystem-safe,
   collision-free path component (Design), so a raw `:` never reaches a path component.

### Requirement 7 — Backward compatibility and migration

**User story:** As an operator upgrading an existing deployment, I want a clear,
documented migration for the changed layout.

**Acceptance criteria:**

1. The README and the `pg-autodump` steering doc SHALL document the new layout and that
   it changes the on-disk path of every artifact.
2. The documentation SHALL describe the migration for versioned collectors (e.g. Kopia):
   old flat `<dbname>.dump` files at the `DUMP_DIR` root are no longer written and may be
   removed once; new dumps appear under `<host>_<port>/` and start a fresh version chain.
3. The system SHALL NOT silently read, move, or delete pre-existing flat artifacts at the
   `DUMP_DIR` root (operator-owned migration).

### Requirement 8 — Component-length robustness (edge case)

**User story:** As an operator with long fully-qualified hostnames, I want the tool to
behave predictably rather than fail obscurely on filesystem name limits.

**Acceptance criteria:**

1. WHEN a `<host>_<port>` subdirectory name would exceed the filesystem per-component
   limit (255 bytes on common Linux filesystems), THEN the system SHALL surface a clear,
   per-database failure (not a generic OS error mid-run) — see Design for the chosen
   guard.
2. Typical hostnames (docker service names, short FQDNs) SHALL be unaffected.

### Requirement 9 — Existing safety invariants are retained

**Acceptance criteria:**

1. Verify-before-replace SHALL still apply within the server subdirectory: a
   partial/truncated/empty dump SHALL never overwrite a known-good file at the new path.
2. The exact-duplicate behavior SHALL be unchanged: a repeated `host:port:dbname` spec is
   still reported `duplicate` and skipped (the new layout removes the cross-host
   collision but does not change exact-duplicate handling).
3. The closed dump-result reason enum SHALL be extended with one new, documented reason
   **`mkdir_failed`** for the destination-directory failure (Requirement 5), added to the
   steering enum list and mapped to `error` level. The over-long-host failure
   (Requirement 8) SHALL use the existing `invalid` reason with a descriptive detail. No
   other reason SHALL change.

### Requirement 10 — IPv6 host support

**User story:** As an operator in an IPv6-only or dual-stack environment, I want to point
pg-autodump at a database by its IPv6 literal address, so backups work regardless of the
network stack.

**Acceptance criteria:**

1. The system SHALL accept a bracketed IPv6 host in `DB_SPECS`:
   `[<ipv6>]:<port>:<dbname>:<user>` and the port-omitted `[<ipv6>]:<dbname>:<user>`
   (port defaults to 5432). The brackets disambiguate the address colons from the
   field-separator colons.
2. The system SHALL validate the bracketed address as an IP literal; a malformed address
   (including IPv6 zone-id forms like `fe80::1%eth0`, and a bracketed value that is not an
   IP) SHALL be reported per-DB as `invalid` with a clear detail, never dispatched.
3. The system SHALL canonicalize the parsed address (lowercase, compressed) so that
   different spellings of the same address (`2001:DB8::1`, `2001:db8:0:0:0:0:0:1`,
   `2001:db8::1`) resolve to ONE identity, ONE artifact path, and ONE duplicate-key entry.
4. The IPv6-derived path component SHALL be filesystem-portable (contain no `:`), injective
   over distinct addresses, AND in a namespace disjoint from hostname/IPv4 components — no
   IPv6 server's directory may ever equal a hostname/IPv4 server's directory (no cross-type
   collision re-introducing h-f3 in IPv6 form).
5. Connectivity SHALL work unchanged: the probe's TCP dial already brackets IPv6 (via
   `net.JoinHostPort`), and `pg_dump`/`psql` receive the bare canonical address (the form
   libpq expects), so NO change to the `pg` boundary is required.
6. The duplicate-detection key SHALL remain unambiguous with IPv6 hosts present (port and
   dbname contain no `:`, so the IPv6 colons stay within the host segment of the key).
7. Unbracketed hostnames and IPv4 addresses SHALL parse and behave exactly as before
   (no regression to the existing grammar or path layout).
8. Documentation SHALL show the bracketed IPv6 syntax and note the `.pgpass` caveat (libpq
   `.pgpass` is `:`-delimited, so an IPv6 host's colons must be backslash-escaped there, or
   `PGPASSWORD` used instead — a typical deployment uses `PGPASSWORD`).
