FROM golang:1.26.11-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY processorapi ./processorapi
COPY adapters/match ./adapters/match
RUN CGO_ENABLED=0 go build -trimpath -o /dh-waypoint-match ./adapters/match/cmd/dh-waypoint-match

FROM alpine:3.23
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 processor
COPY --from=build /dh-waypoint-match /usr/local/bin/dh-waypoint-match
USER 10001:10001
ENTRYPOINT ["dh-waypoint-match"]
