# Contributing to pg-autodump

Notes on the layout, local workflow, and conventions specific to this
repo. Most of it is standard Go, but a few things are essential and
easy to trip over.

## What this is

An on-demand PostgreSQL logical-backup sidecar. It connects to each
database in `DB_SPECS` **over the network** as a least-privilege role,
runs `pg_dump --format=custom`, verifies each dump, and writes it
atomically under a per-server subdirectory of a shared volume. There is
no shell, no CGI, and no Docker socket; the runtime surfaces are the HTTP
server (`POST /dump`, `GET /healthz`), the `/dumps`
volume, and a read-only `.pgpass`.

The repo, image, Go module, and binary are all `pg-autodump`
(`module github.com/cplieger/pg-autodump`).

## Package layout

`cmd/pg-autodump/main.go` is the composition root: the only place that
reads config, builds the slog handler, wires dependencies (including the
cross-process cycle lock, a `scheduler.Exclusive` under `/tmp/pg-autodump`
shared by the server and one-shot runs), and decides fatal-vs-recover. It
dispatches `serve` (default) / `run` / `health` / `trigger`.
The real work lives under `internal/`:

- `internal/config`: the single `os.Getenv` site. Every tunable is a
  typed `Config` field. No database password is ever a field (libpq reads
  `.pgpass`); the lone secret it holds is the `AUTH_TOKEN` bearer
  (`AuthToken`), which is never logged.
- `internal/spec`: the single `DB_SPECS` validation path
  (`host[:port]:dbname:user`). Fuzzed. Nothing else validates specs.
- `internal/pg`: the os/exec boundary over `pg_dump` / `pg_restore` /
  `psql`. Implements `dump.PGTool`. Every call is context-bounded and
  returns `ErrNoDeadline` for a deadline-less context.
- `internal/dump`: the core (orchestrator, bounded worker pool,
  single-flight guard, verify-before-replace, crash-orphan temp reclaim,
  the result/reason taxonomy). It defines the narrow interface it consumes
  (`PGTool`) so it is testable against fakes with no network/daemon.
- `internal/httpapi`: routes, handlers, bearer auth, the shared `Trigger`.
- `internal/obs`: the startup preflight (binaries/dir/specs) that
  decides the health-marker state.

If you add a new `internal/<pkg>/`, the `Dockerfile` builder must
`COPY internal/ internal/`; there is no per-repo path list.

## Conventions and gotchas

- **Verify-before-replace is the core safety property.** A dump
  lands in its final path only after it passes the
  non-empty and `pg_restore --list` (TOC) checks, via an `atomicfile`
  pending file (same-filesystem atomic rename + dir fsync). Any failure
  discards the temp and leaves the prior dump byte-for-byte intact. Do
  not short-circuit it.
- **No secret in argv or logs.** Credentials flow only through `.pgpass`
  (`PGPASSFILE`) or the libpq-owned `PGPASSWORD` env; `pg_dump` runs with
  `--no-password`. Identifiers are passed as `--dbname=`/`--username=`
  long options so a value can never be read as a flag. Never build a
  shell string.
- **Single validation path.** All `DB_SPECS` rules live in
  `internal/spec`; keep the char class tight (`[a-zA-Z0-9_-]`, host also
  `.`; no leading `-`, no `..`, no control chars).
- **Classification is structural.** `dump.classify` maps exit code,
  `ctx.Err()`, and a typed `FailKind` from the boundary to a `Reason`.
  Never `strings.Contains(err.Error(), …)`.
