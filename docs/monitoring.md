# Monitoring and alerts

This page lists the log lines pg-autodump writes and the alert rules it ships, for readers who want to know when backups fail or stop.

## Log lines

pg-autodump writes structured `key=value` logs to standard error, with UTC timestamps. It also serves Prometheus metrics at `GET /metrics` on `LISTEN_ADDR`, with no login.

- One line per database per run. A successful dump logs `msg="dump ok"` at `level=INFO`. A failure logs `msg="dump <result>"` at `level=ERROR`, for example `msg="dump auth_error"`. Each line carries `host`, `db`, `reason`, `bytes` and `duration_s`, plus `detail` on a failure. [How it works](how-it-works.md#results) lists every result.
- `invalid`, `duplicate`, `skipped` and `killed` log at `level=WARN`. A `killed` dump is a stop you asked for, so it is not an error.
- One `msg="dump cycle complete"` line at the end of every run, whatever started it, with `total`, `ok` and `failed` counts.
- At start, a `msg="pg-autodump listening"` line with the number of databases, the interval and whether the container started healthy.

## Alerting

The rules ship as files in [`alerts/`](../alerts/). Load one of the two, not both: both fire for the same fault.

- [`alerts/promql.yaml`](../alerts/promql.yaml) is for Prometheus, Mimir, vmalert or another ruler that reads Prometheus rules. It reads `GET /metrics` on `LISTEN_ADDR`, scraped as job `pg-autodump`. [`alerts/promql_test.yaml`](../alerts/promql_test.yaml) holds its `promtool test rules` fixtures.
- [`alerts/logql.yaml`](../alerts/logql.yaml) is for Loki's ruler. It reads the `dump cycle complete` line. Save it in Loki's rules folder, as [Loading an app's alert rules](https://github.com/cplieger/docs/blob/main/docs/monitoring.md#loading-an-apps-alert-rules) shows.

| Alert | Fires when | Severity | File |
| --- | --- | --- | --- |
| `PgAutodumpDumpFailed` | A database's latest dump failed. It holds until a cycle dumps that database, or with the log rules until a cycle has no failure. | critical | both |
| `PgAutodumpDumpStale` | A database's newest verified dump is older than 50 hours. | warning | `promql.yaml` |
| `PgAutodumpDown` | No scrape of pg-autodump has succeeded for 5 minutes. | warning | `promql.yaml` |
| `PgAutodumpCycleMissing` | No `dump cycle complete` line arrived in 50 hours. | warning | `logql.yaml` |

If you copied the two rules from an earlier version of this page, delete them and load one of the files instead.

The log rules read the container's own log stream. That stream covers the resident server, where the timer, `POST /dump` and `trigger` all dump, and a one-shot container whose command is `run`. A `run` started with `docker exec` logs to that exec session instead, so in that setup alert on your scheduler's job result.

When `PgAutodumpDumpFailed` fires, the `reason` field names the cause:

- `connect_error`, `auth_error` and `pg_error` point to a wrong setting or a database that is down.
- `timeout` means the dump took longer than `DUMP_TIMEOUT`.
- `truncated` and `empty` mean a bad or partial dump.
- `other` means an environment fault, such as a PostgreSQL tool the image can no longer run.

A dump cancelled by a stop logs `reason=killed` at `level=WARN`, so it does not trip `PgAutodumpDumpFailed`.

The 50-hour windows of `PgAutodumpDumpStale` and `PgAutodumpCycleMissing` are two runs of the default 24-hour `DUMP_INTERVAL` plus time for the run, so one late run does not page. If you change `DUMP_INTERVAL`, or trigger dumps on another cadence, set both windows to two intervals plus the time a run takes.

Thresholds and the `severity` labels are starting points. In `alerts/logql.yaml`, change the `container` selector to the label your log collector sets. Route by whatever labels your Alertmanager uses.

If separate monitoring already alerts when this scheduled job produces no result, the absence rule repeats that coverage. Keep whichever signal you trust more.
