# One image: the Go service plus the Node + Chromium recorder it spawns.
FROM golang:1.23-bookworm AS build
WORKDIR /src
# ponytail: go.sum is absent because the service is stdlib-only; add it here the
# day a dependency appears.
COPY go.mod ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /jitsi-capture .

FROM node:22-bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends chromium \
    && rm -rf /var/lib/apt/lists/*

ENV PUPPETEER_SKIP_DOWNLOAD=1 \
    PUPPETEER_EXECUTABLE_PATH=/usr/bin/chromium \
    DATA_DIR=/data \
    RECORDER_PATH=/app/recorder/record.js \
    LISTEN_ADDR=:8080

WORKDIR /app

COPY recorder/package.json recorder/package-lock.json ./recorder/
RUN npm ci --omit=dev --prefix recorder
COPY recorder/ ./recorder/
COPY --from=build /jitsi-capture /app/jitsi-capture

# ponytail: runs as root — Chromium already gets --no-sandbox from the recorder
# and the /data bind mount stays root-owned; a non-root user is a later polish.
EXPOSE 8080
ENTRYPOINT ["/app/jitsi-capture"]
