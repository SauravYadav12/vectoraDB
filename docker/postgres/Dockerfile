# syntax=docker/dockerfile:1
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Stock PostgreSQL 16 + wal-g (object-storage WAL archival, base backups, PITR).
# wal-g is installed as a pinned prebuilt release binary matching the build arch,
# so this image builds natively on both amd64 and Apple Silicon (arm64).
#
# The ubuntu-20.04 build is used deliberately: it links against an older glibc
# and therefore runs on the Debian bookworm base (glibc is forward-compatible).

FROM postgres:16-bookworm

ARG WALG_VERSION=v3.0.9
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends curl ca-certificates; \
    rm -rf /var/lib/apt/lists/*; \
    arch="$(dpkg --print-architecture)"; \
    case "${arch}" in \
      amd64) WG_ARCH=amd64 ;; \
      arm64) WG_ARCH=aarch64 ;; \
      *) echo "unsupported arch: ${arch}" >&2; exit 1 ;; \
    esac; \
    asset="wal-g-pg-20.04-${WG_ARCH}.tar.gz"; \
    url="https://github.com/wal-g/wal-g/releases/download/${WALG_VERSION}/${asset}"; \
    echo "downloading ${url}"; \
    curl -fsSL "${url}" -o /tmp/wal-g.tar.gz; \
    tar -xzf /tmp/wal-g.tar.gz -C /usr/local/bin; \
    mv "/usr/local/bin/wal-g-pg-20.04-${WG_ARCH}" /usr/local/bin/wal-g; \
    chmod +x /usr/local/bin/wal-g; \
    rm -f /tmp/wal-g.tar.gz; \
    /usr/local/bin/wal-g --version

# Disposable point-in-time restore entrypoint (used by scripts/pitr_restore.sh).
COPY restore-entrypoint.sh /usr/local/bin/restore-entrypoint.sh
RUN chmod +x /usr/local/bin/restore-entrypoint.sh
