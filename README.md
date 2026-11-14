# pg-autodump

[![Image Size](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/pg-autodump/badges/size.json)](https://github.com/cplieger/pg-autodump/pkgs/container/pg-autodump) [![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-blue)](https://github.com/cplieger/pg-autodump/pkgs/container/pg-autodump) [![base: Alpine](https://img.shields.io/badge/base-Alpine-0D597F?logo=alpinelinux)](https://github.com/cplieger/pg-autodump/blob/main/Dockerfile) [![Mutation](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/pg-autodump/badges/mutation.json)](https://github.com/cplieger/pg-autodump/issues?q=label%3Agremlins-tracker) [![SBOM](https://img.shields.io/badge/SBOM-SPDX-1D4ED8)](https://github.com/cplieger/pg-autodump/releases)

<!-- hub-overview BEGIN -->
pg-autodump writes a checked backup file of each PostgreSQL database you list into a folder your backup tool already collects. It leaves encryption and off-site copies to that tool, such as Kopia, Restic, Borg or rsync.

## What it does

pg-autodump keeps a fresh, readable dump of every database you list for your backup tool.

- Dumps each database with PostgreSQL's `pg_dump`, compressed, every 24 hours or whenever you ask.
- Replaces a dump only after `pg_restore` can read the new file's table of contents, so a failed run keeps the last good file.
- Keeps the 7 newest dumps of each database, and deletes old ones only once a new one is in place.
- Tells you when a backup fails, through Docker health, Prometheus metrics and alert rules for Prometheus or Loki.

## Who it is for

pg-autodump is built for self-hosters who run PostgreSQL behind apps such as Authentik, Paperless or Immich and already back up a folder. It connects over the network as a read-only role. It dumps only the databases you list, without the server's roles or other server-wide settings. It sends no notifications itself, so alerts come from Prometheus, Loki or a tool watching Docker health.

You need a PostgreSQL server, version 9.2 to 18, that the container can reach, and a role that can read every table.

Consider [pgBackRest](https://pgbackrest.org/) if you want full, differential and incremental backups of a whole server in S3, Azure, GCS or SFTP storage.

pg-autodump is free software under the Apache-2.0 license.
<!-- hub-overview END -->

## Quick start

The image is on GitHub Container Registry and Docker Hub, for `amd64` and `arm64`. This is the [`compose.yaml`](compose.yaml) in this repository.

```yaml
services:
  pg-autodump:
    image: ghcr.io/cplieger/pg-autodump:latest
    container_name: pg-autodump
    restart: unless-stopped
    # Run "mkdir secrets dumps". Once secrets/.pgpass exists, run "sudo chown 1000:1000 secrets/.pgpass dumps",
    # or every dump fails. If .env sets PUID and PGID, use those numbers.
    user: "${PUID:-1000}:${PGID:-1000}"
    stop_grace_period: 320s  # SHUTDOWN_TIMEOUT (default 315s) plus 5s, so a running dump finishes or cleans up on stop

    environment:
      # First create a login role with pg_read_all_data and CONNECT on each database.
      # The README quick start has the SQL.
      DB_SPECS: "mydb-host:5432:myapp:dbdumper_ro"  # host:port:database:role, space-separated, never localhost

    ports:
      # Anyone who reaches this port can start a dump. Keep it on 127.0.0.1,
      # or set AUTH_TOKEN before you publish it to a network, for example for a scraper.
      - "127.0.0.1:9472:9472"

    volumes:
      # Put one "host:port:database:role:password" line per database in secrets/.pgpass,
      # then run "chmod 600 secrets/.pgpass", or the PostgreSQL tools ignore it.
      - "./secrets/.pgpass:/secrets/.pgpass:ro"
      - "./dumps:/dumps"  # point your backup tool at this folder
```

1. On your PostgreSQL server, create the backup role and let it connect to each database you want dumped:

   ```sql
   CREATE ROLE dbdumper_ro LOGIN PASSWORD 'choose-a-strong-password';
   GRANT pg_read_all_data TO dbdumper_ro;
   GRANT CONNECT ON DATABASE myapp TO dbdumper_ro;
   ```

   `pg_read_all_data` needs PostgreSQL 14 or later. On an older server, use the grants in [Security](docs/hardening.md#the-backup-role).

2. In the folder that holds `compose.yaml`, run `mkdir secrets dumps`.
3. Create `secrets/.pgpass` with one `host:port:database:role:password` line per database:

   ```text
   mydb-host:5432:myapp:dbdumper_ro:choose-a-strong-password
   ```

4. Run `chmod 600 secrets/.pgpass`.
5. Run `sudo chown 1000:1000 secrets/.pgpass dumps`, so the container can read the password file and write the dumps. If `.env` sets `PUID` and `PGID`, use those numbers instead.
6. In `compose.yaml`, set `DB_SPECS` to one `host:port:database:role` entry per database, separated by spaces. The host is the address the container reaches your PostgreSQL server at, such as its LAN address, or its container name when both share a Docker network. It is never `localhost`, which inside the container is the container itself.
7. Run `docker compose up -d`.

pg-autodump dumps every database right after the first start. Run `docker logs pg-autodump`. You should see `msg="dump cycle complete"` with `failed=0`. A `dump auth_error` line means the role, its password or the database name in `.pgpass` or `DB_SPECS` is wrong.

## Starting a dump yourself

The built-in timer dumps every 24 hours. To dump right now, run `curl -fsS -X POST http://127.0.0.1:9472/dump` on the Docker host, or `docker exec pg-autodump pg-autodump trigger`. The answer has one line per database and status `200` when every database dumped. Status `500` means at least one failed, and `429` means a dump is already running.

To let cron, a systemd timer or another scheduler decide when dumps run, set `DUMP_INTERVAL=off` and call `trigger` from it. `pg-autodump run`, started as a container of its own, does one dump and exits, with exit code `0` only when every database dumped. [Configuration](docs/configuration.md#scheduling) covers both.

## Restoring a dump

Each dump sits under `dumps/<host>_<port>/`, named `<database>.<UTC time>.dump`, for example `dumps/mydb-host_5432/myapp.20261002T020000Z.dump`. The file is a compressed archive in `pg_dump`'s custom format, which `pg_restore` reads. Create the roles the database uses first, because the dumps do not carry them. Then restore the file into an empty database with `pg_restore --dbname=myapp <file>`. [Configuration](docs/configuration.md#retention-and-file-layout) shows the folder layout.

## Configuration reference

Settings are environment variables, read once at start, so recreate the container after a change. A malformed number or duration falls back to its default, with a warning in the log that names the variable. A `DUMP_DIR` with a `..` part stops the container from starting instead.

| Variable | Description | Default |
| --- | --- | --- |
| `DB_SPECS` | Databases to dump, as space-separated `host[:port]:database:role` entries. The port defaults to 5432 | required |
| `DUMP_INTERVAL` | Time between built-in dumps, such as `12h`. `off` hands scheduling to your own trigger | `24h` |
| `DUMP_KEEP` | Dumps kept per database. With `1`, each database has a single `<database>.dump` file that each run replaces | `7` |
| `DUMP_TIMEOUT` | Seconds one database may take to dump, at least 10 | `300` |
| `DUMP_CONCURRENCY` | Databases dumped at the same time | `2` |
| `AUTH_TOKEN` | When set, `POST /dump` needs the header `Authorization: Bearer <token>` | _(unset)_ |
| `LISTEN_ADDR` | Address the HTTP server listens on | `:9472` |
| `SHUTDOWN_TIMEOUT` | How long a stop waits for a running dump, such as `315s`. Keep `stop_grace_period` about 5s longer than this | `DUMP_TIMEOUT` plus 15s |
| `PGPASSFILE` | Path of the password file inside the container | `/secrets/.pgpass` |
| `DUMP_DIR` | Folder the dumps go to inside the container. A path with a `..` part stops the container from starting | `/dumps` |
| `DUMP_FREE_KB_WARN` | Log a warning when free space for dumps is below this many KB at the start of a run. `0` turns it off | `1048576` |

Instead of a `.pgpass` file, you can set `PGPASSWORD`, which the PostgreSQL tools use for every database. Other PostgreSQL client variables, such as `PGSSLMODE` to require TLS, apply to every connection too. [Configuration](docs/configuration.md) explains the `DB_SPECS` rules, IPv6 hosts, file names and timeouts.

| Mount | Description |
| --- | --- |
| `/secrets/.pgpass` | Password file, mode 0600, read-only. Not needed when you set `PGPASSWORD` |
| `/dumps` | The checked dumps, one folder per server, plus a one-line `.pg-autodump-last-run` record |

| Port | Description |
| --- | --- |
| `9472` | `POST /dump` starts a dump, `GET /healthz` reports health, `GET /metrics` serves Prometheus metrics |

## Security

The container runs as an ordinary user with no Docker socket. It needs only network access to your databases, the password file and a writable `/dumps` folder. Use a role that can only read, as in the quick start.

Port 9472 starts a dump for anyone who reaches it. Keep it on `127.0.0.1` as in the example, or set `AUTH_TOKEN`. Without a token, pg-autodump logs a warning at every start, even with the example's `127.0.0.1` mapping, because it cannot see how Docker publishes the port. Its answers never include `pg_dump` error text, so schema and table names stay in the log. `/healthz` and `/metrics` need no token, and `/metrics` shows database and host names, so publish the port only to your network.

Passwords stay in `.pgpass` or `PGPASSWORD`, never on a command line or in the log. [Security](docs/hardening.md) covers the hardened compose settings, the backup role's limits and what the image contains.

## Troubleshooting

The healthcheck runs `pg-autodump health`, which asks `/healthz` inside the container. Healthy means the last run dumped and checked every database. Unhealthy means a database failed or a run could not start. It lasts until a run fully succeeds, and a restart does not clear it. With the built-in timer, a new container is unhealthy while its first dump runs, unless a run fully succeeded less than one interval ago. It also turns unhealthy when no run has finished in two intervals, 48 hours by default, plus one run's longest time.

- `dump auth_error` means the role, its password or the database name is wrong, or `.pgpass` is not mode 0600 and owned by the container user.
- `dump connect_error` means the container cannot reach that host and port. Give it a network that reaches your server.
- `dump version_mismatch` means the server is newer than the PostgreSQL 18 tools in the image.
- `dump timeout` means the database took longer than `DUMP_TIMEOUT`, so raise it.
- `health preconditions not met` naming the dump dir means the container user cannot write to `./dumps`. Repeat quick start step 5.

The image's healthcheck gives a starting container 6 minutes before a failed check counts. With many databases, raise `start_period` under `healthcheck:` in compose. [How it works](docs/how-it-works.md#health) has the details.

## Monitoring

pg-autodump serves Prometheus metrics at `/metrics` on port 9472 and logs one `dump cycle complete` line per finished run. Alert rules for Prometheus and for Loki ship in [`alerts/`](alerts/). Load one of the two files. A Grafana dashboard of every database's dumps and the free space ships as [`grafana-dashboard.json`](grafana-dashboard.json) and needs Grafana 13.2 or newer. [Monitoring and alerts](docs/monitoring.md) lists the rules. The image has a Docker healthcheck, and [Knowing the app is alive](docs/monitoring.md#knowing-the-app-is-alive) says what it proves and who is told.

## Documentation

- [Configuration](docs/configuration.md) explains every setting, the scheduling modes and the file layout.
- [Security](docs/hardening.md) has the hardened compose settings, the backup role's limits and what the image contains.
- [How it works](docs/how-it-works.md) shows how a dump is checked and replaced, how runs take turns and what health means.
- [Monitoring and alerts](docs/monitoring.md) lists the metrics, the dashboard, the log lines, both rule files and who tells you backups stopped.

## Credits

pg-autodump runs `pg_dump`, `pg_restore` and `psql` from [PostgreSQL](https://www.postgresql.org/), and all credit for them goes to the PostgreSQL developers. The way it builds the `pg_dump` command and reads its exit code follows [pg_back](https://github.com/orgrim/pg_back), a dump tool for PostgreSQL.

## Contributing

Issues and pull requests are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md).

## Disclaimer

This project is built with care and follows security best practices, but it is intended for personal / self-hosted use. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI assistance. The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

Apache-2.0. See [LICENSE](LICENSE).

The image carries the license text of every bundled component under `/usr/share/licenses/`. The Alpine packages in the image ship no license file upstream, so their license texts are kept under `licenses/` in this repository and copied in.

Third-party attributions are in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
