# ── Build stage ──────────────────────────────────────────────────────────────
FROM golang:1.22-alpine AS builder

WORKDIR /src

# Copy module files and download deps first (layer cache)
COPY go.mod ./
RUN go mod download

# Copy source and build a fully static binary for linux/arm64
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
    go build -ldflags="-s -w" -o /hls-monitor .

# ── Runtime stage ─────────────────────────────────────────────────────────────
FROM scratch

# Pull in CA certificates so HTTPS works from the scratch image
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Copy the static binary
COPY --from=builder /hls-monitor /hls-monitor

# ── Environment defaults (override with -e or docker-compose) ─────────────────
ENV HLS_URL="https://stream.studiorhedenlive.nl/hlstv/stream.m3u8"
ENV ALERT_URL="https://uptime.fardau.com/api/push/mGIWEeKvvT1cFdqjMdesZph3v8hxtHfl?status=up&msg=HLSTVStreamFail&ping="
ENV SEGMENT_TIMEOUT="15s"
ENV FAIL_COOLDOWN="60s"

ENTRYPOINT ["/hls-monitor"]
