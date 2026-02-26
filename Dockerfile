FROM golang:1-alpine3.23 AS builder

RUN apk add --no-cache git ca-certificates build-base su-exec olm-dev

WORKDIR /build

# The garmin-messenger Go library lives in lib/go/ of the upstream repo but is
# not properly published to the Go module proxy (tags are on the repo root,
# which has no Go files). We clone at the tagged commit and redirect the module
# via a replace directive so that go mod download resolves it correctly.
ARG GARMIN_TAG=v1.2.7
RUN git clone --depth=1 --branch ${GARMIN_TAG} \
    https://github.com/slush-dev/garmin-messenger.git /garmin-messenger

COPY go.mod go.sum ./
RUN go mod edit -replace github.com/slush-dev/garmin-messenger=/garmin-messenger/lib/go
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