- **Health is the last cycle's verdict.** `dump.CycleOK` (at least one
  database configured, every one dumped and verified) is the one verdict
  behind the health marker, the last-run record, the `POST /dump` status
  and the `run` exit code; `dump.Orchestrator.Run` writes it, so every
  entry point (timer, `POST /dump`, an exec'd `run`) moves health through
  one site, and a `run` whose preflight fails removes the marker too. The
  server writes through `health.Latch`, so a cycle finishing during the
  shutdown drain cannot restore healthy. Boot health is `bootHealthy` in
  `cmd/pg-autodump`: the preflight, plus the last-run record read at boot
  by `readStartupRecord` (fresh success in built-in mode; not a recorded
  failure in external mode; a record the server cannot rewrite is
  ignored). A cycle a shutdown cut short records nothing and leaves the
  marker alone, so the drain's latch owns that state. `bootHealth` and
  `beginDrain` are Go-tested in `cmd/pg-autodump/health_test.go`; the
  resident server's own sink is proven by the image smoke's `trigger`
  cycle, so keep that cycle going through the server.
- **The image smoke test is the only place `pg_dump` runs for real.**
  The Go tests drive `internal/pg` against shell fakes staged on `PATH` and
  `internal/dump` against a fake `PGTool`; the shipped
  `pg_dump`/`pg_restore`/`psql` binaries run only in the image smoke test.
  `tests/image-smoke.conf`, run by CI through the
  synced `tests/image-smoke.sh` (do not edit the harness here), boots the
  assembled image against a real PostgreSQL sidecar, runs one cycle through
  `pg-autodump run`, reads the seeded row back out of the archive with the
  shipped `pg_restore`, then runs two negative controls: a `pg_dump` that
  exits non-zero on a reachable server (a row-level-security table the
  backup role may not read) must be reported as a failed cycle with its
  `pg_error` line and make the container unhealthy, with a clean cycle
  through the resident server (`pg-autodump trigger`) restoring health, and
  a hidden `libpq` must fail the run's preflight and make the container
  unhealthy again. A change to what a failed cycle reports needs the
  matching assertion there. It drives a live server rather than an
  unreachable spec because boot health proves only that the client binaries
  run: measured on v2.2.3 with `/usr/lib/libpq.so.5.18` moved aside,
  `pg_dump --version` exits 127 and every dump fails while the container
  reports healthy in 5s, so a health-only smoke passed that image.
- **No per-host serialization in the pool.** The cap
  (`DUMP_CONCURRENCY`) is the only knob; serializing per host would force
  the common one-server case serial.
- **Logs are UTC.** The `slogx` library (its `UTCTime` `ReplaceAttr`) forces every
  record's timestamp to UTC, so the container needs no `TZ` and the binary
  embeds no `time/tzdata`.

## Running checks locally

From the repo root:

```sh
go build ./...
go test -race ./...
golangci-lint run        # v2 also enforces gofumpt + gci; a format drift fails
golangci-lint fmt        # apply formatting fixes
govulncheck ./...
```

The spec parser is fuzzed; run the target directly when touching it:

```sh
go test ./internal/spec -run '^$' -fuzz '^FuzzParse$' -fuzztime 30s
```

BuildKit checks are errors (`# check=error=true`), so an image build also
surfaces Dockerfile lint failures and confirms the `postgresqlNN-client`
apk exists in the pinned Alpine:

```sh
docker build -t pg-autodump .
```

Run the image smoke test against that build; it needs a Docker daemon that
can pull the PostgreSQL sidecar image and create a network:

```sh
sh tests/image-smoke.sh pg-autodump
```

CI runs the same battery via the shared `cplieger/ci` reusable workflow
(`.github/workflows/ci.yaml`; synced, do not edit). Fuzz targets run on
the weekly schedule; a counterexample opens an issue, fixed by committing
the minimized seed under `internal/spec/testdata/fuzz/FuzzParse/`.

## Commits and PRs

Commits follow [Conventional Commits](https://www.conventionalcommits.org/)
and are parsed by git-cliff to build the release changelog, so the
subject becomes a public changelog line. Use `feat:`, `fix:`, `sec:` for
release-worthy changes; `docs:`/`chore:`/`refactor:`/`test:` for the
rest. Branch from `main` and open a PR.

## Conduct & security

By participating you agree to the
[Code of Conduct](https://github.com/cplieger/.github/blob/main/CODE_OF_CONDUCT.md).
Report security issues through the
[security policy](https://github.com/cplieger/.github/blob/main/SECURITY.md),
never in a public issue.
