# Design — Host-qualified dump paths

## Overview

Qualify every dump artifact by the server that produced it, using a per-server
subdirectory of `DUMP_DIR` named `<host>_<port>`, with the database's existing filename
shape kept inside it. This makes the artifact path honor the `(host, port, dbname)`
identity the validator already enforces, which structurally eliminates the h-f3 silent
overwrite **and** the cross-host prune-prefix bug.

```
DUMP_DIR/
  <host>_<port>/
    <dbname>.dump                 # DUMP_KEEP=1 (stable)
    <dbname>.<UTC>.dump           # DUMP_KEEP>1 (timestamped, pruned to N newest)
```

The change is deliberately minimal: it introduces one helper and changes the `dir`
argument passed to the two functions that already take a directory
(`stageAndReplace`, `pruneOldDumps`). The filename functions and the prune matcher are
untouched — they simply operate inside the subdirectory.

## Identity model

A database pg-autodump can reach is uniquely identified by `(host, port, dbname)`:

- Two genuinely different PostgreSQL servers cannot share one `host:port` — if they did,
  the client could not connect to both. So `host:port` is a unique server identity.
- Within one server (one `host:port`), `dbname` is unique.
- Therefore `(host, port, dbname)` is a complete, unique identity, and it is exactly the
  key `ParseSpecs` builds for duplicate detection:
  `key := s.Host + ":" + strconv.Itoa(s.Port) + ":" + s.DBName`.

`user` is intentionally excluded: the authenticating role is how you dump the database,
not part of which database it is (and the duplicate key already excludes it).

We deliberately do **not** use a database-side id:

- `pg_database.oid` is unique only within one cluster (two servers can both have OID
  16384) and changes on drop/recreate — it would orphan backups. Rejected.
- The cluster `system_identifier` is globally unique and stable but requires an extra
  query per database and renders as an opaque 19-20 digit number, defeating
  operator-readability. Rejected in favor of the network address, which is free (already
  in the spec) and human-readable.

## Directory and filename scheme

New helper (in `internal/dump`, beside `dumpFileName`):

```go
// serverDir is the per-server subdirectory name for a database's artifacts.
// For a hostname or IPv4 host (validated to [a-zA-Z0-9_.-], no ':'), it is
// "<host>_<port>". For a canonical IPv6 host (the only host kind that contains
// ':'), it is "@<addr-with-colons-as-dashes>_<port>": the '@' prefix puts IPv6
// dirs in a namespace disjoint from hostname/IPv4 dirs (the host grammar forbids
// '@'), and ':'->'-' is injective over canonical IPv6 (which contains no '-'),
// so distinct addresses never collide and the result is filesystem-portable.
func serverDir(host string, port int) string {
    if strings.ContainsRune(host, ':') {
        return "@" + strings.ReplaceAll(host, ":", "-") + "_" + strconv.Itoa(port)
    }
    return host + "_" + strconv.Itoa(port)
}
```

`dumpFileName(dbname, keep, t)` is **unchanged** — it still returns `<dbname>.dump` /
`<dbname>.<UTC>.dump`. It is now resolved against the server subdirectory rather than the
`DUMP_DIR` root.

## Code changes

All changes are in `internal/dump/dump.go` (`dumpOne`); `verify.go` and `spec` are
unchanged except for the optional length guard (Edge cases).

Current `dumpOne` (post-probe success path):

```go
res := stageAndReplace(dumpCtx, o.pg, o.dumpDir, dumpFileName(s.DBName, o.keep, start), conn)
...
if res.Reason == ReasonOK && o.keep > 1 {
    if removed, err := pruneOldDumps(o.dumpDir, s.DBName, o.keep); err != nil {
```

New `dumpOne` (post-probe success path):

```go
dir := filepath.Join(o.dumpDir, serverDir(s.Host, s.Port))
if err := os.MkdirAll(dir, 0o700); err != nil {
    res := Result{Reason: ReasonMkdirFailed, Detail: "cannot create server dir " + dir + ": " + err.Error()}
    res.Host, res.DBName, res.ServerVersion, res.Duration = s.Host, s.DBName, major, o.now().Sub(start)
    return o.finish(&res, nil)
}

res := stageAndReplace(dumpCtx, o.pg, dir, dumpFileName(s.DBName, o.keep, start), conn)
...
if res.Reason == ReasonOK && o.keep > 1 {
    if removed, err := pruneOldDumps(dir, s.DBName, o.keep); err != nil {
```

