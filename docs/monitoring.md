# Monitoring and alerts

This page lists the log lines pg-autodump writes and gives two Loki alert rules built on them, for readers who want to know when backups fail or stop.

## Log lines

pg-autodump has no metrics endpoint. It writes structured `key=value` logs to standard error, with UTC timestamps, so the container log is the whole record.

- One line per database per run. A successful dump logs `msg="dump ok"` at `level=INFO`. A failure logs `msg="dump <result>"` at `level=ERROR`, for example `msg="dump auth_error"`. Each line carries `host`, `db`, `reason`, `bytes` and `duration_s`, plus `detail` on a failure. [How it works](how-it-works.md#results) lists every result.
- `invalid`, `duplicate`, `skipped` and `killed` log at `level=WARN`. A `killed` dump is a stop you asked for, so it is not an error.
- One `msg="dump cycle complete"` line at the end of every run, whatever started it, with `total`, `ok` and `failed` counts.
- At start, a `msg="pg-autodump listening"` line with the number of databases, the interval and whether the container started healthy.

## Alerting

Ship the container's logs to Loki and evaluate these rules with [Loki's ruler](https://grafana.com/docs/loki/latest/alert/). Grafana Alloy's Docker log discovery ships them with no extra configuration. Firing alerts go through your Alertmanager like any Prometheus alert.

The two rules cover the two ways backups go wrong. `PgAutodumpDumpFailed` fires when a dump ran and reported an error. `PgAutodumpCycleMissing` fires when no `dump cycle complete` line has arrived for 26 hours. The container or its timer may have stopped, or every trigger may fail before a dump starts. The log stream may also have been renamed or stopped shipping.

The rules read the container's own log stream. That stream covers the resident server, where the timer, `POST /dump` and `trigger` all dump, and a one-shot container whose command is `run`. A `run` started with `docker exec` logs to that exec session instead, so in that setup alert on your scheduler's job result.

```yaml
groups:
  - name: pg-autodump
    rules:
      - alert: PgAutodumpDumpFailed
        expr: |
          sum by (container) (count_over_time(
            {container="pg-autodump"} |= `level=ERROR` |= `reason=` [15m]
          )) > 0
        for: 0m
        labels:
          severity: critical
        annotations:
          summary: "pg-autodump: database dump failed"
          description: >
            A database dump failed, so that database's backup is now stale.
            The last good .dump is kept. Check the reason and detail fields
            of the level=ERROR log line.
      - alert: PgAutodumpCycleMissing
        expr: |
          absent_over_time(
            {container="pg-autodump"} |= `dump cycle complete` [26h]
          )
        for: 0m
        labels:
          severity: warning
        annotations:
          summary: "pg-autodump: no dump cycle complete line in 26h"
          description: >
            No "dump cycle complete" line arrived in 26h, so backups may have
            stopped. Check that the container and its timer or trigger are
            running, and that its logs still reach Loki.
```

When `PgAutodumpDumpFailed` fires, the `reason` field names the cause:

- `connect_error`, `auth_error` and `pg_error` point to a wrong setting or a database that is down.
- `timeout` means the dump took longer than `DUMP_TIMEOUT`.
- `truncated` and `empty` mean a bad or partial dump.
- `other` means an environment fault, such as a PostgreSQL tool the image can no longer run.

A dump cancelled by a stop logs `reason=killed` at `level=WARN`, so it does not trip `PgAutodumpDumpFailed`.

The 26-hour window of `PgAutodumpCycleMissing` fits the built-in 24-hour timer. That timer keeps its timing across restarts, so two `dump cycle complete` lines are at most one `DUMP_INTERVAL` plus one run's time apart. With an external daily trigger, 26 hours also catches a missed day.

Thresholds and the `severity` labels are starting points. Set the `[26h]` window to the longest normal gap, about one `DUMP_INTERVAL` plus a run's time under the built-in timer, or your trigger's cadence plus some margin under an external scheduler. Change the `container` selector to the label your log collector sets, and route by whatever labels your Alertmanager uses.

If separate monitoring already alerts when this scheduled job produces no result, the absence rule repeats that coverage. Keep whichever signal you trust more.
