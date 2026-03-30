# Build stage
FROM golang:1.24-alpine AS builder

ARG VERSION=dev
ARG COMMIT=unknown

RUN apk add --no-cache git

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN CGO_ENABLED=0 go build \
    -ldflags "-s -w -X main.Version=${VERSION} -X main.Commit=${COMMIT} -X main.BuildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    -o /garmin-messenger-relay \
    ./cmd/garmin-messenger-relay

# Runtime stage
FROM alpine:3.21

RUN apk add --no-cache ca-certificates ffmpeg tzdata su-exec

COPY --from=builder /garmin-messenger-relay /usr/local/bin/garmin-messenger-relay
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

RUN addgroup -S relay && adduser -S relay -G relay

WORKDIR /data
VOLUME ["/data"]

ENTRYPOINT ["docker-entrypoint.sh"]
CMD ["run", "-config", "/data/config.yaml"]
