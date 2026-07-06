# syntax=docker/dockerfile:1.7

# ---- Build stage ----
FROM golang:1.26-alpine AS builder

WORKDIR /src

# Cache dependencies first.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG APP=web
RUN CGO_ENABLED=0 go build \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/mutirec ./cmd/${APP}

# ---- Runtime stage ----
FROM ubuntu:22.04

# streamlink handles Twitch/YouTube/etc; FFmpeg records/transcodes raw streams;
# rclone provides optional Dropbox/Google Drive/S3-compatible backups.
ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates ffmpeg streamlink rclone tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system app \
    && useradd --system --gid app --create-home --home-dir /home/app --shell /usr/sbin/nologin app \
    && mkdir -p /app /home/app/.config/rclone \
    && chown -R app:app /app /home/app

WORKDIR /app
COPY --from=builder /out/mutirec /usr/local/bin/mutirec

USER app

ENV HTTP_ADDR=:8080
ENV CONFIG_PATH=/data/config/config.json
ENV FINISHED_DIR=/data/recordings
ENV TEMP_DIR=/data/incomplete
ENV LOG_DIR=/data/logs
VOLUME ["/data"]
EXPOSE 8080

ENTRYPOINT ["mutirec"]
