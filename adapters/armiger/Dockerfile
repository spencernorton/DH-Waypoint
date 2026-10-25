FROM golang:1.26.10-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY processorapi ./processorapi
COPY adapters/armiger ./adapters/armiger
RUN CGO_ENABLED=0 go build -trimpath -o /dh-waypoint-armiger ./adapters/armiger/cmd/dh-waypoint-armiger

FROM alpine:3.23
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 processor
COPY --from=build /dh-waypoint-armiger /usr/local/bin/dh-waypoint-armiger
USER 10001:10001
ENTRYPOINT ["dh-waypoint-armiger"]
