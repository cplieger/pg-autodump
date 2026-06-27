#!/bin/sh
# Runtime image smoke test for pg-autodump. Invoked by the central CI docker
# job:  sh tests/image-smoke.sh <image-ref>
#
# Starts the assembled image (with a dummy DB_SPECS and a writable /dumps)
# and waits for the container's own HEALTHCHECK - the `pg-autodump health`
# file-marker probe - to report "healthy", proving the binary boots, its
# startup preflight passes, and the file-marker probe works. The probe reads
# a file marker, not an HTTP endpoint, so this asserts the container
# HEALTHCHECK status only; the binary serves POST /dump and GET /healthz
# (there is no /metrics endpoint). Preflight never dials the database, so no
# live DB is needed.
set -eu

IMG="${1:?usage: image-smoke.sh <image-ref>}"
NAME="smoke-pg-autodump-$$"
TIMEOUT=60

# shellcheck disable=SC2329  # invoked indirectly via trap
cleanup() {
  printf '%s\n' "--- container logs (tail) ---"
  docker logs "$NAME" 2>&1 | tail -40 || true
  docker rm -f "$NAME" > /dev/null 2>&1 || true
}
trap cleanup EXIT

# Preflight needs a writable /dumps and a non-empty DB_SPECS to mark
# healthy; it does NOT dial the database, so a dummy unreachable spec is
# enough to exercise the healthy startup path (no live DB needed).
docker run -d --name "$NAME" \
  -e DB_SPECS="db.smoke:5432:smokedb:smoke" \
  --tmpfs /dumps:size=16m,mode=1777 \
  "$IMG" > /dev/null

i=0
status=starting
while [ "$i" -lt "$TIMEOUT" ]; do
  status=$(docker inspect --format '{{ if .State.Health }}{{ .State.Health.Status }}{{ else }}no-healthcheck{{ end }}' "$NAME" 2> /dev/null || echo gone)
  case "$status" in
    healthy)
      printf 'pg-autodump image smoke: ok (healthy after %ss)\n' "$i"
      exit 0
      ;;
    unhealthy)
      printf '%s\n' "FAIL: pg-autodump reported unhealthy"
      exit 1
      ;;
    no-healthcheck)
      printf '%s\n' "FAIL: image has no HEALTHCHECK to assert against"
      exit 1
      ;;
    gone)
      printf '%s\n' "FAIL: pg-autodump container exited early"
      exit 1
      ;;
  esac
  i=$((i + 1))
  sleep 1
done
printf 'FAIL: pg-autodump did not become healthy within %ss (last status: %s)\n' "$TIMEOUT" "$status"
exit 1
