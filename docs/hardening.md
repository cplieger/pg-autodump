# Security

This page covers how pg-autodump limits what it can reach, the hardened compose settings, the backup role's limits and what the image contains, for readers who want to tighten a deployment.

## What it can reach

pg-autodump connects to your databases over the network as an ordinary PostgreSQL client. It needs no Docker socket and no root. It needs only network access to the databases, a read-only `.pgpass` or `PGPASSWORD`, and a writable `/dumps`. If the container is compromised, an attacker can read the databases through the backup role and change the files under `/dumps`. The container has no Docker socket and no root, so give it only the network access and mounts it needs.

- Passwords live in `.pgpass` or `PGPASSWORD`, never on a command line or in the log. `pg_dump` runs with `--no-password`, so it never waits for a prompt.
- `DB_SPECS` is checked once at start. Each value is passed as a long option such as `--dbname=` or `--username=`, so a value can never be read as a flag, and no shell is ever started.
- Anyone who reaches `POST /dump` can at most write another dump made by the read-only role into the volume. Keep the port on loopback or a private network, or set `AUTH_TOKEN`.
- When `AUTH_TOKEN` is empty and `LISTEN_ADDR` is not loopback, pg-autodump logs a warning at start.
- The `POST /dump` answer never carries `pg_dump` or `pg_restore` error text, so an open endpoint shows no schema or table names.

## Hardened compose settings

The image runs as a non-root user. Add these lines to the service in `compose.yaml`. [Hardening a compose file](https://github.com/cplieger/docs/blob/main/docs/hardening.md) explains each setting.

```yaml
    read_only: true
    cap_drop: ["ALL"]
    security_opt: ["no-new-privileges:true"]
    tmpfs:
      - "/tmp:size=16m,mode=1777"
```

The `/tmp` tmpfs is required with `read_only`, because the health file and the run lock live there. Mode 1777 lets the non-root user write to it.

## The backup role

`pg_read_all_data`, in PostgreSQL 14 and later, grants read access to every ordinary table, view and sequence, which is what a dump needs. It has these limits:

- Large objects in `pg_largeobject` are not covered by `pg_read_all_data` ([BUG #19379](https://www.postgresql.org/message-id/r5a3aqlrrqen2snktdmx5tjeoakp3hmbektlqmeqhij3fqqez4@zmx3bdscipny)). A database that uses them needs a role that owns them, a superuser, or the server setting `lo_compat_privileges`.
- Tables with row-level security need the `BYPASSRLS` attribute on the role. A superuser grants it, and it is more than read-only. Without it, `pg_dump` fails on those tables with `pg_error`.
- The role belongs to the whole server and can only read. Application updates do not change it, but a fresh data directory drops it, so create it again after one.
- A dump holds `ACCESS SHARE` locks on the tables it reads, so schedule dumps outside heavy schema changes and migrations.
- On PostgreSQL 13 and older, grant `SELECT` on all tables and `USAGE` on each schema instead.

## What the image contains

The image is Alpine Linux with the PostgreSQL 18 client package, which provides `pg_dump`, `pg_restore`, `psql` and libpq. `pg_dump` dumps servers back to version 9.2 and only up to its own major version, so this image dumps PostgreSQL 9.2 to 18. A newer server is reported as `version_mismatch` with a clear message.

The client tools need a C library, which is why the image is Alpine rather than distroless. They stay the Alpine package rather than a separate build, so each image rebuild picks up the current package revision.

`tini` runs as process 1. It is the upstream static binary at a pinned version, checked against a SHA256 sum for each architecture, and the build fails when the sum does not match.

[Renovate](https://github.com/renovatebot/renovate) updates the dependencies, and the base images are pinned by digest. Each image carries a signed SBOM and the build provenance Docker records. [Reading the software bill of materials](https://github.com/cplieger/docs/blob/main/docs/images.md#reading-the-software-bill-of-materials) and [Checking with the GitHub CLI](https://github.com/cplieger/docs/blob/main/docs/images.md#checking-with-the-github-cli) show how to check the SBOM.

| Dependency | Source |
| --- | --- |
| golang | [Go](https://hub.docker.com/_/golang) |
| alpine | [Alpine](https://hub.docker.com/_/alpine) |
| postgresql18-client | [PostgreSQL](https://www.postgresql.org/) |
| tini | [GitHub](https://github.com/krallin/tini) |
| github.com/cplieger/atomicfile | [GitHub](https://github.com/cplieger/atomicfile) |
| github.com/cplieger/envx | [GitHub](https://github.com/cplieger/envx) |
| github.com/cplieger/health | [GitHub](https://github.com/cplieger/health) |
| github.com/cplieger/keyenc | [GitHub](https://github.com/cplieger/keyenc) |
| github.com/cplieger/pathinside | [GitHub](https://github.com/cplieger/pathinside) |
| github.com/cplieger/scheduler | [GitHub](https://github.com/cplieger/scheduler) |
| github.com/cplieger/slogx | [GitHub](https://github.com/cplieger/slogx) |
| github.com/cplieger/webhttp | [GitHub](https://github.com/cplieger/webhttp) |
