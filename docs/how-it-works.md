# How it works

This page describes what pg-autodump does during a dump run and what its health means, for readers who want to judge how it fails before they rely on it.

## One dump, step by step

For each database, pg-autodump does four things in order:

1. It opens a TCP connection to the server, then runs one query with `psql` as the backup role. A failed connection is `connect_error`. A connection that works followed by a failed query is `auth_error`, which also covers a database that does not exist. A server newer than the image's PostgreSQL client is `version_mismatch`, reported before any dump starts.
2. It runs `pg_dump --format=custom` over the network into a temporary file in that server's folder. The custom format is compressed by `pg_dump` by default, and `pg_restore` reads it.
3. It checks that the file is not empty and that `pg_restore --list` can read its table of contents. A failed check is `empty` or `truncated`.
4. Only then does it rename the file into place and flush the folder to disk. The previous good dump stays untouched by any failure before this step.

The check in step 3 proves the archive is readable. It does not restore the data, so a restore test of your own is still worth doing now and then.

Dump files are created owner-only, mode 0600, and each server folder is owner-only too. pg-autodump refuses a server folder that is a symbolic link or that another user owns. When a filesystem setting widened its own folder, it narrows the folder again. A dump folder that cannot keep the owner-only mode fails the start check with a message saying so.

## Results

Every database gets one result per run, from a fixed list:

| Result | Meaning |
| --- | --- |
| `ok` | Dumped, checked and in place |
| `empty`, `truncated` | The new dump was empty or unreadable, so the old one stays |
| `timeout` | The database took longer than `DUMP_TIMEOUT` |
| `killed` | A stop cancelled the dump |
| `pg_error` | `pg_dump` exited with an error |
| `connect_error`, `auth_error`, `version_mismatch` | The step 1 check failed, so no dump ran |
| `other` | An environment fault, such as a PostgreSQL tool the image can no longer run |
| `invalid`, `duplicate` | The `DB_SPECS` entry was rejected |
| `mkdir_failed`, `rename_failed` | The server folder or the final rename failed |
| `skipped` | A stop came before this database's turn |

A run fully succeeds only when every result is `ok`.

## Running databases in parallel

`DUMP_CONCURRENCY` databases dump at the same time, 2 by default. pg-autodump does not serialize dumps per server, because the common setup is one server with many databases. The setting is the only control over the load a run puts on your servers.

## One run at a time

Runs never overlap inside one container. A lock file under `/tmp/pg-autodump` covers both the server and any `pg-autodump run` started inside the same container. The kernel releases the lock when its process ends, so a crashed run never leaves it stuck.

- A timer tick, `POST /dump` or `trigger` that finds a run in progress is skipped, and HTTP callers get `429`.
- A `pg-autodump run` that finds a run in progress queues one more run and exits `0`. The running process does that run when its current one ends, so the request is never lost. Further requests during the same run collapse into that one.

Each run starts by deleting temporary files that a crashed run left in the server folders. It removes only files in its own temporary-file format, never a dump.

## Stopping

On a stop signal, the container turns unhealthy, stops accepting HTTP requests and waits up to `SHUTDOWN_TIMEOUT` for the running dump to finish. When that time runs out, it cancels the dump, stops `pg_dump` and removes the unfinished file, which takes up to 5 more seconds. Each `pg_dump` runs in its own process group, so a stop signal sent to the container never reaches a dump directly.

## Health

The healthcheck runs `pg-autodump health`, a check of a file under `/tmp` that needs no shell, `curl` or open port. Every finished run writes its verdict to that file, whether the timer, `POST /dump`, `trigger` or a `run` inside the container started it.

- Healthy means the most recent run fully succeeded.
- Unhealthy means a database failed or a run could not start. A run cannot start when a PostgreSQL tool cannot run, `/dumps` is not writable or `DB_SPECS` is empty. It stays unhealthy until a run fully succeeds.
- A run cut short by a stop leaves the previous verdict in place.

The log says why. Look for the per-database `level=ERROR` line or the `level=ERROR` start-check line. After a restart, the `pg-autodump listening` line and a `booting unhealthy` warning name the recorded last run. A caller also sees it as a `500` answer or a non-zero `run` exit code.

At start, health follows the [last-run record](configuration.md#the-startup-dump-and-the-last-run-record):

- With the built-in timer, the container starts healthy only when the record shows a fully successful run within one interval. Otherwise it is unhealthy while the startup dump runs.
- With `DUMP_INTERVAL=off`, it starts unhealthy when the last recorded run failed, and healthy otherwise. A new deployment has nothing to report until its first trigger.

The image sets a 6-minute start period, one default `DUMP_TIMEOUT` round plus some margin. A healthy check inside that window ends it early, and a container still dumping is reported unhealthy only after it. Size `healthcheck.start_period` in your compose file for the time your first dump takes.

With the built-in timer, `pg-autodump health` also fails when the file is older than two intervals plus one run's longest time, which catches a timer that stopped firing. `GET /healthz` reports only whether the file is there.
