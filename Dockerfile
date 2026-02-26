FROM golang:1-alpine3.23 AS builder

RUN apk add --no-cache git ca-certificates build-base su-exec olm-dev

COPY . /build
WORKDIR /build
RUN go get github.com/slush-dev/garmin-messenger@main && \
    go mod tidy && \
    ./build.sh

FROM alpine:3.23

ENV UID=1337 \
    GID=1337

RUN apk add --no-cache su-exec ca-certificates olm bash yq-go ffmpeg

COPY --from=builder /build/matrix-garmin-messenger /usr/bin/matrix-garmin-messenger
COPY --from=builder /build/docker-run.sh /docker-run.sh

VOLUME /data

CMD ["/docker-run.sh"]
