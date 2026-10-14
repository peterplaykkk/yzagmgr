# syntax=docker/dockerfile:1
#
# Self-contained multi-stage build:
#   web     — build the React admin UI (web/dist) with Vite
#   build   — static yzagmgr binary (pure Go, CGO_ENABLED=0)
#   runtime — minimal alpine image, non-root (uid 65532)
#
# The daemon does not embed the web assets (no go:embed): the runtime image
# copies web/dist to /app/web/dist, where the daemon picks it up next to the
# binary. Without web/dist the daemon still runs (API only; the admin UI 404s).
#
# Networking: the admin API always binds 127.0.0.1 and the data plane
# (`YZAGMGR_PROXY_BIND`) accepts only loopback/private literals, so this image
# is meant to run with host networking:
#   docker build -t yzagmgr . \
#     && docker run --network host -v yzagmgr-data:/data yzagmgr
# See the README "Configuration" section for the environment variables.
#
# GOARCH follows the build platform via BuildKit's meta-argument TARGETARCH:
# a plain `docker build .` on x86_64 or arm64 produces a native image.
#
# Base image refs are overridable for air-gapped or mirrored registries:
#   docker build --build-arg GO_IMAGE=<registry>/golang:1.25-alpine .
ARG NODE_IMAGE=node:22-alpine
ARG GO_IMAGE=golang:1.25-alpine
ARG RUNTIME_IMAGE=alpine:3.24

FROM ${NODE_IMAGE} AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web ./
RUN npm run build

FROM ${GO_IMAGE} AS build
WORKDIR /src
# go.mod declares `go 1.25.0`; the image toolchain satisfies it, so never
# download another toolchain.
ENV GOTOOLCHAIN=local
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY sdk ./sdk
ARG TARGETARCH
ARG YZAGMGR_VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${YZAGMGR_VERSION}" \
    -o /out/yzagmgr ./cmd/yzagmgr

FROM ${RUNTIME_IMAGE}
# ca-certificates — system trust store for upstream HTTPS (OpenAI / Anthropic / GLM).
# tzdata          — named IANA zones for pricing/quota windows (TZ, rule timezone);
#                   without the tz database Go silently falls back to UTC.
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/yzagmgr /app/yzagmgr
COPY --from=web /web/dist /app/web/dist
# Persistent state lives under YZAGMGR_HOME (usage.db, admin.token, logs/, ...).
# Mount a volume at /data; the directory is pre-owned by the runtime uid so a
# named volume inherits the right ownership on first use.
ENV YZAGMGR_HOME=/data \
    YZAGMGR_WEB_DIR=/app/web/dist
RUN mkdir -p /data && chown 65532:65532 /data
VOLUME /data
USER 65532:65532
ENTRYPOINT ["/app/yzagmgr"]
