#!/bin/sh
# Build-time smoke test for the pg-autodump image's tini payload.
#
# Runs in the Dockerfile `test` stage, so the centralized `ci / validate`
# docker build-ability gate executes it on every PR and push (the final image
# stage depends on this stage's marker). Catches a broken tini fetch/COPY
# (missing or non-executable /sbin/tini, wrong release shipped) and a missing
# or version-drifted embedded SBOM fragment: the failure modes for a payload
# that is fetched from upstream instead of installed via apk. The app binary
# itself is covered by the runtime image-smoke harness (tests/image-smoke.*),
# which boots the final image and waits for its HEALTHCHECK.
#
# Run locally:  sh tests/smoke.sh   (needs tini at /sbin/tini or $TINI_BIN;
# set TINI_EXPECTED_VERSION=<X.Y.Z> to also run the exact-version and SBOM
# checks — the fragment only exists in-image, so a bare local run skips them)
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
#     version (TINI_EXPECTED_VERSION, passed by the Dockerfile test stage
#     from ARG TINI_VERSION; a leading "v" is stripped here). Catches a
#     fetch mixup shipping the wrong release. Unset means a bare local run:
#     the check is skipped with a notice. The Dockerfile guards the ARG with
#     :? so the in-image gate can never silently skip. Release binaries
#     report "tini version X.Y.Z - git.<sha>"; the trailing space in the
#     match keeps a prefix (0.19.0 vs 0.19.01) from passing.
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
#    component, and carry the ARG-derived version — a hardcoded version would
#    drift silently on the next Renovate bump, which is exactly the failure
#    mode the fragment exists to prevent. Gated on TINI_EXPECTED_VERSION like
#    section 1a: in-image the Dockerfile's :? guard guarantees the variable,
#    so the gate can never silently skip; a bare local run (no image
#    filesystem) skips with a notice. BusyBox has no jq, so assert shape with
#    grep: non-empty, starts with { and ends with }.
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

[ "$fail" -eq 0 ] && log "pg-autodump image smoke: ok"
exit "$fail"
