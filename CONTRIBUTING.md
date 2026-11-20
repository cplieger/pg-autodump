# Contributing to pg-autodump

The [shared rules](https://github.com/cplieger/.github/blob/main/CONTRIBUTING.md) for commits, releases, synced files and checks apply here.

## Rules

- A change to how pg-autodump runs `pg_dump`, `pg_restore` or `psql`, or to what a failed cycle reports, needs the matching assertion in `tests/image-smoke.conf`. The Go tests use fake clients, so they pass while the shipped binaries fail.
- A package you `apk add` in the Dockerfile's `base` stage needs a `licenses/<origin>/` folder for each new origin it or its dependencies bring, plus their lines in `licenses/APK-MANIFEST`. No check notices a missing folder.
- The PostgreSQL client tools are built from the source release `ARG PG_VERSION` names in the Dockerfile. Renovate moves it with the official `postgres` image's tags, and `scripts/repin-sha.sh` recomputes `PG_SHA256` in the same commit. The smoke test's server major, the license text and the SBOM fragment all follow from that ARG; a new major also changes the server range in `README.md`'s requirements. A library the build links needs its runtime package in the `base` stage's `apk add`, which the build-time `tests/smoke.sh` catches by running each tool.

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
