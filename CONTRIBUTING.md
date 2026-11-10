# Contributing to pg-autodump

The [shared rules](https://github.com/cplieger/.github/blob/main/CONTRIBUTING.md) for commits, releases, synced files and checks apply here.

## Rules

- A change to how pg-autodump runs `pg_dump`, `pg_restore` or `psql`, or to what a failed cycle reports, needs the matching assertion in `tests/image-smoke.conf`. The Go tests use fake clients, so they pass while the shipped binaries fail.
- A package you `apk add` in the Dockerfile's `base` stage needs a `licenses/<origin>/` folder for each new origin it or its dependencies bring, plus their lines in `licenses/APK-MANIFEST`. No check notices a missing folder.
- A new PostgreSQL client major changes `postgresqlNN-client` in the Dockerfile, `SMOKE_PG_IMAGE` in `tests/image-smoke.conf`, `licenses/postgresqlNN/` with its manifest lines, and the major that `README.md` and `docs/hardening.md` name. When the client and smoke-server majors differ, the smoke test no longer proves the image dumps the newest server.

## Dashboard

- `grafana-dashboard.json` reads series names and labels as text. A test in `internal/obs` fails when a panel reads a series or a label pg-autodump does not serve, when a rule in `alerts/promql.yaml` has no Needs attention tile, or when the dump age colour stops matching twice the default `DUMP_INTERVAL`. Update the dashboard with any rename.
- Save a dashboard change made in the Grafana UI with Export, then Export as code, choosing the V2 Resource model.
- A change to `grafana-dashboard.json` takes `fix:` or `feat:`. Under `chore:` or `docs:` it replaces the dashboard's OCI artifact at the current version tags, and the GitHub Release keeps the old file.

## Checks

When you change `internal/spec`, fuzz both of its targets. Pull-request CI runs only their seed inputs:

```sh
go test ./internal/spec -run '^$' -fuzz '^FuzzParse$' -fuzztime 30s
go test ./internal/spec -run '^$' -fuzz '^FuzzParseSpecsDedupeKeyInjective$' -fuzztime 30s
```
