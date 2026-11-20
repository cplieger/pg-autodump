# check=error=true

# TINI_VERSION is the single source of truth for the pinned tini release.
# Declared as a global ARG (before the first FROM) so the fetch stage below
# consumes it with a bare `ARG TINI_VERSION`; Renovate bumps this one line.
# renovate: datasource=github-releases depName=krallin/tini
ARG TINI_VERSION=v0.19.0
# A stale pin fail-closes the build (sha256sum -c in the fetch stage).
# repin: dep=krallin/tini url=https://github.com/krallin/tini/releases/download/{version}/tini-static-amd64
ARG TINI_SHA256_AMD64=c5b0666b4cb676901f90dfcb37106783c5fe2077b04590973b885950611b30ee
# repin: dep=krallin/tini url=https://github.com/krallin/tini/releases/download/{version}/tini-static-arm64
ARG TINI_SHA256_ARM64=eae1d3aa50c48fb23b8cbdf4e369d0910dfc538566bfd09df89a774aa84a48b9
# repin: dep=krallin/tini url=https://raw.githubusercontent.com/krallin/tini/{version}/LICENSE
ARG TINI_LICENSE_SHA256=e5f46bca81266bdd511cf08018d66866870531794569c04f9b45f50dd23c28b0

# PG_VERSION is the PostgreSQL release whose client tools the pg-builder stage
# compiles. Renovate reads new releases from the official postgres image's tags,
# which track the source releases, so a patch, minor or new major arrives as a
# bump of this one line, and scripts/repin-sha.sh recomputes the tarball pin in
# the same commit. Keep the major at or above the newest server major you dump
# (pg_dump refuses a newer server); the smoke test's server image follows it.
# renovate: datasource=docker depName=postgres
ARG PG_VERSION=19.0
# repin: dep=postgres url=https://ftp.postgresql.org/pub/source/v{version}/postgresql-{version}.tar.bz2
ARG PG_SHA256=a89fd70efbad6a08c7dbed8d4872e3af30170d9e5ca9dc7906523be95b4e692d

# renovate: datasource=docker depName=golang
FROM golang:1.27-alpine@sha256:7d5cbf6833f7331dafd25a2e8b9673477f559759ff8ed4ca8efabe6795ad08db AS builder

# GOTOOLCHAIN=auto: a dependency bump that requires a newer Go patch downloads
# that toolchain instead of failing the build, which decouples module updates
# from base-image updates. The build stays reproducible because go.mod pins the
# version; `local` (the golang image default) hard-fails such a build.
ENV GOTOOLCHAIN=auto

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
# The release pipeline passes the channel tag as BUILD_VERSION; a local build
# reports "dev".
ARG BUILD_VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 \
    go build -trimpath -ldflags="-s -w -X main.version=${BUILD_VERSION}" -o /pg-autodump ./cmd/pg-autodump

COPY LICENSE NOTICE THIRD_PARTY_NOTICES.md ./
COPY scripts/collect-licenses.sh scripts/collect-licenses.sh
RUN --mount=type=cache,target=/go/pkg/mod \
    sh scripts/collect-licenses.sh --name pg-autodump ./cmd/pg-autodump

# ---------------------------------------------------------------------------
# tini fetch stage -- downloads the pinned upstream static binary and its MIT
# license text, each verified fail-closed against a SHA256 pin above. Discarded
# at the end of the build; only those two files reach the runtime image below.
# Native per-arch builds (no TARGETARCH): `uname -m` IS the target arch.
# ---------------------------------------------------------------------------
FROM alpine:3.24@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf AS tini-fetcher

SHELL ["/bin/ash", "-eo", "pipefail", "-c"]

