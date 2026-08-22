# syntax=docker/dockerfile:1

# Three stages: build the console, build the binary with the console embedded,
# then ship the binary alone. The runtime image carries no Node, no toolchain
# and no source — just a static binary and a CA bundle.

# ─── 1. Console ───────────────────────────────────────────────────────────────
FROM node:22-alpine AS web

WORKDIR /web

# Dependencies are installed from the lockfile before the source is copied, so
# a change to a component does not reinstall node_modules.
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund

COPY web/ ./
# Fonts are bundled by this build rather than fetched from Google at runtime,
# which is what lets the console work on an air-gapped network.
RUN npm run build

# ─── 2. Binary ────────────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS build

WORKDIR /src

# Modules first, for the same reason.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
# The console build output replaces the placeholder the repository carries.
COPY --from=web /internal/web/dist ./internal/web/dist

# The data directory is created here, owned by the runtime user, and copied
# into the final image. It cannot be created there: distroless has no shell, so
# there is no RUN to make it with.
#
# The ownership is the point. Docker seeds a fresh named volume from whatever
# is at the mount path in the image — including its ownership. Without a
# /data owned by uid 65532 in the image, the volume is created root-owned and
# the non-root process cannot write to it, which fails at startup with
# "mkdir /data/blobs: permission denied".
RUN mkdir -p /data && chown 65532:65532 /data

ARG VERSION=dev

# CGO_ENABLED=0 makes the binary static, so it runs on a distroless image with
# no libc at all. -trimpath keeps build paths out of the binary; -s -w drop the
# symbol table and DWARF, which is most of the size.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/s3d ./cmd/s3d

# ─── 3. Runtime ───────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot

# ca-certificates comes with the distroless image and is needed to reach
# Resend over TLS. Nothing else is.
COPY --from=build /out/s3d /usr/local/bin/s3d

# Carried over with its ownership, so a fresh named volume is seeded writable
# by the non-root user. See the note in the build stage.
COPY --from=build --chown=65532:65532 /data /data

VOLUME ["/data"]

# 9000 speaks the S3 API, 9001 serves the console. They are separate so bucket
# paths can never collide with console routes, and so each can be given its own
# hostname in a reverse proxy.
EXPOSE 9000 9001

# nonroot is uid 65532. The data volume must be writable by it; the compose
# file uses a named volume, which Docker creates with the right ownership.
USER nonroot:nonroot

ENV DATA_DIR=/data \
    S3_PORT=9000 \
    CONSOLE_PORT=9001

ENTRYPOINT ["/usr/local/bin/s3d"]
