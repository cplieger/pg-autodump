# Monitoring and alerts

pg-autodump serves a metrics page and ships metric alert rules, so a Prometheus-compatible stack learns when a dump fails, when a database's newest dump gets old and when pg-autodump stops answering. If you run Loki and no metric store, a log rule file gives you the failure alert and an alert for runs that stopped. This page lists what each setup gets, the metrics, the Grafana dashboard, the log lines, the rules and what the healthcheck proves.

## What you get

| You run | You get | You set up |
| --- | --- | --- |
| Prometheus, Mimir or VictoriaMetrics, with Alertmanager (the monitoring guide's starting stack adds Loki) | `PgAutodumpDumpFailed` per database, `PgAutodumpDumpStale` per database, `PgAutodumpDown`, and the [dashboard](#dashboard) of every database's dumps and the free space | A scrape of `/metrics` as job `pg-autodump`, and [`alerts/promql.yaml`](../alerts/promql.yaml) in your ruler |
| Loki and Alertmanager, no metric store | `PgAutodumpDumpFailed` for the container and `PgAutodumpCycleMissing` | [`alerts/logql.yaml`](../alerts/logql.yaml) in Loki's ruler, and a log collector that keeps INFO lines |
| Grafana-managed alerting only | The same rules, imported into Grafana | A Prometheus or Loki data source, and the rules of one file |
| Docker or Unraid only | Docker health, which turns unhealthy while the last run failed or the timer stopped | An uptime prober on `/healthz`, or something that watches Docker health |

## Metrics

The resident server always serves `GET /metrics` on `LISTEN_ADDR`, `:9472` by default, with no token, even when `AUTH_TOKEN` is set. The labels carry your database names and a folder name built from each host and port, so publish the port only to your own network. The `server` label is the per-server folder name under `/dumps`, such as `mydb-host_5432`, and `db` is the database name.

| Series | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `pg_autodump_dumps_total` | counter | `server`, `db`, `result` | Dumps by database and result. `result` is one of the words in [Results](how-it-works.md#results), such as `ok` or `auth_error`. A run a stop cuts short counts its dumps too |
| `pg_autodump_dump_ok` | gauge | `server`, `db` | `1` unless the database's latest dump failed. After a restart it reads the last-run record, so a failed run on record sets every database to `0` until the next run |
| `pg_autodump_dump_bytes` | gauge | `server`, `db` | Size of the latest verified dump in bytes |
| `pg_autodump_dump_duration_seconds` | gauge | `server`, `db` | How long the latest dump attempt took |
| `pg_autodump_newest_dump_timestamp_seconds` | gauge | `server`, `db` | Modification time of the newest verified dump file, read at each scrape. No sample until the database's first dump |
| `pg_autodump_dump_dir_free_bytes` | gauge | none | Bytes free to the container user on the filesystem that holds `DUMP_DIR`, read at each scrape |
| `pg_autodump_last_cycle_timestamp_seconds` | gauge | none | Unix time the latest run ended. After a restart it reads the last-run record |
| `pg_autodump_last_cycle_ok` | gauge | none | `1` when every database dumped and verified in the latest run, else `0` |
| `pg_autodump_cycle_deadline_timestamp_seconds` | gauge | none | Unix time by which the next fully successful run is due: the end of the latest fully successful run, or the start when the last-run record shows none, plus two `DUMP_INTERVAL` runs and the longest time one run may take. Only a fully successful run moves it, so a failed run leaves it where it was. `0` with `DUMP_INTERVAL=off` |
| `pg_autodump_build_info` | gauge | `version` | Always `1`. `version` is the image's release tag |

Every database in `DB_SPECS` has its labelled series from the start: the counters, size and duration at `0`, and `pg_autodump_dump_ok` from the last-run record. A database you remove from `DB_SPECS` has no series after the restart that applies the change.

The page also carries the Go runtime and process families (`go_*`, `process_*`) under the names Go's official Prometheus client uses, so stock Go dashboards work. The bundled [dashboard](#dashboard) shows the `pg_autodump_*` series in Grafana.

A scrape config for the default port:

```yaml
scrape_configs:
  - job_name: pg-autodump
    static_configs:
      - targets: ["pg-autodump:9472"]
```

Keep the job name `pg-autodump`, because `PgAutodumpDown` reads it. A scraper on another host needs the port published to that network. Set `AUTH_TOKEN` too, since anyone who reaches the port can start a dump.

The metrics come from the resident server. A one-shot container whose command is `run` serves no `/metrics`, so in that setup load `alerts/logql.yaml` or alert on your scheduler's job result instead. A `run` started with `docker exec` writes the dump files and the last-run record but not the server's metrics or health. With `DUMP_INTERVAL=off`, `PgAutodumpDumpStale` notices its failures after 50 hours and `PgAutodumpDumpFailed` does not.

## Dashboard

[`grafana-dashboard.json`](../grafana-dashboard.json) imports as the pg-autodump dashboard. It reads the metrics above from a Prometheus data source and needs Grafana 13.2 or newer. When you run more than one container, the pg-autodump variable picks the instance to show. It has one page of three rows:

- Needs attention appears only while `PgAutodumpDumpFailed`, `PgAutodumpDumpStale` or `PgAutodumpDown` fires, with one tile per rule. Each tile reads its rule's `ALERTS` series, which Prometheus and the Mimir ruler store beside your metrics, so the row stays hidden while those rules are not loaded.
- Databases shows whether the latest run succeeded, when it ended, how many databases are listed and the free space, then a table with one row per database: whether its latest dump succeeded, the age of its newest dump, its size, how long the latest attempt took and the dumps that failed in the selected range. The dump age turns yellow at 48 hours, twice the default `DUMP_INTERVAL`. The failure count leaves out `killed` and `skipped`, the results of a stop. After a start, the Duration cell reads `-` until the first run, Size until the first successful dump, and Latest dump until the database has a dump or a failed run.
- Space and trend charts the free space for dumps and each database's dump size over the selected range.

The dashboard is versioned with the image, and the `grafana-dashboard.json` of release `<tag>` matches the metrics that image serves. Each release attaches it with its checksum at `https://github.com/cplieger/pg-autodump/releases/download/<tag>/grafana-dashboard.json`, and each image publish pushes it as the OCI artifact `ghcr.io/cplieger/pg-autodump/dashboard:<tag>`. The file sets `metadata.name` to `pg-autodump`, which Grafana uses as the dashboard UID, so importing a newer copy over the old one updates it in place.

## Logs

pg-autodump writes structured `key=value` logs to standard error, with UTC timestamps, at `INFO` and above.

- One line per database per run. A successful dump logs `msg="dump ok"` at `level=INFO`. A failure logs `msg="dump <result>"` at `level=ERROR`, for example `msg="dump auth_error"`. Each line carries `host`, `db`, `reason`, `bytes` and `duration_s`, plus `detail` on a failure. [How it works](how-it-works.md#results) lists every result.
- `invalid`, `duplicate`, `skipped` and `killed` log at `level=WARN`. A `killed` dump was cancelled by a stop, which is an expected operator action, not a dump failure.
- One `msg="dump cycle complete"` line at `level=INFO` at the end of every run that finishes, whatever started it, with `total`, `ok` and `failed` counts. This is the only line the log rules read.
- A run that a stop cuts short logs `msg="dump cycle cut short"` at `level=WARN` instead, with the same counts. It keeps the previous verdict and writes no `dump cycle complete` line, so it changes no alert in either file.
- At start, a `msg="pg-autodump listening"` line with the number of databases, the interval and whether the container started healthy.

When `PgAutodumpDumpFailed` fires, the `reason` of the database's line, or the `result` label in the metrics, names the cause:

- `connect_error`, `auth_error` and `pg_error` point to a wrong setting or a database that is down.
- `timeout` means the dump took longer than `DUMP_TIMEOUT`.
- `truncated` and `empty` mean a bad or partial dump.
- `other` means an environment fault, such as a PostgreSQL tool the image can no longer run.

## Alerting

The rules ship as files in [`alerts/`](../alerts/). Load `alerts/promql.yaml` or `alerts/logql.yaml`, not both: they hold the same failure alert under the same name, and both would fire for one fault.

- [`alerts/promql.yaml`](../alerts/promql.yaml) is for Prometheus, Mimir, vmalert or another ruler that reads Prometheus rules. It reads the `pg-autodump` scrape job. [`alerts/promql_test.yaml`](../alerts/promql_test.yaml) holds its `promtool test rules` fixtures.
- [`alerts/logql.yaml`](../alerts/logql.yaml) is for Loki's ruler. It reads the `dump cycle complete` line, which is written at `level=INFO`, so if your log collector filters by level, keep INFO lines. Save it in Loki's rules folder, as [Loading an app's alert rules](https://github.com/cplieger/docs/blob/main/docs/monitoring.md#loading-an-apps-alert-rules) shows.

`PgAutodumpDumpStale` and `PgAutodumpDown` exist only in `alerts/promql.yaml`, and `PgAutodumpCycleMissing` only in `alerts/logql.yaml`; each file covers a stopped app and a stopped timer with its own names.

| Alert | Fires when | Severity |
| --- | --- | --- |
| `PgAutodumpDumpFailed` | A database's latest dump failed. In `promql.yaml` it fires per database and holds until a run dumps that database; in `logql.yaml` it holds until a run has no failure. | critical |
| `PgAutodumpDumpStale` | With the built-in timer, no run has fully succeeded by the cycle deadline plus 15 minutes. With `DUMP_INTERVAL=off`, a database's newest verified dump is older than 50 hours (`promql.yaml` only). | warning |
| `PgAutodumpDown` | No scrape of pg-autodump has succeeded for 5 minutes (`promql.yaml` only). | warning |
| `PgAutodumpCycleMissing` | No `dump cycle complete` line arrived in 50 hours (`logql.yaml` only). | warning |

`PgAutodumpDumpStale` has two arms, and `pg_autodump_cycle_deadline_timestamp_seconds` picks one. While the built-in timer runs, the deadline is above `0` and the alert fires 15 minutes after it passes, which covers a stopped timer and runs that keep failing. The app works the deadline out from `DUMP_INTERVAL`, `DUMP_TIMEOUT`, `DUMP_CONCURRENCY` and the number of databases, so this arm has no window to change. With `DUMP_INTERVAL=off` the deadline reads `0`, and the alert fires per database when its newest verified dump is older than 50 hours. A database with no dump yet has no newest-dump series, so a new install stays quiet until its first dump. Load the rule file of the release you run, since an older image serves no deadline and neither arm can fire.

Thresholds and the `severity` labels are starting points. The 50-hour windows are two runs of the default 24-hour `DUMP_INTERVAL` plus time for the run, so one late run does not page. If you trigger dumps on another cadence with `DUMP_INTERVAL=off`, set the window in `alerts/promql.yaml` to two of your runs plus the time a run takes. Set the window in `alerts/logql.yaml` the same way whenever your runs are more than 24 hours apart. In `alerts/logql.yaml`, change the `container` selector to the label your log collector sets, and keep the scrape job name `PgAutodumpDown` reads.

The log rules read the container's own log stream. That stream covers the resident server, where the timer, `POST /dump` and `trigger` all dump, and a one-shot container whose command is `run`. A `run` started with `docker exec` logs to that exec session instead, so in that setup alert on your scheduler's job result.

## Knowing the app is alive

| Route | Method | Port | Login | Answers |
| --- | --- | --- | --- | --- |
| `/healthz` | `GET`, `HEAD` | `LISTEN_ADDR`, `:9472` by default | none | `200 {"status":"ok"}`, or `503 {"status":"unhealthy","reasons":[...]}` |
| `/metrics` | `GET` | the same port | none; keep the port on your network | Prometheus text format |

**What healthy means.** The server answers, and the most recent run fully succeeded: every database dumped and verified. With the built-in timer, that run must also have finished within two intervals plus the longest time one run may take, 48 hours by default plus that time, or the timer has stopped. pg-autodump is unhealthy while the first run after a start is still going, unless the last-run record shows a fully successful run within one interval. It is unhealthy while it shuts down. A `503` answer carries one of two fixed sentences: no fully successful run is on record, which covers a failed run, a first run still going and a shutdown, since the endpoint cannot tell those apart; or the built-in timer has stopped. A database server that is down fails its dump, so it makes the container unhealthy until a run succeeds: the failed run is the fault this app reports. The verdict covers the runs the server does, whether the timer, `POST /dump` or `trigger` started them. A `pg-autodump run` started with `docker exec` reports by its exit code alone and leaves the server's health as it was; the server reads that run's result from the last-run record at its next start.

**Docker health.** The image checks itself every 30 seconds with `pg-autodump health`, which asks `/healthz` on the container's loopback, or on the address `LISTEN_ADDR` names. Docker marks the container unhealthy after 3 failures, and allows 6 minutes after a start. Docker's health status notifies nobody on its own, and Docker does not restart an unhealthy container. A one-shot container whose command is `run` opens no port, so its exit code is its verdict.

**Who is told.**

| You run | A dead app | A failed run or a stopped timer |
| --- | --- | --- |
| Prometheus with this app's rules | `PgAutodumpDown` | `PgAutodumpDumpFailed`, then `PgAutodumpDumpStale` |
| Loki only | `PgAutodumpCycleMissing` | `PgAutodumpDumpFailed`, or `PgAutodumpCycleMissing` |
| Docker or Unraid only | An uptime prober on `/healthz`, or something that watches Docker health | The same |

If separate monitoring already alerts when this scheduled job produces no result, `PgAutodumpCycleMissing` and `PgAutodumpDumpStale` repeat that coverage. Keep whichever signal you trust more.

**Probing from outside.** Check `GET http://<host>:9472/healthz` and pass on status 200 only; do not match the body. Use a 5-second timeout, check every 60 seconds, alert after 3 failures, and wait 6 minutes after a first start, or the time your first dump takes. With `DUMP_INTERVAL=off` a new container reports healthy before its first trigger, because it has nothing to report yet.
