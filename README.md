# jitsi-capture

Records **Jitsi Meet** calls started from **Zulip**, on request, entirely on your
own hardware. A participant clicks a 🎙️ reaction, a headless bot joins the call,
and the audio lands on disk and is handed to the next service by a signed
webhook. No audio ever leaves the local perimeter.

---

## 1. What this is

`jitsi-capture` is the **first of three services**:

```text
Zulip 🎙️ click
  -> jitsi-capture        records the Jitsi call, audio under DATA_DIR
  -> signed webhook `recording.finished`
  -> transcriber          CPU transcription
  -> Anarlog-format webhook
  -> tr2outline           publishes the transcript into Outline
  -> callback POST /notify on jitsi-capture
  -> "transcript ready" message in the original Zulip topic
```

This repository does **only** the first box: the Zulip bot (reaction flow),
running the Node recorder as a child process, persisting job state and audio on
disk, sending the webhook, and serving `/notify` + `/health`. **No transcription
and no Outline here.**

Sibling repositories:

* [`korjavin/transcriber`](https://github.com/korjavin/transcriber) — takes
  `recording.finished` and transcribes the audio on CPU.
* [`korjavin/tr2outline`](https://github.com/korjavin/tr2outline) — publishes the
  finished transcript into Outline and calls back.

---

## 2. What it looks like in Zulip

An unobtrusive, semi-automatic flow: no service messages in the chat, and no
accidental recordings of short calls.

1. **Call detection.** The bot is subscribed to the Zulip event queue. When
   someone starts a video call, Zulip posts a message with a link to the Jitsi
   room. The bot matches it against `JITSI_BASE_URL` and extracts the room URL
   and the message id.
2. **A quiet offer.** The bot posts **no text**. It adds a 🎙️
   (`studio_microphone`) reaction to the call message itself.
3. **Recording on a click.** Anyone who wants a transcript clicks that reaction.
   The bot starts the recorder for the extracted URL and adds a 🔴
   (`red_circle`) reaction as a "recording now" indicator. A second click while
   the same job runs is a silent no-op.
4. **Admission.** The bot joins as `NoteTaker`, muted and camera-off. On a
   lobby-enabled room (public `meet.jit.si` included) a **human has to admit it**
   — until then it waits, up to `JOIN_TIMEOUT_S`.
5. **Nobody clicks.** Nothing happens, nothing is recorded, no resources spent.
6. **Completion.** When everyone leaves, the recorder finalizes the file, the 🔴
   indicator is removed and the webhook goes out. The "transcript ready" message
   arrives later, in the same topic, via `POST /notify` from downstream.

Failures are reported as one English line in the job's topic:

| cause | message |
|---|---|
| `not_admitted` | NoteTaker was not admitted to the call (or nobody joined) — nothing recorded. |
| `recorder_failed` | Recording failed (recorder error) — nothing recorded. |
| `too_short` | Recording too short (under 15 s) — nothing to transcribe. |
| `interrupted` | Recording was interrupted by a service restart — no transcript. |

---

## 3. How the recording works (research findings)

### Joining a Jitsi call and recording its audio

* **Why not Jitsi Jibri:** Jibri is hard-wired to the internal XMPP control
  protocol of its own Jitsi server. For joining a public `meet.jit.si` room as a
  guest it is unusable and excessively heavy.
* **Chosen approach: headless Chromium + `puppeteer-stream` (Node.js).**
  It captures the page's incoming audio stream directly through the Chrome
  DevTools / Extension API, with no virtual display (Xvfb) and no virtual audio
  server (PulseAudio).
* **Automatic join without clicking through the UI:** Jitsi settings are passed
  straight in the URL hash, so the bot never depends on the Jitsi UI:

  ```text
  https://meet.jit.si/<ROOM_ID>#config.prejoinConfig.enabled=false&config.startWithAudioMuted=true&config.startWithVideoMuted=true&userInfo.displayName="NoteTaker"
  ```

* **Detecting the end of the meeting:** the recorder polls Jitsi's internal
  `window.APP` every 2 s. When the bot is the only one left for `EMPTY_GRACE_S`
  seconds — or `MAX_DURATION_S` is reached, or a `SIGTERM` arrives — the
  recording is finalized.
* **Output:** WebM/Opus exactly as Chrome's `MediaRecorder` produces it, no
  ffmpeg step. `ffprobe` reports `Duration: N/A` on such files; use the
  `duration_s` field from the recorder's JSON line instead.

### Per-participant tracks

Beyond the mixed conference audio, the recorder is gaining **per-participant
tracks**: one file per speaker, each with its offset into the call. They surface
as the optional `tracks` array in the recorder's JSON line, in `job.json` and in
the webhook payload — consumers must tolerate its absence. See
[`recorder/README.md`](recorder/README.md) for the recorder's full contract.

---

## 4. Contracts

### (a) Outgoing webhook `recording.finished`

Sent to `WEBHOOK_URL` once a recording finishes successfully. Headers:

```text
x-jitsi-capture-event: recording.finished
x-jitsi-capture-signature: sha256=<hex>
Content-Type: application/json
```

The signature is `HMAC-SHA256(raw body, WEBHOOK_SECRET)`, lowercase hex. Body:

```json
{
  "event": "recording.finished",
  "id": "123456789",
  "message_id": 123456789,
  "stream": "some-stream",
  "topic": "some topic",
  "jitsi_url": "https://meet.jit.si/SomeRoom",
  "audio_path": "/srv/jitsi-capture/data/jobs/123456789/audio.webm",
  "duration_s": 1834.2,
  "started_at": "2026-01-01T10:00:00Z",
  "ended_at": "2026-01-01T10:30:34Z",
  "participants": ["Alice", "Bob"],
  "callback_url": "http://jitsi-capture:8080/notify",
  "tracks": [
    {"id": "p1", "name": "Alice", "path": "/srv/jitsi-capture/data/jobs/123456789/tracks/p1.webm", "offset_s": 0, "ended_s": 1834.2}
  ]
}
```

`audio_path` (and every `tracks[].path`) is a **host** path — `DATA_DIR` rebased
onto `HOST_DATA_DIR` — so the receiving service reads the file through its own
bind mount. `tracks` is omitted when the recorder produced no per-speaker files.

Verifying the signature:

```bash
# $BODY is the exact raw request body
printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$WEBHOOK_SECRET" -r
# -> <hex>  compare with the header value after "sha256="
```

Delivery retries on `5 s, 15 s, 45 s, 2 min, 5 min`; anything still undelivered
is retried by the hourly sweep and after a restart. The receiver must be
idempotent on `id`.

### (b) `POST /notify`

The callback downstream uses to put a message into the job's Zulip topic.

```http
POST /notify
x-jitsi-capture-signature: sha256=<hex>
Content-Type: application/json

{"id": "123456789", "content": "Transcript ready: <link>"}
```

Same HMAC over the raw body, same `WEBHOOK_SECRET`. `content` is posted verbatim
into the stream/topic of job `id`. Responses: `200 {"status":"ok"}` ·
`400` missing fields · `401` bad signature · `404` unknown job · `502` Zulip
refused the message.

### (c) `GET /health`

`200 {"status":"ok"}` as soon as the process is serving. No dependency checks.

### (d) State on disk

```text
DATA_DIR/jobs/<id>/job.json      # id = the Zulip message id
DATA_DIR/jobs/<id>/audio.webm    # mixed conference audio
DATA_DIR/jobs/<id>/tracks/       # per-participant files, when available
```

`job.json` carries `state` (`recording` | `finished` | `failed`), `error`,
timings, `participants`, `audio_path`, `tracks` and `webhook_sent_at`. It is
written atomically (temp file + rename), so a crash never leaves a half-written
record.

* **Retention:** the hourly sweep deletes job directories older than
  `AUDIO_RETENTION_DAYS` (a job still `recording` is never touched).
* **Restart:** on startup, jobs left in `recording` are marked `failed` /
  `interrupted` and reported in Zulip; every `finished` job without
  `webhook_sent_at` is delivered again. The same sweep re-checks hourly.

---

## 5. Environment variables

`config.go` is the single reader of the environment; there are no flags and no
dotenv loading — Compose passes `.env` through `env_file`.

| Variable | Default | Meaning |
|---|---|---|
| `ZULIP_SITE` | — *(required)* | Zulip base URL, no trailing slash |
| `ZULIP_BOT_EMAIL` | — *(required)* | Generic bot's email |
| `ZULIP_BOT_API_KEY` | — *(required)* | Generic bot's API key |
| `JITSI_BASE_URL` | `https://meet.jit.si` | Only links under this URL are offered a recording |
| `DATA_DIR` | `/data` | Container path; jobs live in `DATA_DIR/jobs/<id>/` |
| `HOST_DATA_DIR` | = `DATA_DIR` | Host path of the bind mount; used for `audio_path` |
| `RECORDER_PATH` | `recorder/record.js` | Node recorder (the image sets `/app/recorder/record.js`) |
| `BOT_DISPLAY_NAME` | `NoteTaker` | Display name in the call |
| `JOIN_TIMEOUT_S` | `600` | Give up if not admitted within this many seconds |
| `MAX_DURATION_S` | `14400` | Hard cap on one recording |
| `EMPTY_GRACE_S` | `60` | Stop after this long alone in the room |
| `MIN_RECORDING_S` | `15` | Shorter recordings are reported as `too_short` |
| `AUDIO_RETENTION_DAYS` | `7` | Sweep deletes older jobs; `0` or less keeps everything |
| `WEBHOOK_URL` | *(empty)* | Empty disables the webhook (logs "webhook disabled") |
| `WEBHOOK_SECRET` | — | Required when `WEBHOOK_URL` is set; also verifies `POST /notify` |
| `LISTEN_ADDR` | `:8080` | HTTP listen address |
| `PUBLIC_URL` | `http://localhost:8080` | `callback_url` = `PUBLIC_URL` + `/notify` |
| `LOG_LEVEL` | `INFO` | `DEBUG` \| `INFO` \| `WARN` \| `ERROR` |

Secrets are never logged — only the variable **name** appears in an error.

---

## 6. Deployment

One image contains the Go service, Node and Chromium; `docker-compose.yml` is
the whole deployment.

### Zulip bot

1. Settings → Personal → **Bots** → *Add a new bot*, type **Generic**. Note the
   bot email and API key.
2. **Subscribe the bot** to every stream whose calls should be recordable — it
   only sees messages in streams it is subscribed to.
3. The bot needs no admin rights: it reads messages and adds reactions.

### Run it locally

```bash
cp .env.example .env     # fill in ZULIP_*, WEBHOOK_*, HOST_DATA_DIR, DOMAIN
docker build -t jitsi-capture .
docker compose up -d
docker compose logs -f
```

`docker-compose.yml` expects an existing external Traefik network
(`TRAEFIK_NETWORK_NAME`, default `traefik`) — it publishes no ports of its own,
Traefik fronts the service on `DOMAIN`.

### Automated deployment (GitHub Actions → ghcr.io → Portainer)

`.github/workflows/deploy.yml` runs on every push to `master` (and on
`workflow_dispatch`):

1. builds the image and pushes it to `ghcr.io/korjavin/jitsi-capture:<sha>`;
2. checks out a `deploy` branch, rewrites the `image:` line in
   `docker-compose.yml` with that SHA tag, commits `[skip ci]` and force-pushes
   `deploy`;
3. calls the Portainer redeploy webhook stored in the repository secret
   `PORTAINER_REDEPLOY_HOOK` (skipped when the secret is empty).

`master` keeps `image: ghcr.io/korjavin/jitsi-capture:latest` as a placeholder;
only the `deploy` branch carries an immutable SHA tag. **Point the Portainer
git-ops stack at branch `deploy`**, never at `master`, and paste the webhook URL
Portainer generates into the `PORTAINER_REDEPLOY_HOOK` secret.

Portainer pulls the image, so no `.env` file exists on the node —
`docker-compose.yml` passes every variable through from the stack environment.
Set these in the stack:

| Variable | Notes |
| --- | --- |
| `ZULIP_SITE`, `ZULIP_BOT_EMAIL`, `ZULIP_BOT_API_KEY` | required |
| `JITSI_BASE_URL` | required |
| `WEBHOOK_URL`, `WEBHOOK_SECRET` | transcriber endpoint + shared secret |
| `PUBLIC_URL` | how the transcriber reaches this service (`callback_url` prefix) |
| `HOST_DATA_DIR` | **host path on the Portainer node** for the bind mount |
| `DATA_DIR` | container path (default `/data`) |
| `DOMAIN` | public hostname Traefik routes to this service |
| `TRAEFIK_NETWORK_NAME` | existing external Traefik network (default `traefik`) |
| `TRAEFIK_CERTRESOLVER` | Traefik ACME resolver (default `myresolver`) |
| `AUDIO_RETENTION_DAYS`, `LOG_LEVEL`, … | optional, see [§5](#5-environment-variables) |

The parts that matter:

* **`HOST_DATA_DIR` bind mount** — job state and audio must survive a redeploy.
  It is a path on the Portainer node, not in this repository; create it there
  first (`mkdir -p /srv/jitsi-capture/data`).
* **`shm_size: 1g`** — Chromium crashes on longer calls with Docker's 64 MB
  default `/dev/shm`.
* **`stop_grace_period: 120s`** — lets an in-flight recording finalize its file
  and deliver its webhook on `SIGTERM`. Do not lower it.
* **RAM** — roughly 400–800 MB per concurrent recording (one Chromium each).
* Port `8080` is reached through Traefik, or directly by the sibling
  `transcriber` / `tr2outline` on the shared Docker network.

### Smoke checklist

1. Start a call in a subscribed Zulip stream (the call button posts the link).
2. A 🎙️ reaction appears on the message within a second or two.
3. Click it → a 🔴 reaction appears.
4. **Admit `NoteTaker`** from the Jitsi lobby (a human has to do this).
5. Talk for more than 15 seconds, then everyone leaves the call.
6. `HOST_DATA_DIR/jobs/<message-id>/job.json` shows `"state": "finished"` with a
   non-zero `duration_s`, next to `audio.webm`; the receiver logs the
   `recording.finished` webhook. The 🔴 reaction is gone.

---

## 7. Development

```bash
# Go service
gofmt -l . && go vet ./... && go test -race ./...

# Node recorder (PUPPETEER_SKIP_DOWNLOAD=1 avoids a ~150 MB Chrome download)
cd recorder && PUPPETEER_SKIP_DOWNLOAD=1 npm ci && npm test

# Image + compose file
docker build -t jitsi-capture .
cp .env.example .env && docker compose config -q
```

Tests run offline — no Zulip, no Jitsi, no network: HTTP boundaries use
`net/http/httptest` and the recorder subprocess is a fake shell script. CI
(`.github/workflows/ci.yml`) runs the same three jobs (`go` / `node` / `docker`)
on every pull request. The Go side is a flat `package main` at the repository
root and is **stdlib-only** — no new dependencies.