Notes:

- `os.MkdirAll` is idempotent and safe to call concurrently for the same or different
  subdirectories, satisfying the worker-pool concurrency requirement (two workers dumping
  two databases on the same server both create/observe the same subdir).
- `0700` matches the unprivileged, non-root, `read_only` container posture; only the
  process needs to traverse/write it. `DUMP_DIR` itself is operator-mounted; we create
  only its children.
- `stageAndReplace` already stages an `atomicfile` temp inside the `dir` it is given and
  renames within it (same-filesystem atomic replace + dir fsync). Passing the subdirectory
  is sufficient; no change to `verify.go` is required. The subdir must exist first, which
  `MkdirAll` guarantees.
- The `MkdirAll` failure path is mapped to a **new closed-enum reason `ReasonMkdirFailed
  = "mkdir_failed"`** (added in `internal/dump/result.go`, beside `ReasonRenameFailed`),
  with a detail naming the directory and the OS error — so an operator can tell a
  destination-directory problem from a dump failure (`pg_error`/`empty`/`truncated`) or a
  commit failure (`rename_failed`). `levelFor` already returns `slog.LevelError` for any
  reason not in its `ok`/warn cases (the `default` arm), so `mkdir_failed` logs at `error`
  with no `levelFor` change; the steering reason-enum list and any enum-completeness test
  are updated to include it.

## Collision-safety (why this is provably injective)

The artifact path is `DUMP_DIR / "<host>_<port>" / "<dbname>.dump"` (or the timestamped
variant). Two identities collide only if both the subdirectory component and the filename
component are equal.

- **Filename component** is `<dbname>.dump` (or `<dbname>.<ts>.dump`); equal iff `dbname`
  (and `ts`) are equal.
- **Subdirectory component** is `<host>_<port>`. This is injective over valid inputs: the
  port is `strconv.Itoa(port)` — a contiguous run of digits — placed after the final `_`.
  Given the string, the port is recovered as the maximal trailing digit run (any digits in
  `host` are separated from it by the joining `_`, which is not a digit), and `host` is the
  remainder before that `_`. So distinct `(host, port)` pairs never produce the same
  component.

> This is precisely why the dbname is kept in a **separate path component** rather than
> joined into one string. A flat `<host>_<port>_<dbname>` would **not** be injective,
> because `dbname` may itself contain `_` (allowed by `validateIdentifier`):
> `(host=a, port=5, dbname=b_5_c)` and `(host=a_5_b, port=5, dbname=c)` both render
> `a_5_b_5_c`. The subdirectory split removes that ambiguity (`/` separates the components
> and is forbidden in both grammars).

## Path-traversal safety

`host` is validated to `[a-zA-Z0-9_.-]` with `..` rejected, and `dbname` to
`[a-zA-Z0-9_-]` with `..` rejected; neither may contain `/`. Therefore both path
components are single, non-escaping segments, and `filepath.Join(DUMP_DIR, serverDir,
filename)` can never resolve outside `DUMP_DIR`. No new validation is required; the
feature relies on the existing single validation path.

## Retention behavior

`pruneOldDumps(dir, dbname, keep)` is called with the server subdirectory as `dir`. Because
each server has its own subdirectory, the prefix match `<dbname>.` is now naturally scoped
to one server — the cross-host prefix-sharing bug disappears with no change to the matcher.
The `DUMP_KEEP=1` stable file exclusion and the best-effort error handling are unchanged.

## Migration

The on-disk path of every artifact changes (e.g. `/dumps/authentik.dump` →
`/dumps/authentik-pg_5432/authentik.dump`). Consequences and guidance:

- **Versioned collectors (Kopia — a common self-hosted default):** Kopia snapshots the whole
  `/dumps` tree, so the new paths simply begin a fresh version chain; the old flat files
  retain their last version until the operator removes them. No data is lost.
- **The tool does not touch existing flat files.** It neither reads, moves, nor deletes
  pre-existing `<dbname>.dump` files at the `DUMP_DIR` root; cleanup is an explicit,
  one-time operator action.
- **Docs:** README config/usage and the `.kiro/steering/pg-autodump.md` layout/retention
  notes are updated to describe `<host>_<port>/<dbname>.dump` and the migration.

## Edge cases

