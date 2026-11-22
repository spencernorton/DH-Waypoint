# DH-TMS companion sidecar image (non-root UID 1000, multi-arch Alpine).
FROM rust:1-alpine3.23@sha256:9577a4edf61be25721f5fa4006ad463b83ea52c0a17c6e6b673c72c794aee738 AS build

# musl-dev provides C toolchain headers needed by bundled zstd-sys.
RUN apk --no-cache add musl-dev

WORKDIR /src
COPY Cargo.toml Cargo.lock ./
COPY crates ./crates

RUN cargo build --release --locked --bin dh-tmsd

FROM alpine:3.23.3@sha256:25109184c71bdad752c8312a8623239686a9a2071e8825f20acb8f2198c3f659

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=1970-01-01T00:00:00Z
ARG SOURCE=https://github.com/spencernorton/DH-Waypoint
LABEL org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.source="${SOURCE}"

# curl supports the Compose health probe; tzdata aligns container timestamps.
RUN apk --no-cache add curl tzdata \
 && addgroup -g 1000 -S dhtms \
 && adduser -u 1000 -S -G dhtms -h /var/lib/dh-tms dhtms \
 && mkdir -p /var/lib/dh-tms/db /var/lib/dh-tms/wal /var/lib/dh-tms/run \
 && chown -R 1000:1000 /var/lib/dh-tms

COPY --from=build /src/target/release/dh-tmsd /usr/bin/dh-tmsd
USER 1000:1000
WORKDIR /var/lib/dh-tms
EXPOSE 7841
ENTRYPOINT ["/usr/bin/dh-tmsd"]
CMD ["serve", "--db-root", "/var/lib/dh-tms/db", "--wal-dir", "/var/lib/dh-tms/wal", "--listen", "0.0.0.0:7841"]
