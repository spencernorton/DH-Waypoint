FROM golang:1.26.11-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY adapters/prowlarr-sync ./adapters/prowlarr-sync
RUN CGO_ENABLED=0 go build -trimpath -o /dh-waypoint-prowlarr-sync ./adapters/prowlarr-sync/cmd/dh-waypoint-prowlarr-sync

FROM alpine:3.23
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 adapter
COPY --from=build /dh-waypoint-prowlarr-sync /usr/local/bin/dh-waypoint-prowlarr-sync
USER 10001:10001
ENTRYPOINT ["dh-waypoint-prowlarr-sync"]
