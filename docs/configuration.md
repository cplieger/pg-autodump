# Configuration

This page explains every pg-autodump setting in depth, for readers who want to change more than the quick start sets. The short table is in the README's [configuration reference](../README.md#configuration-reference).

## Where settings live

Settings are environment variables. pg-autodump reads them once at start, so recreate the container after a change. A malformed or out-of-range value falls back to its default, and the container logs a warning at start that names the variable. The one exception is `DUMP_DIR`, described below.

## Databases to dump

`DB_SPECS` lists the databases as space-separated `host[:port]:database:role` entries. The port defaults to 5432. pg-autodump dumps only the databases listed here, and it does not discover the others on a server.

- A host may use letters, digits, `_`, `-` and `.`. A database name and a role may use letters, digits, `_` and `-`.
- No part may start with `-`, contain `..` or contain a control character.
- An IPv6 host uses the bracketed form, `[2001:db8::1]:5432:myapp:dbdumper_ro`, and the port stays optional. A zone ID or anything else that is not an IP address is rejected inside the brackets.
- A host whose folder name would be longer than 255 bytes is rejected.

An invalid entry is reported as `invalid` for that database and skipped, and the other databases still dump. A second entry for the same host, port and database is reported as `duplicate`, and the first one is kept. Either result means the run did not fully succeed, so the container turns unhealthy.

## Passwords

libpq reads passwords from the file `PGPASSFILE` names, `/secrets/.pgpass` by default. Each line is `host:port:database:role:password`. The file must have mode 0600, because libpq ignores a password file that other users can read. The container user must own it.

You can set `PGPASSWORD` instead, which libpq uses for every database. A `.pgpass` file is the better choice when the databases use different passwords, because each line is scoped to one host, database and role.

A `.pgpass` line separates its fields with colons, so an IPv6 host needs its colons escaped with backslashes, as in `2001\:db8\:\:1:5432:myapp:dbdumper_ro:password`. Or use `PGPASSWORD` for that host.

The PostgreSQL tools inherit the container's environment, so other libpq variables such as `PGSSLMODE` apply to every connection.

## Scheduling

pg-autodump runs one dump run at a time, and a run dumps every listed database. A run can start in four ways:

- The built-in timer, every `DUMP_INTERVAL`. The default is `24h`, and any Go duration such as `12h` or `90m` works.
- `POST /dump` on the HTTP port.
- `docker exec pg-autodump pg-autodump trigger`, which sends `POST /dump` from inside the container and sets the `AUTH_TOKEN` header itself.
- `pg-autodump run`, the one-shot mode below.

`DUMP_INTERVAL=off`, `disabled` or `0` turns the timer off and leaves scheduling to your own trigger. A negative value also turns it off, with a warning. Pick one scheduling mode per deployment.

### The startup dump and the last-run record

After every finished run, whatever started it, pg-autodump writes a one-line record to `DUMP_DIR/.pg-autodump-last-run`. The built-in timer reads it at start.

- The first timed dump comes one interval after the previous finished run, so a restart neither adds a dump nor delays the next one.
- At start, pg-autodump dumps right away unless the previous run fully succeeded within one interval.
- Only a run in which every listed database dumped and passed the check counts as a success. After a failed run, every restart dumps again until a run fully succeeds. A dump never replaces the last good file, so a retry costs time and never data.
- A run cut short by a stop records nothing, and the previous record stands. A redeploy during a dump keeps the timer's schedule, and the new container's health follows that previous record. A recent success starts it healthy, and a failed or older record starts it unhealthy while the startup dump runs.
- When `/dumps` is not kept across container recreates, the record is lost and every start dumps once.
- A record the container can read but not rewrite is ignored with a warning. The startup dump then runs, and the container stays unhealthy until it succeeds.

The record is harmless if your backup tool collects it.

### One-shot mode

`pg-autodump run` does exactly one dump run and exits. It suits cron, a systemd timer, a Kubernetes CronJob or Ofelia, where the scheduler owns the timing and reads the exit code:

```sh
docker run --rm \
  -e DB_SPECS="mydb-host:5432:myapp:dbdumper_ro" \
  -v ./secrets/.pgpass:/secrets/.pgpass:ro \
  -v ./dumps:/dumps \
  ghcr.io/cplieger/pg-autodump:latest run
```