ARG TINI_VERSION
ARG TINI_SHA256_AMD64
ARG TINI_SHA256_ARM64
ARG TINI_LICENSE_SHA256
# ---------------------------------------------------------------------------
# Embedded SBOM fragment, emitted at the tail of the fetch RUN below: the purl
# names the resolved release asset and carries the sha256 that same RUN
# verified, and both are shell variables local to it.
# Syft inventories the final image from Alpine's APK database, Go-binary
# buildinfo, and known binary classifiers, so the upstream-fetched tini static
# binary is invisible to the signed release SBOM and to vulnerability scanners
# (the Go binary and the APK packages ARE visible — the Go module graph is
# pg-autodump's own supply chain and needs no syft config beyond the defaults;
# the source-built PostgreSQL client gets its own fragment in pg-builder). Generate a CycloneDX fragment from the same Renovate-tracked ARGs
# the fetch uses — a Renovate bump keeps the SBOM correct with zero extra
# maintenance — and ship it in the runtime image where Syft's sbom-cataloger
# picks it up. The cataloger is enabled centrally by the release pipeline
# (cplieger/ci); no per-repo .syft.yaml is needed.
# purl: pkg:github/krallin/tini@<tag>?download_url=<asset>&checksum=sha256:<pin>
# describes the exact bytes the build verified. Name, version and CPE are
# arch-independent (one upstream release), but the asset and its pin are not,
# so the fragment is PER-ARCH: each image's SBOM covers the binary that image
# ships. Nothing needs the two to be byte-identical, since each arch builds
# natively on its own runner and the multi-arch manifest points at two distinct
# images. CPE vendor:product is tini_project:tini per the NVD CPE dictionary
# (its entries reference krallin/tini; there is no krallin:tini vendor), e.g.
# https://nvd.nist.gov/products/cpe/detail/084AA5DA-AE01-43E0-957F-78FC06932C08/
# ---------------------------------------------------------------------------
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
    && /tini --version \
    && mkdir -p /out/usr/share/licenses/tini \
    && wget -q --tries=3 --timeout=30 -O /out/usr/share/licenses/tini/LICENSE \
      "https://raw.githubusercontent.com/krallin/tini/${TINI_VERSION}/LICENSE" \
    && { echo "${TINI_LICENSE_SHA256}  /out/usr/share/licenses/tini/LICENSE" | sha256sum -c - || { \
      echo "tini LICENSE sha256 pin mismatch for ${TINI_VERSION}: recompute from the release tag and update ARG TINI_LICENSE_SHA256" >&2; \
      exit 1; \
    }; } \
    && cat > /tini.cdx.json <<EOF
{
  "bomFormat": "CycloneDX",
  "specVersion": "1.5",
  "version": 1,
  "components": [
    {
      "bom-ref": "pkg:github/krallin/tini@${TINI_VERSION}",
      "type": "application",
      "name": "tini",
      "version": "${TINI_VERSION#v}",
      "purl": "pkg:github/krallin/tini@${TINI_VERSION}?download_url=https://github.com/krallin/tini/releases/download/${TINI_VERSION}/tini-static-${TINI_ARCH}&checksum=sha256:${TINI_SHA256}",
      "cpe": "cpe:2.3:a:tini_project:tini:${TINI_VERSION#v}:*:*:*:*:*:*:*"
    }
  ]
}
EOF

# ---------------------------------------------------------------------------
# PostgreSQL client build stage -- compiles pg_dump, pg_dumpall, pg_restore,
# psql and libpq from the source release PG_VERSION names, verified fail-closed
# against PG_SHA256. Alpine packages one PostgreSQL major per release branch
# and its name carries the major (postgresql18-client), which no Renovate
# manager can move; a source build follows PG_VERSION instead. Built on the
# same Alpine pin as the runtime base, so the binaries link the musl, OpenSSL
# and compression libraries the base installs. Only the features a network
# dump client uses are enabled: TLS (OpenSSL), the lz4/zstd/gzip dump
# compression methods and readline for psql. ICU, GSSAPI, LDAP, PAM and the
# server-side languages stay off, so their libraries never reach the image.
# Discarded at the end of the build; /pkg, the license text and the SBOM
# fragment are all that reach the runtime image below.
# ---------------------------------------------------------------------------
# renovate: datasource=docker depName=alpine
FROM alpine:3.24@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf AS pg-builder

