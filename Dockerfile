# Multi-stage build: compile in golang, run in alpine (minimal, but has curl for healthchecks)
FROM golang:1.21-alpine AS builder

RUN apk add --no-cache ca-certificates

WORKDIR /app
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /app/server ./cmd/api

FROM alpine:3.20

# curl is required by the compose healthcheck: ["CMD", "curl", "-f", "http://localhost:8083/health"]
RUN apk add --no-cache ca-certificates curl

COPY --from=builder /app/server /server

VOLUME ["/var/data/sqrll/media"]
EXPOSE 8083

HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
    CMD ["curl", "-f", "http://127.0.0.1:8083/health"]

ENTRYPOINT ["/server"]
