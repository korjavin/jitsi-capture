# One image: Node + Chromium (recorder) and Python (transcriber/bot).
# ponytail: no ENTRYPOINT/volumes yet - the compose bead owns runtime wiring.
FROM python:3.12-slim

ENV PUPPETEER_SKIP_DOWNLOAD=1 \
    PUPPETEER_EXECUTABLE_PATH=/usr/bin/chromium \
    PIP_NO_CACHE_DIR=1 \
    PYTHONUNBUFFERED=1

RUN apt-get update \
    && apt-get install -y --no-install-recommends chromium nodejs npm \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY recorder/package.json recorder/package-lock.json ./recorder/
RUN npm ci --omit=dev --prefix recorder

COPY transcriber/requirements.txt ./transcriber/
RUN pip install -r transcriber/requirements.txt

COPY recorder/ ./recorder/
COPY transcriber/ ./transcriber/
