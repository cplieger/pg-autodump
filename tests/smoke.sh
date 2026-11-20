#!/bin/sh
# Build-time smoke test for the pg-autodump image's tini and PostgreSQL client
# payloads.
#
# Runs in the Dockerfile `test` stage, so the centralized `ci / validate`
# docker build-ability gate executes it on every PR and push (the final image
# stage depends on this stage's marker). Catches a broken tini fetch/COPY
# (missing or non-executable /sbin/tini, wrong release shipped) and a missing
# or version-drifted embedded SBOM fragment, and the same for the PostgreSQL
# client tools built from source (a binary missing a runtime library fails its
# --version run): the failure modes for a payload that is fetched or built
# from upstream instead of installed via apk. The app binary
# itself is covered by the runtime image-smoke harness (tests/image-smoke.*),
# which boots the final image and waits for its HEALTHCHECK.
#
# Run locally:  sh tests/smoke.sh   (needs tini at /sbin/tini or $TINI_BIN;
# set TINI_EXPECTED_VERSION=<X.Y.Z> and PG_EXPECTED_VERSION=<X.Y> to also run
# the exact-version and SBOM checks — the fragments only exist in-image, so a
# bare local run skips them)
set -eu

fail=0
log() { printf '%s\n' "$*"; }
err() { printf '%s\n' "$*" >&2; }

TINI_BIN="${TINI_BIN:-/sbin/tini}"

# 1. The shipped tini is present, executable, and runs (catches a broken
#    fetch-stage COPY or a corrupt/wrong-arch binary).
if ! ver=$("$TINI_BIN" --version 2>&1); then
  err "FAIL: '$TINI_BIN --version' did not run"
  err "$ver"
  fail=1
fi

# 1a. Exact-version assertion: the binary must report the pinned upstream
#     version (TINI_EXPECTED_VERSION, from the Dockerfile's ARG TINI_VERSION,
#     leading "v" stripped). Catches a fetch mixup shipping the wrong release.
#     Unset means a bare local run: skipped with a notice (the Dockerfile
#     guards the ARG with :? so the in-image gate can never silently skip).
#     The trailing space in the match keeps a prefix (0.19.0 vs 0.19.01) from
#     passing.
if [ -n "${TINI_EXPECTED_VERSION:-}" ]; then
  expected=${TINI_EXPECTED_VERSION#v}
  if ! printf '%s\n' "$ver" | head -n 1 | grep -qF "tini version ${expected} "; then
    err "FAIL: version mismatch: expected ${expected}, got: $(printf '%s\n' "$ver" | head -n 1)"
    fail=1
  fi
else
  log "note: TINI_EXPECTED_VERSION unset - skipping exact-version check (local run)"
fi

# 2. Embedded SBOM fragment (Dockerfile tini-fetcher stage): the CycloneDX
#    file covering the upstream-fetched tini must ship in the image, name the
#    component, and carry the ARG-derived version (a hardcoded version would
#    drift silently on the next Renovate bump). Gated on TINI_EXPECTED_VERSION
#    like section 1a. BusyBox has no jq, so assert shape with grep instead.
if [ -n "${TINI_EXPECTED_VERSION:-}" ]; then
  sbom=/usr/share/sbom/pg-autodump.cdx.json
  expected=${TINI_EXPECTED_VERSION#v}
  if [ ! -s "$sbom" ]; then
    err "FAIL: embedded SBOM fragment missing or empty: $sbom"
    fail=1
  else
    if [ "$(head -c 1 "$sbom")" != "{" ] || [ "$(tail -c 2 "$sbom")" != "}" ]; then
      err "FAIL: embedded SBOM fragment is not a JSON object (bad first/last byte)"
      fail=1
    fi
    grep -q '"name": "tini"' "$sbom" || {
      err "FAIL: embedded SBOM fragment missing component: tini"
      fail=1
    }
    # Exactly one version-shaped component version ("version": 1 — the BOM
    # serial, unquoted — and "specVersion" don't match the pattern).
    # grep -c prints the count (0 included) even when it exits 1 on zero
    # matches; || true keeps set -e from aborting before the FAIL report.
    versions=$(grep -c '"version": "[0-9][0-9.]*"' "$sbom" || true)
    if [ "$versions" -ne 1 ]; then
      err "FAIL: embedded SBOM fragment has $versions version-shaped component versions (want 1)"
      fail=1
    fi
    grep -qF "\"version\": \"${expected}\"" "$sbom" || {
      err "FAIL: embedded SBOM fragment version is not ${expected} (ARG wiring broken?)"
      fail=1
    }
  fi
else
  log "note: TINI_EXPECTED_VERSION unset - skipping SBOM fragment check (local run)"
fi

# 3. The PostgreSQL client tools pg-autodump runs (internal/pg requiredBins,
#    plus pg_dumpall) are present and run, which proves every library they
#    link is in the image, and each reports the pinned PG_VERSION. A tool
#    prints "<name> (PostgreSQL) <version>"; the trailing end-of-line anchor
#    keeps 19.1 from passing for 19.10.
for tool in pg_dump pg_dumpall pg_restore psql; do
  if ! tver=$("$tool" --version 2>&1); then
    err "FAIL: '$tool --version' did not run"
    err "$tver"
    fail=1
    continue
  fi
  if [ -n "${PG_EXPECTED_VERSION:-}" ] \
    && ! printf '%s\n' "$tver" | head -n 1 | grep -qx "$tool (PostgreSQL) ${PG_EXPECTED_VERSION}"; then
    err "FAIL: $tool version mismatch: expected ${PG_EXPECTED_VERSION}, got: $(printf '%s\n' "$tver" | head -n 1)"
    fail=1
  fi
done

# 4. The PostgreSQL client's SBOM fragment (Dockerfile pg-builder stage) ships,
#    names the component and carries the ARG-derived version, as section 2
#    checks for tini.
if [ -n "${PG_EXPECTED_VERSION:-}" ]; then
  pgsbom=/usr/share/sbom/postgresql.cdx.json
  if [ ! -s "$pgsbom" ]; then
    err "FAIL: embedded SBOM fragment missing or empty: $pgsbom"
    fail=1
  else
    if [ "$(head -c 1 "$pgsbom")" != "{" ] || [ "$(tail -c 2 "$pgsbom")" != "}" ]; then
      err "FAIL: PostgreSQL SBOM fragment is not a JSON object (bad first/last byte)"
      fail=1
    fi
    grep -q '"name": "postgresql"' "$pgsbom" || {
      err "FAIL: PostgreSQL SBOM fragment missing component: postgresql"
      fail=1
    }
    grep -qF "\"version\": \"${PG_EXPECTED_VERSION}\"" "$pgsbom" || {
      err "FAIL: PostgreSQL SBOM fragment version is not ${PG_EXPECTED_VERSION} (ARG wiring broken?)"
      fail=1
    }
  fi
else
  log "note: PG_EXPECTED_VERSION unset - skipping PostgreSQL version and SBOM checks (local run)"
fi

[ "$fail" -eq 0 ] && log "pg-autodump image smoke: ok"
exit "$fail"
