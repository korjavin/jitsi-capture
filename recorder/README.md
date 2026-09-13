# recorder — headless Jitsi audio recorder

`record.js` drives a headless Chromium into a Jitsi room as a muted, camera-off
participant, records the tab's incoming (mixed) audio and writes a single
WebM/Opus file. It is launched as a subprocess by the caller service; the CLI
below is the contract.

## Usage

```bash
node record.js --url <https://jitsi.example.com/ROOM> --out <path/audio.webm> \
  [--join-timeout <sec, default 600>] \
  [--max-duration <sec, default 14400>] \
  [--empty-grace <sec, default 60>] \
  [--display-name <str, default NoteTaker>]
```

The parent directory of `--out` is created if missing.

### stdout

Exactly one JSON line, on success only:

```json
{"out":"/data/audio.webm","duration_s":114.1,"reason":"empty_room","participants":["Alice","Bob"]}
```

* `reason` — `empty_room` | `signal` | `max_duration`
* `participants` — display names of non-bot participants seen at any point
  during the recording, deduped, first-seen order. Hidden participants
  (transcriber/SIP ghosts) and nameless ones are omitted.

All logs go to **stderr**, each line prefixed with an ISO timestamp. The room
name is logged, never the full URL — it may carry a JWT or a password.

### Exit codes

| code | meaning |
|------|---------|
| 0 | recorded OK; file exists and is non-empty |
| 2 | bad arguments (usage on stderr) |
| 3 | never got into the conference within `--join-timeout` (incl. never admitted from the lobby), or stopped by a signal before joining |
| 4 | browser launch / page failure |
| 5 | finished, but the output file is missing or empty |

### Signals

`SIGTERM` / `SIGINT` stop gracefully: the recording is finalized, the JSON line
is printed with `"reason":"signal"` and the process exits 0. A second signal
exits immediately.

## Output format

WebM/Opus (48 kHz stereo) exactly as Chrome's `MediaRecorder` produces it — no
ffmpeg, no WAV conversion. faster-whisper decodes it through PyAV directly.
`ffprobe` reports `Duration: N/A` on these files (a live MediaRecorder stream
has no seek cues); that is normal and decoders still read every frame. Use the
`duration_s` field from the JSON line.

Audio is the **mixed** conference stream, one track for everybody. Per-speaker
tracks are a separate piece of work.

## Environment

* `PUPPETEER_EXECUTABLE_PATH` — Chromium binary. Set in the Docker image
  (`/usr/bin/chromium`); if unset, puppeteer's own downloaded browser is used.
* Chromium is launched with `--no-sandbox` and
  `--autoplay-policy=no-user-gesture-required`. In Docker give the container
  `shm_size: 512m` or larger, or Chromium will crash on longer calls.

`puppeteer-stream` captures through a Chrome extension, so it only works in the
**new** headless mode — the code passes `headless: 'new'` literally, which is
the only value the library honours (anything else, `true` included, silently
launches headed). If a future Chromium drops extension support in headless
mode, the fallback is `xvfb-run -a node record.js …` with `headless: false`;
that needs `xvfb` in the image, so prefer keeping new-headless working.

## Joining, lobbies and the meet.jit.si moderator wall

Join config is passed in the URL hash (`config.prejoinConfig.enabled=false`,
`startWithAudioMuted`, `startWithVideoMuted`, `userInfo.displayName`), so the
bot never clicks the UI. The join phase then polls Jitsi's internal `window.APP`
every 2 s — all of it in `readJitsiState()`, so a Jitsi UI change is a one-place
fix. Verified against live Jitsi on **2026-09-13**:

* `APP.conference.isJoined()` → joined
* `APP.store.getState()['features/lobby'].knocking` → parked in the lobby,
  logged as `waiting_in_lobby`
* `APP.conference.membersCount` → participant count **including** the bot
* `APP.conference.listMembers()` → remote participants only, `getDisplayName()`

Anything that is not "joined" counts as waiting until `--join-timeout` expires,
then exit 3. That deliberately covers the public **meet.jit.si** case: an
unauthenticated client creating a fresh room is put in the lobby with
`knocking: true`, `membersOnly: true` and the message *"The conference has not
yet started because no moderators have yet arrived"* — indistinguishable from a
normal lobby, and it clears the moment a human moderator arrives and admits the
bot. There is no rejected/kicked detection: Jitsi signals that only through a
transient notification, so a rejection falls through to the same timeout.

## Stopping

The record phase polls every 2 s and stops on the first of:

* `membersCount <= 1` (only the bot left) continuously for `--empty-grace`
  seconds — note that Jitsi's count can lag ~30–60 s when a participant's
  browser dies instead of leaving cleanly, so the real stop can come later than
  the grace period alone suggests;
* `--max-duration` reached;
* `SIGTERM` / `SIGINT`.

The decision itself is the pure, unit-tested `shouldStop()`.

## Tests

```bash
PUPPETEER_SKIP_DOWNLOAD=1 npm ci
npm test        # node --check record.js && node --test
```

`parseArgs`, `buildUrl`, `roomName` and `shouldStop` are exported and covered.
No browser is launched and no network is touched; the browser paths are
verified manually against a throwaway room.
