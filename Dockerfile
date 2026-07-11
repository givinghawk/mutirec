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
# rclone provides optional Dropbox/Google Drive/S3-compatible backups;
# yt-dlp powers the on-demand YouTube/video-set download tool (a separate
# tool from streamlink - streamlink is for live streams, yt-dlp is for
# already-published videos/playlists).
ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl ffmpeg streamlink rclone tzdata \
    mesa-va-drivers intel-media-va-driver-non-free vainfo \
    && curl -fL https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp -o /usr/local/bin/yt-dlp \
    && chmod a+rx /usr/local/bin/yt-dlp \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system app \
    && useradd --system --gid app --create-home --home-dir /home/app --shell /usr/sbin/nologin app \
    # VAAPI/QSV need access to /dev/dri, whose device nodes on the host are
    # normally owned by the "video"/"render" groups - add app to whatever
    # this image's own video/render groups are, on top of docker-compose.yml
    # granting the host's actual GIDs via group_add for the common case
    # where they differ (which they usually do).
    && (getent group video || groupadd --system video) \
    && (getent group render || groupadd --system render) \
    && usermod -aG video,render app \
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