- **Filesystem per-component limit (NAME_MAX, 255 bytes on ext4/xfs/btrfs).** A maximal
  253-char FQDN plus `_<port>` can exceed 255. Chosen guard: extend `spec` validation so a
  spec whose resulting `serverDir` would exceed 255 bytes is marked `Invalid` with a clear
  reason ("host too long for artifact path"), so it is reported per-DB and skipped like any
  other invalid spec rather than failing as a generic OS error mid-run. This keeps the
  failure inside the existing single validation path and the closed reason taxonomy.
  Typical hostnames are far under the limit; this is a robustness guard, not a common path.
- **Same database via two host spellings** (`db.internal` vs `10.0.0.5`): produces two
  subdirectories and two (redundant) backups. This is not data loss and is the operator's
  choice; documented as a one-liner, not guarded against.
- **IPv6 literal hosts**: supported via the bracketed `[addr]:port` syntax; parsing,
  canonicalization, and the disjoint+injective path encoding are specified in the **IPv6
  host support** section below. Unbracketed hostnames/IPv4 are unaffected.

## Testing strategy

- **Unit (collision):** two specs sharing `dbname` across different hosts produce two
  distinct paths and two intact files at `DUMP_KEEP=1` (the regression test the review
  flagged as missing — `retention_test.go` currently has no multi-host/same-dbname case).
- **Unit (port disambiguation):** same host, two ports → two subdirectories.
- **Unit (default port):** `host:dbname:user` and `host:5432:dbname:user` resolve to the
  same subdirectory/path.
- **Unit (retention isolation):** `DUMP_KEEP>1` with two servers sharing a dbname prunes
  each server independently; neither affects the other's count or files.
- **Unit (subdir creation failure):** a non-writable `DUMP_DIR` (or a file where the subdir
  should be) yields a per-DB non-OK Result, not a panic or a cross-DB effect.
- **Unit (traversal):** confirm validated inputs cannot escape `DUMP_DIR` (covered by the
  existing spec tests; add a path-construction assertion).
- **Unit (length guard):** an over-long host is marked `Invalid` (Requirement 8).
- **Verify-before-replace within subdir:** the existing known-good-preserved tests, retargeted
  to the subdirectory path, still hold.

## Out of scope

- Migrating or relocating pre-existing flat artifacts (operator-owned).
- Changes to the `DB_SPECS` grammar beyond what IPv6 host support requires (the bracketed
  `[addr]:port` form) and the length guard — e.g. no new auth fields, no connection-URI
  form, no IPv6 zone-id (`%scope`) support.
- A configurable name scheme / opt-out flag. The subdirectory layout is adopted uniformly;
  if a back-compat opt-out is later desired it can be a follow-up, but it is not part of
  this feature (it would re-introduce a divergent code path).

## IPv6 host support

IPv6 hosts are supported in `DB_SPECS` via the standard bracketed form. The change is
confined to `internal/spec` (parsing/validation) and the `serverDir` encoding above; the
`pg` boundary needs no change (verified — see "Connectivity" below).

### Grammar and parsing (`internal/spec.parseOne`)

`DB_SPECS` tokens are `:`-split today, which mis-tokenizes a raw IPv6 literal. Add a
bracket-aware branch at the front of `parseOne`:

```go
if strings.HasPrefix(tok, "[") {
    close := strings.Index(tok, "]")
    if close < 0 { s.Invalid = "host: missing ']' for bracketed IPv6"; return s }
    addr := tok[1:close]                 // e.g. "2001:db8::1"
    rest := tok[close+1:]                // ":port:dbname:user" or ":dbname:user"
    if !strings.HasPrefix(rest, ":") { s.Invalid = "host: expected ':' after ']'"; return s }
    fields := strings.Split(rest[1:], ":") // dbname/user/port contain no ':', so this is safe
    // len 2 => [dbname,user] (default port); len 3 => [port,dbname,user]; else invalid
    ...
    ip := net.ParseIP(addr)
    if ip == nil { s.Invalid = "host: invalid IP literal in brackets"; return s }
    s.Host = ip.String()                 // CANONICAL form (lowercase, compressed)
    // continue with the existing port/dbname/user validation
}
```

- The trailing `:dbname:user` (and optional `:port`) is unambiguous because `dbname`, `user`,
  and `port` contain no `:` (existing grammars), so splitting the post-`]` remainder on `:`
  is safe.