- The exit code is `0` when every listed database dumped. It is not `0` when any database failed, or when the PostgreSQL tools cannot run, `/dumps` is not writable or `DB_SPECS` is empty.
- A stop signal during a run stops the `pg_dump` in progress cleanly. That database is reported as `killed`, and the exit code is not `0`.
- `run` opens no HTTP port and ignores `LISTEN_ADDR`, `AUTH_TOKEN`, `DUMP_INTERVAL` and `SHUTDOWN_TIMEOUT`. The image's healthcheck is meant for the resident server and says nothing useful about a container that runs once and exits.
- Runs inside one container never overlap. Separate one-shot containers do not share this lock, so their scheduler must keep them from overlapping. When `run` is started inside a container where a run is already going, it exits `0` at once, and the running process does one more run as soon as the current one ends. The per-database results of that run are in the running process's log. This makes a stray manual `run` safe, and it is not meant for running both modes on a schedule.

`trigger` and `run` behave differently on a busy container. `trigger` gets `429` and exits with an error, while `run` queues its request and exits `0`.

## Retention and file layout

Each database's dump goes into a folder for its server, named `<host>_<port>`, so two databases with the same name on different servers never share a file:

```text
/dumps/
  db1.example.com_5432/myapp.20261002T020000Z.dump
  db2.example.com_5432/myapp.20261002T020000Z.dump    # same database name, different server
  apphost_5433/myapp.20261002T020000Z.dump            # same host, a second server on port 5433
  @2001-db8--1_5432/myapp.20261002T020000Z.dump       # IPv6 host, colons written as dashes, prefixed with @
  .pg-autodump-last-run
```

With `DUMP_KEEP` above 1, the default 7, each run writes `<database>.<UTC time>.dump` and then deletes all but the newest `DUMP_KEEP` of that database in that server's folder. Retention never counts one server's dumps against another's. A failed prune is logged and does not fail the dump.

With `DUMP_KEEP=1`, each run overwrites one `<database>.dump`. Choose this when your backup tool keeps the versions.

pg-autodump never reads, moves or deletes other files at the root of `/dumps`.

## Timeouts and stopping

`DUMP_TIMEOUT` is the number of seconds one database may take, 300 by default and at least 10. A database that takes longer is reported as `timeout`. pg-autodump also sets the server's `statement_timeout` to 60 seconds more than `DUMP_TIMEOUT`, so a dump whose network connection drops is ended on the server too.

`SHUTDOWN_TIMEOUT` is how long a stop waits for a running dump to finish, as a Go duration such as `315s`. It defaults to `DUMP_TIMEOUT` plus 15 seconds, and a value below `DUMP_TIMEOUT` logs a warning because a stop may then cancel a dump. When the time runs out, the dump is cancelled, `pg_dump` is stopped and its unfinished file is removed, which takes up to 5 more seconds. Set the compose `stop_grace_period` to at least `SHUTDOWN_TIMEOUT` plus about 5 seconds.

`DUMP_CONCURRENCY` sets how many databases dump at once, 2 by default. Raise it for many servers or fast storage. Set `1` when one slow backup disk holds `/dumps`.

## Dump folder and disk space

`DUMP_DIR` is the folder inside the container, `/dumps` by default. A value with a `..` path part stops the container at start instead of silently writing somewhere else. A name that only contains dots, such as `/dumps/a..b`, is fine.

`DUMP_FREE_KB_WARN` logs a warning when free space on the dump folder is below that many KB at the start of a run, 1048576 (1 GiB) by default. `0` turns it off. The warning never stops a run.

## HTTP endpoints

The server listens on `LISTEN_ADDR`, `:9847` by default.

- `POST /dump` runs every dump and answers with one `host/database: result` line per database, such as `mydb-host/myapp: ok (4823104 bytes)`. For a `pg_error`, `truncated` or `other` failure the line holds only that word, and the full `pg_dump` or `pg_restore` error text goes to the log.
- `200` means every database dumped. `500` means at least one failed or no database is configured, and the body then reads `no databases configured`. `429` means a run is already in progress. `401` means `AUTH_TOKEN` is set and the request had no valid `Authorization: Bearer <token>` header.
- After repeated bad tokens, `POST /dump` answers `429` with a `Retry-After` header for a while. A valid token is never held back.
- `GET /healthz` answers `200 ok` or `503 unhealthy` from the same file the Docker healthcheck reads.

Without `AUTH_TOKEN` the endpoint is open, which is fine on loopback or a private network. pg-autodump logs a warning at start when it is open and listens beyond loopback.
