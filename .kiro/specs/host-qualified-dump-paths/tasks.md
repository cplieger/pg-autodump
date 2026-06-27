# Tasks — Host-qualified dump paths

- [x] 1. Add the `serverDir(host, port)` helper in `internal/dump`
  - Implement `serverDir(host string, port int) string` beside `dumpFileName` in `verify.go`: `<host>_<port>` for hostname/IPv4; `@<addr-with-colons-as-dashes>_<port>` when `host` contains `:` (canonical IPv6).
  - Document in the doc comment: port = trailing digit run (injective), and the `@` prefix keeps IPv6 dirs disjoint from the hostname namespace (host grammar forbids `@`).
  - _Requirements: 2.1, 2.3, 3.1, 10.4_

- [x] 2. Add the `mkdir_failed` reason, then route dumps through the per-server subdirectory
  - In `internal/dump/result.go`, add `ReasonMkdirFailed Reason = "mkdir_failed"` beside `ReasonRenameFailed`. Confirm `levelFor` returns `error` for it (via the `default` arm — no change needed) and update any enum-completeness test.
  - In `internal/dump/dump.go` `dumpOne`, compute `dir := filepath.Join(o.dumpDir, serverDir(s.Host, s.Port))` on the post-probe success path.
  - `os.MkdirAll(dir, 0o700)` before staging; on error return a per-DB `Result{Reason: ReasonMkdirFailed, Detail: "cannot create server dir " + dir + ": " + err.Error()}` populated with Host/DBName/ServerVersion/Duration, via `o.finish`.
  - Pass `dir` (not `o.dumpDir`) to both `stageAndReplace` and `pruneOldDumps`.
  - Add the `os`/`path/filepath` imports as needed.
  - _Requirements: 2.1, 2.2, 4.1, 5.1, 5.2, 5.4, 9.3_

- [x] 3. Confirm retention is server-scoped
  - Verify `pruneOldDumps(dir, dbname, keep)` now receives the subdirectory; no matcher change needed.
  - _Requirements: 4.1, 4.2, 4.3_

- [x] 4. Extend `internal/spec` parsing: bracketed IPv6 + component-length guard
  - Add a bracket-aware branch to `parseOne`: detect a leading `[`, extract the IPv6 literal up to `]`, then parse the trailing `:port:dbname:user` / `:dbname:user`. Validate the literal with `net.ParseIP` (reject missing `]`, non-IP, and zone-id `%scope` forms as `invalid` with a clear detail). Store the **canonical** address (`net.ParseIP(addr).String()`) in `s.Host`. Bracketed IPv4 canonicalizes and flows through the normal (no-`@`) directory.
  - Keep the unbracketed hostname/IPv4 path exactly as today (no regression).
  - After host+port are known, mark the spec `Invalid` ("host too long for artifact path") when `len(serverDir(host, port)) > 255`.
  - _Requirements: 8.1, 8.2, 9.3, 10.1, 10.2, 10.3, 10.7_

- [x] 5. Unit tests — collision and disambiguation
  - [x] 5.1 Two specs sharing `dbname` on different hosts → two distinct paths, both files intact at `DUMP_KEEP=1` (the missing h-f3 regression test).
  - [x] 5.2 Same host, two ports → two subdirectories.
  - [x] 5.3 `host:dbname:user` and `host:5432:dbname:user` → identical subdirectory/path.
  - [x] 5.4 IPv6: a `@`-encoded IPv6 dir and a hostname that would dash-encode to the same string produce **different** directories (disjointness); two distinct IPv6 addresses → different dirs (injectivity).
  - [x] 5.5 IPv6 canonicalization: `[2001:DB8::1]…` and `[2001:db8:0:0:0:0:0:1]…` dedup to one spec and one path.
  - _Requirements: 1.1, 1.2, 1.3, 1.4, 3.2, 10.3, 10.4_

- [x] 6. Unit tests — retention isolation
  - `DUMP_KEEP>1` with two servers sharing a dbname: pruning one server leaves the other's files and count untouched.
  - _Requirements: 4.2, 4.3_

- [x] 7. Unit tests — subdirectory creation failure and traversal safety
  - [x] 7.1 Unwritable `DUMP_DIR` / a file blocking the subdir path → per-DB Result with reason `mkdir_failed` and a detail naming the dir + OS error, no cross-DB effect, no panic.
  - [x] 7.2 Assert constructed paths from validated inputs stay within `DUMP_DIR` (no traversal).
  - _Requirements: 5.2, 5.3, 6.1, 6.2_

- [x] 8. Unit tests — spec parsing edges (length guard + IPv6)
  - An over-long host yields `Invalid` with the documented reason.
  - Bracketed IPv6 parses (`[2001:db8::1]:5432:db:user` and the port-omitted form) to the canonical host + correct fields; missing `]`, a non-IP in brackets, and a zone-id form (`[fe80::1%eth0]…`) each yield `Invalid` with a clear detail.
  - `FuzzParseSpecs` covers bracketed input (no panic; one spec per token).
  - _Requirements: 8.1, 10.1, 10.2_

- [x] 9. Retarget verify-before-replace tests to the subdirectory
  - Ensure the known-good-preserved tests (empty/truncated/pg_error never overwrite a good file) still hold at the new subdirectory path.
  - _Requirements: 9.1_

- [x] 10. Documentation and steering updates
  - Update `README.md` (config/usage, retention, healthcheck-adjacent layout notes) to describe `<host>_<port>/<dbname>.dump` and the migration for versioned collectors.
  - Update `.kiro/steering/pg-autodump.md` (verify-before-replace / DUMP_KEEP layout description) to match, and add `mkdir_failed` to the documented dump-result reason enum.
  - Document that pre-existing flat artifacts are left untouched and may be removed once.
  - Document the IPv6 spec syntax (`[<ipv6>]:port:dbname:user`), the `@`-encoded IPv6 subdir form, and the `.pgpass` colon-escaping caveat (or use `PGPASSWORD`). Update the `DB_SPECS` row in `.kiro/steering/pg-autodump.md` (currently notes "IPv6 literals unsupported").
  - _Requirements: 2.1, 7.1, 7.2, 7.3, 10.7, 10.8_

- [x] 11. Full validation
  - Run the standard battery (`go build`, `go vet`, `golangci-lint`, `go test -race ./...`, `govulncheck`) and the local CI runner; confirm green.
  - _Requirements: 9.1, 9.2, 9.3_
