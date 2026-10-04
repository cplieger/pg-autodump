# pg-autodump

[![Image Size](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/pg-autodump/badges/size.json)](https://github.com/cplieger/pg-autodump/pkgs/container/pg-autodump) [![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-blue)](https://github.com/cplieger/pg-autodump/pkgs/container/pg-autodump) [![base: Alpine](https://img.shields.io/badge/base-Alpine-0D597F?logo=alpinelinux)](https://github.com/cplieger/pg-autodump/blob/main/Dockerfile) [![Mutation](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/pg-autodump/badges/mutation.json)](https://github.com/cplieger/pg-autodump/issues?q=label%3Agremlins-tracker) [![SBOM](https://img.shields.io/badge/SBOM-SPDX-1D4ED8)](https://github.com/cplieger/pg-autodump/releases)

<!-- hub-overview BEGIN -->
pg-autodump writes a checked backup file of each of your PostgreSQL databases into a folder that your backup tool already collects. It leaves encryption and off-site copies to that tool, such as Kopia, Restic, Borg or rsync.

## What it does

pg-autodump keeps a fresh, readable dump of every database you list, ready for your backup tool to pick up.

- Dumps each database with PostgreSQL's own `pg_dump` every 24 hours, or whenever you ask.
- Replaces a dump only after `pg_restore` can read the new one, so a failed run keeps the last good file.
- Keeps the 7 newest dumps of each database and deletes older ones.
- Shows a failed backup as an unhealthy container until every database dumps cleanly again.

## Who it is for

pg-autodump is built for self-hosters who run PostgreSQL behind apps such as Authentik, Paperless or Immich and already back up a folder. It connects over the network as a read-only role and needs no root and no Docker socket. It dumps the databases you list, one file each, without roles or other server-wide settings.

You need a PostgreSQL server from version 9.2 to 18 that the container can reach, and a role allowed to read every table.

One other tool suits a different need. Consider [pgBackRest](https://pgbackrest.org/) if you want full, differential and incremental backups of a whole server, which it can store in S3, Azure or GCS.

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
    stop_grace_period: 320s  # SHUTDOWN_TIMEOUT (default 315s) plus the 5s cancel budget, so a dump finishes or unwinds on stop

    environment:
      # First create a login role with pg_read_all_data and CONNECT on each database.
      # The README quick start has the SQL.
      DB_SPECS: "mydb-host:5432:myapp:dbdumper_ro"  # host:port:database:role, space-separated, never localhost

    ports:
      - "127.0.0.1:9847:9847"  # POST /dump starts a backup, so keep it on loopback or set AUTH_TOKEN

    volumes:
      # Put one "host:port:database:role:password" line per database in secrets/.pgpass,
      # then run "chmod 600 secrets/.pgpass", or libpq ignores it.
      - "./secrets/.pgpass:/secrets/.pgpass:ro"
      - "./dumps:/dumps"  # point your backup tool at this folder
```

1. On your PostgreSQL server, create the backup role and let it connect to each database you want dumped:

   ```sql
   CREATE ROLE dbdumper_ro LOGIN PASSWORD 'choose-a-strong-password';
   GRANT pg_read_all_data TO dbdumper_ro;
   GRANT CONNECT ON DATABASE myapp TO dbdumper_ro;
   ```

   `pg_read_all_data` needs PostgreSQL 14 or later. On an older server, use the grants in [Security](docs/security.md#the-backup-role).

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

The built-in timer dumps every 24 hours. To dump right now, run `curl -fsS -X POST http://127.0.0.1:9847/dump` on the Docker host, or `docker exec pg-autodump pg-autodump trigger`. The answer has one line per database and status `200` when every database dumped. Status `500` means at least one failed, and `429` means a dump is already running.

To let cron, a systemd timer or another scheduler decide when dumps run, set `DUMP_INTERVAL=off` and call `trigger` from it. `pg-autodump run`, started as a container of its own, does one dump and exits, with exit code `0` only when every database dumped. [Configuration](docs/configuration.md#scheduling) covers both.

## Restoring a dump

Each dump sits under `dumps/<host>_<port>/`, named `<database>.<UTC time>.dump`, for example `dumps/mydb-host_5432/myapp.20261002T020000Z.dump`. The files are in `pg_dump`'s custom format, so restore one into an empty database with `pg_restore --dbname=myapp <file>`. Create the roles the database uses first, because the dumps do not carry them.

## Configuration reference

Settings are environment variables, read once at start, so recreate the container after a change. A malformed value falls back to its default with a warning in the log. The one exception is a `DUMP_DIR` with a `..` part, which stops the container from starting.

| Variable | Description | Default |
| --- | --- | --- |
| `DB_SPECS` | Databases to dump, as space-separated `host[:port]:database:role` entries. The port defaults to 5432 | required |
| `DUMP_INTERVAL` | Time between built-in dumps, such as `12h`. `off` hands scheduling to your own trigger | `24h` |
| `DUMP_KEEP` | Dumps kept per database. `1` keeps one `<database>.dump` that each run overwrites | `7` |
| `DUMP_TIMEOUT` | Seconds one database may take to dump, at least 10 | `300` |
| `DUMP_CONCURRENCY` | Databases dumped at the same time | `2` |
| `AUTH_TOKEN` | When set, `POST /dump` needs the header `Authorization: Bearer <token>` | _(unset)_ |
| `LISTEN_ADDR` | Address the HTTP server listens on | `:9847` |
| `SHUTDOWN_TIMEOUT` | How long a stop waits for a running dump, such as `315s`. Keep `stop_grace_period` about 5s longer than this | `DUMP_TIMEOUT` plus 15s |
| `PGPASSFILE` | Path of the password file inside the container | `/secrets/.pgpass` |
| `DUMP_DIR` | Folder the dumps go to inside the container. A path with a `..` part stops the container from starting | `/dumps` |
| `DUMP_FREE_KB_WARN` | Log a warning when free space for dumps is below this many KB at the start of a run. `0` turns it off | `1048576` |

Instead of a `.pgpass` file, you can set `PGPASSWORD`, which libpq uses for every database. [Configuration](docs/configuration.md) explains the `DB_SPECS` rules, IPv6 hosts, file names and timeouts.

| Mount | Description |
| --- | --- |
| `/secrets/.pgpass` | Password file, mode 0600, read-only. Not needed when you set `PGPASSWORD` |
| `/dumps` | The checked dumps, one folder per server, plus a one-line `.pg-autodump-last-run` record |

| Port | Description |
| --- | --- |
| `9847` | `POST /dump` starts a dump, `GET /healthz` reports health |

## Security

The container runs as an ordinary user with no Docker socket. It needs only network access to your databases, the password file and a writable `/dumps` folder. Use a role that can only read, as in the quick start.

Port 9847 starts a dump for anyone who reaches it. Keep it published on `127.0.0.1` as in the example, or set `AUTH_TOKEN`. pg-autodump logs a warning at start when the endpoint is open and listens beyond loopback. Its answers never include `pg_dump` error text, so schema and table names stay in the log.

Passwords stay in `.pgpass` or `PGPASSWORD`, never on a command line or in the log. [Security](docs/security.md) covers the hardened compose settings, the backup role's limits and what the image contains.

## Troubleshooting

The healthcheck runs `pg-autodump health`, which reads a file written after each run. Healthy means the last run dumped and checked every database. Unhealthy means a database failed or a run could not start. It stays unhealthy until a run fully succeeds, and a restart does not clear it.

With the built-in timer, a new container is unhealthy while its first dump runs, unless the last run fully succeeded less than one interval ago. It also turns unhealthy when no run has finished in two intervals, 48 hours by default, plus the time one run may take.

- `dump auth_error` means the role, its password or the database name is wrong, or `.pgpass` is not mode 0600 and owned by the container user.
- `dump connect_error` means the container cannot reach that host and port. Put it on a network that reaches your server.
- `dump version_mismatch` means the server is newer than the PostgreSQL 18 tools in the image.
- `health preconditions not met` naming the dump dir means the container user cannot write to `./dumps`. Repeat quick start step 5.

The image waits 6 minutes before it counts a starting container as unhealthy. With many databases, raise `healthcheck.start_period` in your compose file. [How it works](docs/how-it-works.md#health) has the details.

## Monitoring

pg-autodump has no metrics endpoint. It logs one line per database and one `dump cycle complete` line per run to standard error. [Monitoring and alerts](docs/monitoring.md) lists the log lines and has two Loki alert rules, one for a failed dump and one for a missing completion heartbeat after 26 hours.

## Documentation

- [Configuration](docs/configuration.md) explains every setting, the scheduling modes and the file layout.
- [Security](docs/security.md) has the hardened compose settings, the backup role's limits and what the image contains.
- [How it works](docs/how-it-works.md) shows how a dump is checked and replaced, how runs take turns and what health means.
- [Monitoring and alerts](docs/monitoring.md) lists the log lines and the Loki alert rules.

## Credits

pg-autodump runs `pg_dump`, `pg_restore` and `psql` from [PostgreSQL](https://www.postgresql.org/), and all credit for them goes to the PostgreSQL developers. The way it builds the `pg_dump` command and reads its exit code follows [pg_back](https://github.com/orgrim/pg_back), a dump tool for PostgreSQL.

## Contributing

Issues and pull requests are welcome. Please open an issue first for larger changes. See [CONTRIBUTING.md](CONTRIBUTING.md) for the package layout, the rules the code keeps and the local checks.

## Disclaimer

This project is built with care and follows security best practices, but it is intended for personal / self-hosted use. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude](https://claude.com), [GPT](https://openai.com), and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

Apache-2.0. See [LICENSE](LICENSE).

The image carries the license text of every bundled component under `/usr/share/licenses/`. The Alpine packages in the image ship no license file upstream, so their license texts are kept under `licenses/` in this repository and copied in.

Third-party attributions are in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
