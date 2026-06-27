# Contributing to pg-autodump

Notes on the layout, local workflow, and conventions specific to this
repo. Most of it is standard Go, but a few things are load-bearing and
easy to trip over.

## What this is

An on-demand PostgreSQL logical-backup sidecar. It connects to each
database in `DB_SPECS` **over the network** as a least-privilege role,
runs `pg_dump --format=custom`, verifies each dump, and atomically
replaces the previous `<dbname>.dump` in a shared volume. There is no
shell, no CGI, and no Docker socket — the runtime surfaces are the HTTP
server (`POST /dump`, `GET /healthz`), the `/dumps`
volume, and a read-only `.pgpass`.

The repo, image, Go module, and binary are all `pg-autodump`
(`module github.com/cplieger/pg-autodump`).

## Package layout

`cmd/pg-autodump/main.go` is the composition root — the only place that
reads config, builds the slog handler, wires dependencies, and decides
fatal-vs-recover. It dispatches `serve` (default) / `health` / `trigger`.
The real work lives under `internal/`:

- `internal/config` — the single `os.Getenv` site. Every tunable is a
  typed `Config` field. No database password is ever a field (libpq reads
  `.pgpass`); the lone secret it holds is the `AUTH_TOKEN` bearer
  (`AuthToken`), which is never logged.
- `internal/spec` — the single `DB_SPECS` validation path
  (`host[:port]:dbname:user`). Fuzzed. Nothing else validates specs.
- `internal/pg` — the os/exec boundary over `pg_dump` / `pg_restore` /
  `psql`. Implements `dump.PGTool`. Every call is context-bounded and
  returns `ErrNoDeadline` for a deadline-less context.
- `internal/dump` — the core: orchestrator, bounded worker pool,
  single-flight guard, verify-before-replace, the result/reason taxonomy.
  It defines the narrow interface it consumes (`PGTool`) so
  it is testable against fakes with no network/daemon.
- `internal/httpapi` — routes, handlers, bearer auth, the shared `Trigger`.
- `internal/obs` — the startup preflight (binaries/dir/specs) that
  decides the health-marker state.

If you add a new `internal/<pkg>/`, the `Dockerfile` builder must
`COPY internal/ internal/` — there is no per-repo path list.

## Conventions and gotchas

- **Verify-before-replace is the core safety property.** A dump
  overwrites the previous `<dbname>.dump` only after it passes the
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
- **Liveness, not readiness.** Health checks binaries/dir/specs, never
  per-host DB reachability, so a down database does not flap the
  container.
- **No per-host serialization in the pool.** The cap
  (`DUMP_CONCURRENCY`) is the only knob; serializing per host would force
  the common one-server case serial.

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
go test ./internal/spec -run '^$' -fuzz '^FuzzParseSpecs$' -fuzztime 30s
```

BuildKit checks are errors (`# check=error=true`), so an image build also
surfaces Dockerfile lint failures and confirms the `postgresqlNN-client`
apk exists in the pinned Alpine:

```sh
docker build -t pg-autodump .
```

CI runs the same battery via the shared `cplieger/ci` reusable workflow
(`.github/workflows/ci.yaml` — synced, do not edit). Fuzz targets run on
the weekly schedule; a counterexample opens an issue, fixed by committing
the minimized seed under `internal/spec/testdata/fuzz/FuzzParseSpecs/`.

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