SHELL ["/bin/ash", "-eo", "pipefail", "-c"]

ARG PG_VERSION
ARG PG_SHA256
# bison, flex and perl: release tarballs ship no generated parser or catalog
# headers since PostgreSQL 17, so the build generates them, first and alone
# (generated-headers), because a parallel subdirectory build races them.
RUN apk add --no-cache bison build-base flex linux-headers lz4-dev openssl-dev \
      perl readline-dev zlib-dev zstd-dev
WORKDIR /build
RUN wget -q --tries=3 --timeout=60 -O /postgresql.tar.bz2 \
      "https://ftp.postgresql.org/pub/source/v${PG_VERSION}/postgresql-${PG_VERSION}.tar.bz2" \
    && { echo "${PG_SHA256}  /postgresql.tar.bz2" | sha256sum -c - || { \
      echo "PostgreSQL sha256 pin mismatch for ${PG_VERSION}: recompute from https://ftp.postgresql.org/pub/source/v${PG_VERSION}/ and update ARG PG_SHA256" >&2; \
      exit 1; \
    }; } \
    && tar -xjf /postgresql.tar.bz2 --strip-components=1 \
    && rm /postgresql.tar.bz2
RUN ./configure --prefix=/usr/local --with-ssl=openssl --with-lz4 --with-zstd \
      --without-icu --without-gssapi --without-ldap --without-pam \
      --without-perl --without-python --without-tcl --without-libxml \
      --without-llvm \
    && make -j"$(nproc)" -C src/backend generated-headers \
    && make -j"$(nproc)" -C src/interfaces/libpq install-strip DESTDIR=/out \
    && make -j"$(nproc)" -C src/bin/pg_dump install-strip DESTDIR=/out \
    && make -j"$(nproc)" -C src/bin/psql install-strip DESTDIR=/out \
    && mkdir -p /pkg/usr/local/bin /pkg/usr/local/lib \
    && cp -a /out/usr/local/bin/pg_dump /out/usr/local/bin/pg_dumpall \
      /out/usr/local/bin/pg_restore /out/usr/local/bin/psql /pkg/usr/local/bin/ \
    && cp -a /out/usr/local/lib/libpq.so.5* /pkg/usr/local/lib/ \
    && mkdir -p /licenses/postgresql \
    && cp COPYRIGHT /licenses/postgresql/COPYRIGHT \
    && printf 'COPYRIGHT from https://ftp.postgresql.org/pub/source/v%s/postgresql-%s.tar.bz2, version %s\n' \
      "${PG_VERSION}" "${PG_VERSION}" "${PG_VERSION}" > /licenses/postgresql/SOURCE \
    && cat > /postgresql.cdx.json <<EOF
{
  "bomFormat": "CycloneDX",
  "specVersion": "1.5",
  "version": 1,
  "components": [
    {
      "bom-ref": "pkg:generic/postgresql@${PG_VERSION}",
      "type": "application",
      "name": "postgresql",
      "version": "${PG_VERSION}",
      "purl": "pkg:generic/postgresql@${PG_VERSION}?download_url=https://ftp.postgresql.org/pub/source/v${PG_VERSION}/postgresql-${PG_VERSION}.tar.bz2&checksum=sha256:${PG_SHA256}",
      "cpe": "cpe:2.3:a:postgresql:postgresql:${PG_VERSION}:*:*:*:*:*:*:*"
    }
  ]
}
EOF

# Alpine for a libc base (the PostgreSQL client links libpq). pg-autodump is a
# plain network client, so there is no Docker socket or docker CLI bundle.
# renovate: datasource=docker depName=alpine
FROM alpine:3.24@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf AS base

