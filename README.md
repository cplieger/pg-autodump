# pg-autodump

[![Image Size](https://ghcr-badge.egpl.dev/cplieger/pg-autodump/size)](https://github.com/cplieger/pg-autodump/pkgs/container/pg-autodump)
![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-blue)
![base: Alpine](https://img.shields.io/badge/base-Alpine-0D597F?logo=alpinelinux)
[![Go Report Card](https://goreportcard.com/badge/github.com/cplieger/pg-autodump)](https://goreportcard.com/report/github.com/cplieger/pg-autodump)
[![Test coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/pg-autodump/badges/coverage.json)](https://github.com/cplieger/pg-autodump/actions/workflows/coverage.yml)
[![Mutation](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/pg-autodump/badges/mutation.json)](https://github.com/cplieger/pg-autodump/issues?q=label%3Agremlins-tracker)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/13215/badge)](https://www.bestpractices.dev/projects/13215)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/cplieger/pg-autodump/badge)](https://scorecard.dev/viewer/?uri=github.com/cplieger/pg-autodump)
[![SBOM](https://img.shields.io/badge/SBOM-SPDX-1D4ED8)](https://github.com/cplieger/pg-autodump/releases)

On-demand PostgreSQL logical-backup sidecar. Trigger it, and it writes a verified dump per database for your real backup tool to collect.

## What it does

pg-autodump runs `pg_dump` (custom format) against every database in `DB_SPECS`,
verifies each dump with `pg_restore --list`, and writes it atomically into a
shared volume under a per-server `<host>_<port>/` subdirectory, keeping the
newest `DUMP_KEEP` copies per database (7 by default).
It does its one job well and delegates the heavy lifting: no compression,
encryption, or off-site sync — your backup tool (Kopia, Restic, Borg, rsync)
already does those, and points at the `/dumps` volume. If the collector also
versions your backups, set `DUMP_KEEP=1` to keep a single stable `<dbname>.dump`
and hand retention to it.

It runs as an ordinary **unprivileged** user, connects to Postgres **over the
network** (the PostgreSQL wire protocol, not HTTP), and never touches the Docker
socket or runs as root. A truncated or failed dump can never overwrite a
known-good backup.

### Why this design

- **Unprivileged and socket-less** — non-root, `cap_drop: [ALL]`, `read_only`. A compromise reads databases through a least-privilege role; it cannot reach the host. No Docker socket, so no root-equivalent surface.
- **Verify before replace** — each dump stages to a temp file, passes a non-empty and `pg_restore --list` (TOC) check, then atomically renames into place. The last known-good dump survives any failure (`atomicfile` adds the dir-fsync a plain `mv` lacks).
- **Bounded parallelism** — `DUMP_CONCURRENCY` dumps databases concurrently with no per-host serialization, so the common one-server-many-DBs case is not forced serial. One knob, safe default.
- **Built-in retention** — keeps the newest `DUMP_KEEP` timestamped dumps per database (7 by default), pruning older ones after each successful run, so it works as a self-contained incremental backup out of the box. Set `DUMP_KEEP=1` to instead keep a single stable `<dbname>.dump` and delegate versioning to your backup tool.
- **Standard surface** — `POST /dump`, `GET /healthz`. Trigger by the built-in daily timer (default), over HTTP, or `docker exec ... pg-autodump trigger`.

## Quick start

The image is published to both GHCR (`ghcr.io/cplieger/pg-autodump`) and Docker Hub (`cplieger/pg-autodump`) — identical contents, use whichever you prefer.

1. Create a least-privilege backup role in each database:

   ```sql
   CREATE ROLE dbdumper_ro LOGIN PASSWORD 'choose-a-strong-password';
   GRANT pg_read_all_data TO dbdumper_ro;          -- PostgreSQL 14+
   GRANT CONNECT ON DATABASE myapp TO dbdumper_ro;
   ```

2. Create a `.pgpass` (mode **0600**, or libpq silently ignores it). One line
   per `host:port:dbname:user`:

   ```text
   mydb-host:5432:myapp:dbdumper_ro:choose-a-strong-password
   ```

3. Run it (see [`compose.yaml`](compose.yaml) for the full example):

   ```yaml
   services:
     pg-autodump:
       image: ghcr.io/cplieger/pg-autodump:latest
       container_name: pg-autodump
       restart: unless-stopped
       read_only: true
       cap_drop: ["ALL"]
       security_opt: ["no-new-privileges:true"]
       environment:
         DB_SPECS: "mydb-host:5432:myapp:dbdumper_ro"
       ports:
         - "127.0.0.1:9847:9847"
       volumes:
         - "./secrets/.pgpass:/secrets/.pgpass:ro"   # mode 0600
         - "./dumps:/dumps"
       tmpfs:
         - "/tmp:size=16m,mode=1777"   # 1777 so the non-root user can write the health marker
   ```

4. Trigger a backup:

   ```sh
   curl -fsS -X POST http://127.0.0.1:9847/dump
   # or: docker exec pg-autodump pg-autodump trigger
   ```

## Configuration reference

### Environment variables

| Variable            | Description                                                                                                                                                                                                                                                                                         | Default            | Required |
| ------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------ | -------- |
| `DB_SPECS`          | Space-separated `host[:port]:dbname:user` tuples (port defaults to 5432). Ids are `[a-zA-Z0-9_-]` (host also allows `.`), no leading `-`, no `..`, no control chars. IPv6 literal hosts use the bracketed form `[2001:db8::1][:port]:dbname:user`. Invalid entries are reported per-DB and skipped. | -                  | Yes      |
| `PGPASSFILE`        | Path to a read-only `.pgpass` (mode 0600). `PGPASSWORD` is also honoured by libpq but `.pgpass` is preferred (scoped per host/db/user).                                                                                                                                                             | `/secrets/.pgpass` | No       |
| `DUMP_DIR`          | Output directory; each database's dump lands under a per-server `<host>_<port>/` subdirectory (see [On-disk layout](#on-disk-layout)). A value containing `..` is **fatal** — startup aborts rather than silently relocate backups to the default.                                                  | `/dumps`           | No       |
| `DUMP_TIMEOUT`      | Per-dump seconds (min 10).                                                                                                                                                                                                                                                                          | `300`              | No       |
| `DUMP_CONCURRENCY`  | Parallel dumps. Raise for many hosts / fast storage; set `1` for a single slow backup volume.                                                                                                                                                                                                       | `2`                | No       |
| `DUMP_INTERVAL`     | Built-in timer cadence (Go duration). On startup it runs one dump only when no existing dump is newer than one interval, so a deployment that restarts faster than its interval is never starved of backups. `off` / `disabled` / `0` hand scheduling to an external trigger.                       | `24h`              | No       |
| `DUMP_KEEP`         | Retained dumps per database. `>1` (default 7) writes timestamped `<dbname>.<UTC>.dump` files and prunes to the N newest. `1` writes a single stable `<dbname>.dump`, overwritten each run (delegate versioning to your backup tool).                                                                | `7`                | No       |
| `DUMP_FREE_KB_WARN` | Warn when free space on `/dumps` falls below this (KB) at run start. `0` disables.                                                                                                                                                                                                                  | `1048576`          | No       |
| `AUTH_TOKEN`        | When set, `/dump` requires `Authorization: Bearer <token>`. Empty = open (fine on a private network / loopback); pg-autodump logs a startup warning when it is empty **and** `LISTEN_ADDR` is non-loopback.                                                                                         | `""`               | No       |
| `LISTEN_ADDR`       | HTTP listen address.                                                                                                                                                                                                                                                                                | `:9847`            | No       |
| `SHUTDOWN_GRACE`    | Drain budget on SIGTERM. Set compose `stop_grace_period` >= this + ~5s (a cancelled in-flight dump gets a short extra window to reap pg_dump and clear its staged temp).                                                                                                                            | `DUMP_TIMEOUT+15s` | No       |

> **IPv6 hosts.** Use the bracketed form in `DB_SPECS` (`[2001:db8::1]:5432:db:user`; the port may be omitted). libpq's `.pgpass` is colon-delimited, so an IPv6 host's colons must be backslash-escaped there (`2001\:db8\:\:1:5432:db:user:pw`) — or use `PGPASSWORD` instead.

### Volumes

| Mount              | Description                                                                                                                                        |
| ------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------- |
| `/secrets/.pgpass` | Read-only `.pgpass` (mode 0600). Optional when `PGPASSWORD` is used.                                                                               |
| `/dumps`           | Output directory; verified dumps under a per-server `<host>_<port>/` subdirectory (one stable `<dbname>.dump`, or `DUMP_KEEP` timestamped copies). |

### Endpoints

- `POST /dump` — run all dumps; `200` if every database succeeded, `500` if any failed, `429` if a run is already in progress, `401` if `AUTH_TOKEN` is set and the bearer token is missing/wrong. The body has one `host/db: <detail>` line per database; for an execution-tool failure (`pg_error` / `truncated` / `other`) the line carries only the reason word — the raw `pg_dump`/`pg_restore` stderr is logged, not returned, so an open endpoint never discloses schema or object names.
- `GET /healthz` — `200 ok` / `503 unhealthy`. Reflects liveness preconditions (client binaries present, `/dumps` writable, `DB_SPECS` non-empty), **not** per-host database reachability, so a transiently-down database never flips the container unhealthy.

### On-disk layout

Each database's dump is written under a per-server subdirectory of `DUMP_DIR`
named `<host>_<port>`, so two databases that share a name on different servers
never collide on one file:

```text
/dumps/
  db1.example.com_5432/myapp.dump
  db2.example.com_5432/myapp.dump        # same dbname, different host — no clash
  apphost_5433/myapp.dump                # same host, a second instance on :5433
  @2001-db8--1_5432/myapp.dump           # IPv6 host (':' encoded as '-', '@'-prefixed)
```

With `DUMP_KEEP>1` the timestamped `<dbname>.<UTC>.dump` files live inside that
subdirectory and are pruned per server, so retention never counts one server's
dumps against another's. **Upgrading from a flat layout:** dumps previously
written as `<dbname>.dump` at the `DUMP_DIR` root are no longer updated; new
dumps appear under `<host>_<port>/`. A versioning collector (Kopia, etc.) simply
begins a fresh chain at the new paths — pg-autodump never reads, moves, or
deletes the old flat files, so remove them once at your convenience.

## Healthcheck

The Docker `HEALTHCHECK` runs the `pg-autodump health` subcommand — a file-marker probe, so no shell, `curl`, or open port is needed in the image. The main process writes the marker once liveness preconditions hold (the client binaries resolve, `/dumps` is writable, `DB_SPECS` is non-empty); a transiently-down database does **not** flip the container unhealthy, because per-host reachability is a per-dump concern reported in `POST /dump`, not liveness.

## The backup role

`pg_read_all_data` (PostgreSQL 14+) grants read on all ordinary tables, views,
and sequences — exactly what a logical dump needs. Caveats to document for your
databases:

- **Large objects** (`pg_largeobject`) are not covered by `pg_read_all_data` ([BUG #19379](https://www.postgresql.org/message-id/r5a3aqlrrqen2snktdmx5tjeoakp3hmbektlqmeqhij3fqqez4@zmx3bdscipny)). A database using them needs an owning/superuser role, `lo_compat_privileges`, or `--no-large-objects` if blobs are not part of the backup contract.
- **Row-level security** requires `BYPASSRLS` (a superuser-granted attribute, more than read-only) for `pg_dump` to read RLS-protected tables.
- The role is cluster-level and SELECT-only: it cannot modify the database and is unaffected by application updates. A fresh data-directory re-init drops it (recreate it), and a dump holds `ACCESS SHARE` locks, so schedule dumps outside heavy DDL/migration windows.
- On PostgreSQL < 14, grant `SELECT` on all tables plus schema `USAGE` instead.

## Versioning

The image ships the newest PostgreSQL client major it is built with. `pg_dump`
requires the client major to be **>= the server major**, so a client can dump
any server up to its own version. Bump the client major when you upgrade a
server ahead of it; a too-old client is reported per-DB as `version_mismatch`
with a clear message rather than a cryptic pg_dump abort.

## Security

- **No Docker socket, no root.** The container needs only network reach to the databases, a read-only `.pgpass`, and a writable `/dumps`. Runs as a non-root user with `cap_drop: [ALL]` and `read_only`.
- **Credentials never on a command line or in logs.** They live in `.pgpass` (or `PGPASSWORD`); `pg_dump` is invoked with `--no-password` so it never prompts.
- **No shell, explicit argv.** `DB_SPECS` is validated once, and identifiers are passed as long options (`--dbname=`, `--username=`) so a value can never be read as a flag. No shell is ever invoked.
- **Keep it private or set `AUTH_TOKEN`.** A stray trigger can at most write a read-only-role dump to the volume. When the endpoint is open (`AUTH_TOKEN` empty) on a non-loopback `LISTEN_ADDR`, pg-autodump logs a startup warning; and `POST /dump` returns only the reason word for execution-tool failures (never the raw `pg_dump`/`pg_restore` stderr), so an open endpoint discloses no schema or object names.

The CI battery runs govulncheck, golangci-lint (gosec, gocritic), trivy, grype, gitleaks, semgrep, and hadolint on every change; `DB_SPECS` parsing is fuzzed.

## Dependencies

Updated automatically via [Renovate](https://github.com/renovatebot/renovate) and pinned by digest. Builds carry signed SBOMs and provenance attestations verifiable with `gh attestation verify`.

| Dependency                     | Source                                              |
| ------------------------------ | --------------------------------------------------- |
| golang                         | [Go](https://hub.docker.com/_/golang)               |
| alpine                         | [Alpine](https://hub.docker.com/_/alpine)           |
| postgresql18-client            | [PostgreSQL](https://www.postgresql.org/)           |
| tini                           | [GitHub](https://github.com/krallin/tini)           |
| github.com/cplieger/atomicfile | [GitHub](https://github.com/cplieger/atomicfile)    |
| github.com/cplieger/health     | [GitHub](https://github.com/cplieger/health)        |
| pgregory.net/rapid             | [pkg.go.dev](https://pkg.go.dev/pgregory.net/rapid) |

A logical backup is far more than one query — `pg_dump` reconstructs the full schema from the system catalogs, dependency-orders it, streams every table via `COPY`, and emits the custom archive format `pg_restore` reads and verifies. So the `postgresql-client` (`pg_dump`/`pg_restore`/`psql` + `libpq`) is a required, irreducible dependency, and the reason the image is Alpine (libc) rather than distroless.

## Credits

The PostgreSQL client tools `pg_dump`, `pg_restore`, and `psql` are part of [PostgreSQL](https://www.postgresql.org/) (PostgreSQL License). The pg_dump argument construction and exit-code handling were informed by [orgrim/pg_back](https://github.com/orgrim/pg_back) (2-clause BSD), used as a reference only — not vendored.

## Migrating from db-dumper 1.x

pg-autodump is the successor to `db-dumper`, which ran `pg_dump` via `docker exec` over the root-equivalent Docker socket. pg-autodump is a network client instead: no socket, no root. To migrate: remove the socket mount and `user: "0:0"`; rewrite `DB_SPECS` from `container:dbname:user` to `host[:port]:dbname:user`; provide credentials via a read-only `.pgpass` and a least-privilege role (above); triggers move from `GET /cgi-bin/dump` to `POST /dump` (or `pg-autodump trigger`) and health to `GET /healthz`; the healthcheck becomes `["CMD", "pg-autodump", "health"]`; set `stop_grace_period` >= `SHUTDOWN_GRACE`. `CGI_DIR` and `TZ` are gone.

## Contributing

Issues and pull requests are welcome. Please open an issue first for larger
changes so the approach can be discussed before implementation. See
[CONTRIBUTING.md](CONTRIBUTING.md) for the package layout, load-bearing
invariants, and local checks.

## Disclaimer

These images are built with care and follow security best practices, but they are intended for **homelab use**. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude Opus](https://www.anthropic.com/claude) and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

This project is licensed under the [GNU General Public License v3.0](LICENSE).
