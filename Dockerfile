# syntax=docker/dockerfile:1
#
# Bosun container image, built from source:
#
#   docker build -t bosun:dev --build-arg VERSION=$(git describe --tags --always) .
#
# Release images are built by GoReleaser from the prebuilt binaries with
# deploy/docker/Dockerfile.goreleaser; keep the runtime stage of both files in
# sync. See docs/DEPLOY.md for running it with deploy/docker/compose.yml.

ARG GO_VERSION=1.26
ARG ALPINE_VERSION=3.22

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# modernc.org/sqlite is pure Go, so the binaries are static (CGO_ENABLED=0).
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/bosund ./cmd/bosund && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/bosun ./cmd/bosun

FROM alpine:${ALPINE_VERSION}
# ca-certificates: TLS to LLM providers, mail and calendar servers.
# tzdata: routine schedules in daemon.timezone / TZ.
# openssl: first-run self-signed certificate (see entrypoint.sh).
# WITH_DOCKER_CLI=true adds the docker CLI for the code sandbox; only the
# opt-in compose.sandbox.yml overlay sets it (see docs/DEPLOY.md).
ARG WITH_DOCKER_CLI=false
RUN apk add --no-cache ca-certificates tzdata openssl && \
    if [ "$WITH_DOCKER_CLI" = "true" ]; then apk add --no-cache docker-cli; fi && \
    addgroup -g 1000 bosun && \
    adduser -D -H -u 1000 -G bosun -h /data -s /sbin/nologin bosun && \
    mkdir -p /data && chown bosun:bosun /data && chmod 700 /data
COPY --from=build /out/bosund /out/bosun /usr/local/bin/
COPY deploy/docker/bosun.yaml /etc/bosun/bosun.yaml.template
COPY --chmod=0755 deploy/docker/entrypoint.sh /usr/local/bin/bosun-entrypoint
COPY --chmod=0755 deploy/docker/healthcheck.sh /usr/local/bin/bosun-healthcheck

ENV BOSUN_CONFIG=/data/bosun.yaml \
    HOME=/data
USER bosun:bosun
WORKDIR /data
VOLUME /data
EXPOSE 7777
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 CMD ["bosun-healthcheck"]
ENTRYPOINT ["bosun-entrypoint"]
CMD ["bosund", "--config", "/data/bosun.yaml"]
