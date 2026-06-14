# check=error=true

# renovate: datasource=docker depName=golang
FROM golang:1.26-alpine@sha256:7a3e50096189ad57c9f9f865e7e4aa8585ed1585248513dc5cda498e2f41812c AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 \
    go build -trimpath -ldflags="-s -w" -o /pg-autodump ./cmd/pg-autodump

# Alpine for a libc base (the PostgreSQL client links libpq). pg-autodump is a
# plain network client, so there is no Docker socket or docker CLI bundle.
# postgresqlNN-client provides pg_dump/pg_restore/psql; pin the major to the
# newest PostgreSQL SERVER major you dump (client must be >= server). Bump
# together with your servers.
# renovate: datasource=docker depName=alpine
FROM alpine:3.24@sha256:a2d49ea686c2adfe3c992e47dc3b5e7fa6e6b5055609400dc2acaeb241c829f4

# apk upgrade: the pinned base ships some packages (e.g. libssl3) at a stale,
# CVE-affected revision; upgrading floats them forward on each rebuild.
# postgresql18-client: pg_dump / pg_restore / psql (network clients).
# tini: PID 1 for clean signal handling and zombie reaping.
RUN apk upgrade --no-cache \
    && apk add --no-cache postgresql18-client tini \
    && addgroup -S pgautodump && adduser -S -G pgautodump -u 65532 pgautodump

COPY --from=builder /pg-autodump /usr/local/bin/pg-autodump

# Unprivileged by default: no Docker socket, no root. The container needs only
# network reach to the databases, a read-only .pgpass, and a writable /dumps.
USER 65532:65532

# Liveness via the binary's own probe (file marker): no shell, no curl, no port.
HEALTHCHECK --interval=30s --timeout=5s --retries=3 --start-period=10s \
    CMD ["pg-autodump", "health"]

# Default command is the server; `health` and `trigger` are the other verbs.
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/pg-autodump"]
