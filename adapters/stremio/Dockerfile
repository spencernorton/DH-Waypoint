FROM golang:1.26.11-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY adapters/stremio ./adapters/stremio
RUN CGO_ENABLED=0 go build -trimpath -o /dh-waypoint-stremio ./adapters/stremio/cmd/dh-waypoint-stremio

FROM alpine:3.23
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 adapter
COPY --from=build /dh-waypoint-stremio /usr/local/bin/dh-waypoint-stremio
USER 10001:10001
ENTRYPOINT ["dh-waypoint-stremio"]
