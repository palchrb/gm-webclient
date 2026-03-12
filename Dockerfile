FROM golang:1-alpine3.23 AS builder

RUN apk add --no-cache git ca-certificates build-base su-exec olm-dev

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Re-apply replace in case go.mod was overwritten by COPY . .
RUN go mod edit -replace github.com/slush-dev/garmin-messenger=/garmin-messenger/lib/go

# Robust fix:
#  - Remove possible CRLF line endings
#  - Ensure executable bit
#  - Execute build script
RUN sed -i 's/\r$//' ./build.sh \
 && chmod +x ./build.sh \
 && ./build.sh

FROM alpine:3.23

ENV UID=1337 \
    GID=1337

RUN apk add --no-cache su-exec ca-certificates olm bash yq-go ffmpeg

COPY --from=builder /build/matrix-garmin-messenger /usr/bin/matrix-garmin-messenger
COPY --from=builder /build/docker-run.sh /docker-run.sh

RUN sed -i 's/\r$//' /docker-run.sh \
 && chmod +x /docker-run.sh

VOLUME /data

CMD ["/docker-run.sh"]