- **Validation = `net.ParseIP`.** It accepts only valid IP syntax (hex, `:`, `::`, and the
  embedded-IPv4 dotted form), and rejects `..`, `/`, shell metacharacters, and zone-id
  forms (`fe80::1%eth0` → nil). So it is both the syntactic and the safety check; no
  separate charset pass is needed for the bracketed branch.
- **Canonicalization.** `s.Host = net.ParseIP(addr).String()` stores the canonical address,
  so spelling variants collapse to one identity — the duplicate key and the artifact path
  then treat them as the same server (avoiding accidental redundant backups).
- **Bracketed IPv4** (`[192.0.2.1]`) parses to a 4-byte IP; store its canonical string and,
  because it contains no `:`, it flows through the normal `<host>_<port>` directory (same as
  the unbracketed IPv4 it is equivalent to). Unbracketed hostnames/IPv4 are untouched.

### Path encoding (collision-safety across host types)

`serverDir` (above) renders an IPv6 host as `@<addr-with-colons-as-dashes>_<port>`. Why this
is safe — the same three-part bar h-f3 demands:

- **Filesystem-portable:** the `:` (illegal on Windows, a footgun for `host:path` tools) is
  replaced by `-`; the `@` and `-` are safe on every target filesystem.
- **Injective over IPv6:** `net.IP.String()` is canonical (one string per address), and its
  alphabet is `{0-9, a-f, :}` (plus `.` only for embedded IPv4). `:`→`-` is a relabel of a
  symbol (`:`) to one absent from the source (`-`), so it cannot merge two distinct
  canonical strings. The trailing `_<port>` is unambiguous because the encoded address
  contains no `_`.
- **Disjoint from hostname/IPv4 dirs:** IPv6 dirs start with `@`, which the host grammar
  (`[a-zA-Z0-9_.-]`) can never produce — so an IPv6 server's directory can never equal a
  hostname/IPv4 server's directory. This is the cross-type guarantee: without the `@` marker
  an encoded IPv6 like `2001-db8--1` could collide with a literal hostname `2001-db8--1`,
  re-introducing h-f3 in IPv6 form.

Example: `[2001:db8::1]:5432:authentik:ro` → `/dumps/@2001-db8--1_5432/authentik.dump`.

### Connectivity (no `pg` boundary change)

Verified against `internal/pg/pg.go`:

- `Probe` dials with `net.JoinHostPort(c.Host, strconv.Itoa(c.Port))`, which brackets an
  IPv6 `c.Host` automatically (`[2001:db8::1]:5432`). Already correct.
- `Dump` and `Probe` pass the **bare** address to `--host=`/`-h` — the form libpq expects
  for an IPv6 host param (brackets are only for connection URIs). Since `s.Host` (hence
  `Conn.Host`) is the bare canonical address, this works unchanged.

So IPv6 reachability requires **no** change to `pg.go`.

### Duplicate key

The dedup key `s.Host + ":" + itoa(port) + ":" + dbname` stays unambiguous with IPv6
present: `port` and `dbname` contain no `:`, so the last two `:`-delimited segments are
always port and dbname and the IPv6 colons remain within the host segment. No change needed.

### `.pgpass` caveat

libpq resolves passwords from a `:`-delimited `.pgpass` keyed on `host:port:db:user`. An
IPv6 host's colons must be backslash-escaped in `.pgpass` (`2001\:db8\:\:1:5432:db:user:pw`),
or `PGPASSWORD` used instead. This is an operator/.pgpass concern, not pg-autodump code; it
is documented in the README. (A typical deployment uses `PGPASSWORD`, so it is unaffected.)

### Testing (IPv6)

- Parse: `[2001:db8::1]:5432:db:user` and the port-omitted `[2001:db8::1]:db:user` parse to
  the canonical host + correct fields; missing `]`, non-IP brackets, and zone-id forms are
  `invalid` with clear details.
- Canonicalization: `[2001:DB8::1]` and `[2001:db8:0:0:0:0:0:1]` dedup to one spec / one path.
- Encoding: an IPv6 server and a hostname that would dash-encode to the same string produce
  **different** directories (the `@` disjointness test); two distinct IPv6 addresses produce
  different directories.
- Regression: unbracketed hostname/IPv4 specs are byte-for-byte unchanged.
- `FuzzParseSpecs` exercises bracketed input (no panic; every token yields exactly one spec).
