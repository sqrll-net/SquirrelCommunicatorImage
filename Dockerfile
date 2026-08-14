# Multi-stage build: compile in golang, run in scratch
FROM golang:1.21-alpine AS builder

RUN apk add --no-cache ca-certificates

WORKDIR /app
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /app/server ./cmd/api

FROM scratch

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /app/server /server

VOLUME ["/var/data/sqrll/media"]
EXPOSE 8083

ENTRYPOINT ["/server"]
