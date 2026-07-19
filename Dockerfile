# check=error=true

# TINI_VERSION is the single source of truth for the pinned tini release.
# Declared as a global ARG (before the first FROM) so the fetch stage below
# consumes it with a bare `ARG TINI_VERSION`; Renovate bumps this one line.
# renovate: datasource=github-releases depName=krallin/tini
ARG TINI_VERSION=v0.19.0
# Integrity pins -- when TINI_VERSION is bumped, update both SHA256s to the
# new release's values. Renovate can't recompute them (github-releases exposes
# the tag, not asset hashes), so the bump needs a manual recompute; upstream
# publishes the hash next to each asset:
#   curl -fsSL https://github.com/krallin/tini/releases/download/<version>/tini-static-amd64.sha256sum
#   curl -fsSL https://github.com/krallin/tini/releases/download/<version>/tini-static-arm64.sha256sum
# A stale pin fail-closes the build (sha256sum -c in the fetch stage).
ARG TINI_SHA256_AMD64=c5b0666b4cb676901f90dfcb37106783c5fe2077b04590973b885950611b30ee
ARG TINI_SHA256_ARM64=eae1d3aa50c48fb23b8cbdf4e369d0910dfc538566bfd09df89a774aa84a48b9

# renovate: datasource=docker depName=golang
FROM golang:1.26-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 \
    go build -trimpath -ldflags="-s -w" -o /pg-autodump ./cmd/pg-autodump

# ---------------------------------------------------------------------------
# tini fetch stage -- downloads the pinned upstream static binary and verifies
# it fail-closed against the per-arch SHA256 pins above. Discarded at the end
# of the build; only the verified binary reaches the runtime image below.
# Native per-arch builds (no TARGETARCH): `uname -m` IS the target arch.
# ---------------------------------------------------------------------------
FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b AS tini-fetcher

SHELL ["/bin/ash", "-eo", "pipefail", "-c"]

ARG TINI_VERSION
ARG TINI_SHA256_AMD64
ARG TINI_SHA256_ARM64
RUN case "$(uname -m)" in \
      x86_64)  TINI_ARCH=amd64 TINI_SHA256="${TINI_SHA256_AMD64}" ;; \
      aarch64) TINI_ARCH=arm64 TINI_SHA256="${TINI_SHA256_ARM64}" ;; \
      *) echo "unsupported build architecture: $(uname -m) (expected x86_64 or aarch64); no integrity pin defined" >&2; exit 1 ;; \
    esac \
    && wget -q --tries=3 --timeout=30 -O /tini \
      "https://github.com/krallin/tini/releases/download/${TINI_VERSION}/tini-static-${TINI_ARCH}" \
    && { echo "${TINI_SHA256}  /tini" | sha256sum -c - || { \
      echo "tini sha256 pin mismatch: tini-static-${TINI_ARCH} does not match TINI_SHA256_${TINI_ARCH} for ${TINI_VERSION}; recompute from upstream's published .sha256sum assets and update both ARG TINI_SHA256_* pins" >&2; \
      exit 1; \
    }; } \
    && chmod 755 /tini \
    && /tini --version

# Alpine for a libc base (the PostgreSQL client links libpq). pg-autodump is a
# plain network client, so there is no Docker socket or docker CLI bundle.
# postgresqlNN-client provides pg_dump/pg_restore/psql; pin the major to the
# newest PostgreSQL SERVER major you dump (client must be >= server). Bump
# together with your servers.
# renovate: datasource=docker depName=alpine
FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b

# apk upgrade: the pinned base ships some packages (e.g. libssl3) at a stale,
# CVE-affected revision; upgrading floats them forward on each rebuild.
# postgresql18-client: pg_dump / pg_restore / psql (network clients).
RUN apk upgrade --no-cache \
    && apk add --no-cache postgresql18-client \
    && addgroup -g 65532 -S pgautodump && adduser -S -G pgautodump -u 65532 pgautodump

# tini (PID 1 for clean signal handling and zombie reaping): pinned upstream
# static binary, SHA256-verified in the fetch stage above. /sbin/tini matches
# the path the apk package used, so the ENTRYPOINT is unchanged.
#
# tini runs in its default child-only forwarding mode (no -g): a docker-stop
# SIGTERM reaches the daemon, never the pg_dump children, so the shutdown
# drain can finish an in-flight dump. The app does not depend on that default,
# though: every child it spawns leads its own process group (Setpgid in
# internal/pg), so even a group-forwarding init cannot TERM a dump out-of-band
# — the hardening docker-renovate-scheduler needed under its dumb-init PID 1.
COPY --chmod=755 --from=tini-fetcher /tini /sbin/tini

COPY --chmod=755 --from=builder /pg-autodump /usr/local/bin/pg-autodump

# Unprivileged by default: no Docker socket, no root. The container needs only
# network reach to the databases, a read-only .pgpass, and a writable /dumps.
USER 65532:65532

# Liveness via the binary's own probe (file marker): no shell, no curl, no port.
HEALTHCHECK --interval=30s --timeout=5s --retries=3 --start-period=15s \
    CMD ["/usr/local/bin/pg-autodump", "health"]

# Default command is the server; `health` and `trigger` are the other verbs.
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/pg-autodump"]
