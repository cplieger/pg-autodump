# Security assurance case — pg-autodump

This extends the shared
[default assurance case](https://github.com/cplieger/.github/blob/main/assurance-case.md)
with the threat model specific to `pg-autodump`. Read that first.

## What this is

A non-root network backup sidecar: on `POST /dump` it runs `pg_dump` over TCP
against each configured database, verifies each dump, and writes atomic `.dump`
files. It exposes `/healthz` and Prometheus `/metrics`. It holds database
credentials and accepts an HTTP trigger, so trigger-surface and command-safety
are the core concerns.

## Top-level claim

pg-autodump produces verified, atomic backups when triggered, without exposing
database credentials, without a shell-injection surface, and without an
unauthenticated path to arbitrary command execution.

## Threats and mitigations

| Threat                               | Mitigation                                                                                                  | Evidence                          |
| ------------------------------------ | ----------------------------------------------------------------------------------------------------------- | --------------------------------- |
| Command injection via DB name/params | `pg_dump` invoked with an argument vector (no shell); inputs validated                                      | `internal/httpapi`, source review |
| Untrusted HTTP trigger abuse         | the trigger only runs the _configured_ dump set; it takes no arbitrary command/target from the request body | `httpapi.go`, handler tests       |
| Silent backup corruption             | each dump is verified after creation; atomic write (temp→rename)                                            | dump + verify tests               |
| Credential exposure                  | DB credentials come from config/env, never logged; redaction on error paths                                 | source review                     |
| Privilege escalation at runtime      | runs as non-root; distroless; `/healthz` for the Docker probe                                               | Dockerfile, healthcheck           |
| Resource exhaustion                  | bounded request handling; fuzz on the HTTP surface                                                          | fuzz target, `httpapi` tests      |

## Residual risks

- Network exposure of the trigger endpoint is a deployment concern; in a
  self-hosted deployment it is reachable only on the internal network, and the dump set is
  fixed by configuration so a caller cannot redirect it.
- Backup confidentiality depends on the permissions of the volume the `.dump`
  files land on (a deployment concern).

Report vulnerabilities privately per
[SECURITY.md](https://github.com/cplieger/.github/blob/main/SECURITY.md).
