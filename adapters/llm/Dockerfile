FROM golang:1.26.10-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY processorapi ./processorapi
COPY adapters/llm ./adapters/llm
RUN CGO_ENABLED=0 go build -trimpath -o /dh-waypoint-llm ./adapters/llm/cmd/dh-waypoint-llm

FROM alpine:3.23
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 processor
COPY --from=build /dh-waypoint-llm /usr/local/bin/dh-waypoint-llm
USER 10001:10001
ENTRYPOINT ["dh-waypoint-llm"]
