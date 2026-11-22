# The public image contains only the headless indexing backend (non-root UID 1000).
FROM golang:1.26.11-alpine3.23@sha256:db99415e2625ce0ee03eed09edf7f32ae8c58dc399f28cc602a87fe7238119e4 AS build

RUN apk --no-cache add git

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

# VERSION is the release source of truth, including source archives without Git.
# CI and release builds pass the same version explicitly for OCI labels.
ARG VERSION
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/spencernorton/DH-Waypoint/internal/version.GitTag=${VERSION:-v$(cat VERSION)}" \
    -o /build/dh-waypoint .

FROM alpine:3.23.3@sha256:25109184c71bdad752c8312a8623239686a9a2071e8825f20acb8f2198c3f659

ARG BUILD_DATE
ARG REVISION
ARG VERSION
ARG SOURCE
LABEL org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.source="${SOURCE}"

# TLS roots for metadata and optional service APIs, time zones, and the Compose health probe.
RUN apk --no-cache add ca-certificates curl tzdata \
 && addgroup -g 1000 -S dhwaypoint \
 && adduser -u 1000 -S -G dhwaypoint -h /var/lib/dh-waypoint dhwaypoint \
 && mkdir -p /var/lib/dh-waypoint/config/bitmagnet /var/lib/dh-waypoint/data/bitmagnet \
 && chown -R 1000:1000 /var/lib/dh-waypoint

ENV HOME=/var/lib/dh-waypoint \
    XDG_CONFIG_HOME=/var/lib/dh-waypoint/config \
    XDG_DATA_HOME=/var/lib/dh-waypoint/data

COPY --from=build /build/dh-waypoint /usr/bin/dh-waypoint
USER 1000:1000
WORKDIR /var/lib/dh-waypoint
ENTRYPOINT ["dh-waypoint"]
