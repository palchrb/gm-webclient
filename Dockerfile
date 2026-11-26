# Pin the Go major explicitly. Must be >= 1.27.3 / 1.26.10 for the crypto/x509
# block of the compromised eMudhra CA keys (CVE-2026-111943); the floating
# 1.27 tag picks up later patch releases on every rebuild.
FROM golang:1.27-alpine3.23 AS builder

RUN apk add --no-cache git ca-certificates build-base

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN go build -ldflags="-s -w" -o garmin-web ./cmd/garmin-web

FROM alpine:3.24

# apk upgrade pulls the newest ca-certificates (and other security fixes) even
# when the alpine:3.23 base layer in the build cache is older.
RUN apk upgrade --no-cache \
 && apk add --no-cache ca-certificates ffmpeg

COPY --from=builder /build/garmin-web /usr/bin/garmin-web

VOLUME /data
EXPOSE 8080

CMD ["/usr/bin/garmin-web", "-addr", ":8080", "-data-dir", "/data"]