# apk upgrade: the pinned base ships some packages (e.g. libssl3) at a stale,
# CVE-affected revision; upgrading floats them forward on each rebuild.
# The runtime libraries the pg-builder binaries link: OpenSSL (libssl3 brings
# libcrypto3), lz4, zstd and readline; zlib and musl are already in the base.
# PKG_REFRESH busts the cache for this layer. Without it BuildKit restores the
# layer verbatim on every rebuild, so the `apk upgrade` below floats nothing
# forward after the first build and the image keeps shipping the packages that
# were current then. The central release/CI/scan builds pass today's UTC date.
# The `echo` is load-bearing: BuildKit keys a RUN on the build args it actually
# CONSUMES, so a merely-declared ARG would change nothing.
ARG PKG_REFRESH=static
RUN echo "OS package refresh: ${PKG_REFRESH}" \
    && apk upgrade --no-cache \
    && apk add --no-cache libssl3 lz4-libs readline zstd-libs \
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

# CycloneDX SBOM fragment for the upstream-fetched tini (generated in the
# fetch stage from the Renovate-tracked version ARG). Placed where the
# release pipeline's Syft sbom-cataloger inventories it, so SBOMs and
# scanners see tini alongside the APK packages and the Go binary.
COPY --from=tini-fetcher /tini.cdx.json /usr/share/sbom/pg-autodump.cdx.json

# pg_dump, pg_dumpall, pg_restore, psql (/usr/local/bin) and libpq
# (/usr/local/lib), built in the pg-builder stage, with their SBOM fragment.
COPY --from=pg-builder /pkg/ /
COPY --from=pg-builder /postgresql.cdx.json /usr/share/sbom/postgresql.cdx.json

COPY --chmod=755 --from=builder /pg-autodump /usr/local/bin/pg-autodump

COPY --from=builder /out/usr/share/licenses /usr/share/licenses
COPY --from=tini-fetcher /out/usr/share/licenses/tini /usr/share/licenses/tini
COPY --from=pg-builder /licenses/postgresql /usr/share/licenses/postgresql

# Unprivileged by default: no Docker socket, no root. The container needs only
# network reach to the databases, a read-only .pgpass, and a writable /dumps.
USER 65532:65532

# ---------------------------------------------------------------------------
# Test stage — runs the build-time smoke test (the shipped tini and PostgreSQL
# client tools run at their pinned versions + both embedded SBOM fragments are
# present and version-true).
# A failure here fails the centralized `ci / validate` docker build gate,
# because the final stage below depends on this stage's marker. Runs as the
# runtime user (inherited from base); the marker lands in /tmp because / is
# not writable at UID 65532.
# ---------------------------------------------------------------------------
FROM base AS test
ARG TINI_VERSION
ARG PG_VERSION
COPY tests/ /tmp/tests/
# ${TINI_VERSION:?} fails the build if the ARG wiring ever breaks, so the
# smoke test's exact-version assertions can never be skipped in-image.
RUN TINI_EXPECTED_VERSION="${TINI_VERSION:?}" PG_EXPECTED_VERSION="${PG_VERSION:?}" \
    sh /tmp/tests/smoke.sh \
    && touch /tmp/tests-passed

# ---------------------------------------------------------------------------
# Final stage — the runtime image. Must remain last so the CI build gate
# (which builds the default target) produces it; the marker COPY forces the
# test stage to build and pass first (root-owned, zero-byte, shadowed by any
# tmpfs mounted over /tmp at runtime).
# ---------------------------------------------------------------------------
FROM base AS final
COPY --from=test /tmp/tests-passed /tmp/tests-passed

COPY licenses/ /usr/share/licenses/

EXPOSE 9472

# Health via the binary's own probe, which asks GET /healthz on loopback: no
# shell, no curl.
HEALTHCHECK --interval=30s --timeout=5s --retries=3 --start-period=6m \
    CMD ["/usr/local/bin/pg-autodump", "health"]

# Default command is the server; `health` and `trigger` are the other verbs.
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/pg-autodump"]
