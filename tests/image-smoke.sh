#!/bin/sh
# Runtime image smoke test for pg-autodump. Invoked by the central CI docker
# job:  sh tests/image-smoke.sh <image-ref>
#
# Starts the assembled image (with a dummy DB_SPECS and a writable /dumps) and
# waits for the container's own HEALTHCHECK - the `pg-autodump health`
# file-marker probe - to report "healthy": proves the binary boots in the real
# base image, its startup preflight passes (pg_dump/pg_restore/psql present,
# /dumps writable, DB_SPECS non-empty), and the file-marker probe works. The
# probe reads a file marker, not an HTTP endpoint, so this asserts the
# container HEALTHCHECK status only. Preflight never dials the database, so a
# dummy unreachable spec is enough and no live DB is needed.
set -eu

IMG="${1:?usage: image-smoke.sh <image-ref>}"
NAME="smoke-pg-autodump-$$"
TIMEOUT=60 # must cover the image's healthcheck start-period + a few intervals

# shellcheck disable=SC2329  # invoked indirectly via trap
cleanup() {
  code=$?
  # Dump container logs only on failure (a passing run stays quiet).
  if [ "$code" -ne 0 ]; then
    printf '%s\n' "--- container logs (tail) ---" >&2
    docker logs "$NAME" 2>&1 | tail -40 >&2 || true
  fi
  docker rm -f "$NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# Preflight needs a writable /dumps and a non-empty DB_SPECS to mark healthy;
# it does NOT dial the database, so a dummy unreachable spec is enough to
# exercise the healthy startup path (no live DB needed).
docker run -d --name "$NAME" \
  -e DB_SPECS="db.smoke:5432:smokedb:smoke" \
  --tmpfs /dumps:size=16m,mode=1777 \
  "$IMG" >/dev/null

i=0
status=starting
while [ "$i" -lt "$TIMEOUT" ]; do
  # Fail fast on an early exit: poll .State.Running before the health status so
  # a crash-boot is caught by its exit code (more debuggable than "unhealthy")
  # and the verdict never depends on what health a stopped container reports.
  if [ "$(docker inspect --format '{{ .State.Running }}' "$NAME" 2>/dev/null || echo missing)" != "true" ]; then
    ec=$(docker inspect --format '{{ .State.ExitCode }}' "$NAME" 2>/dev/null || echo '?')
    printf 'FAIL: pg-autodump container exited early (exit code %s)\n' "$ec" >&2
    exit 1
  fi
  status=$(docker inspect --format '{{ if .State.Health }}{{ .State.Health.Status }}{{ else }}no-healthcheck{{ end }}' "$NAME" 2>/dev/null || echo gone)
  case "$status" in
    healthy)
      printf 'pg-autodump image smoke: ok (healthy after %ss)\n' "$i"
      exit 0
      ;;
    unhealthy)
      printf 'FAIL: pg-autodump reported unhealthy\n' >&2
      exit 1
      ;;
    no-healthcheck)
      printf 'FAIL: image has no HEALTHCHECK to assert against\n' >&2
      exit 1
      ;;
    gone)
      printf 'FAIL: pg-autodump container is gone\n' >&2
      exit 1
      ;;
  esac
  i=$((i + 1))
  sleep 1
done
printf 'FAIL: pg-autodump did not become healthy within %ss (last status: %s)\n' "$TIMEOUT" "$status" >&2
exit 1
