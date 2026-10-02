# Google Meet join spike (throwaway — do not merge into record.js)

Bead `jitsi2outline-8kp`. `meet-spike.js` tries to join a real Google Meet call
with Puppeteer Chromium and logs what the go/no-go report needs:

- every page state change (`loading`, `prejoin`, `lobby`, `admitted`, `denied`,
  `blocked`, `invalid`, `removed`, `ended`, `signin`, `unknown`), with the
  phrase that matched and the first 400 characters of page text;
- every incoming WebRTC track (`rtc` lines) and, every 5 s while in the call,
  the live remote audio track count plus per-SSRC `audioLevel` (`media` lines).
  This shows whether Meet sends ~3 rotating "loudest speaker" streams;
- visible participant names, by three selector guesses, plus caption text with
  `--captions` (`names` lines);
- N seconds of all remote audio, mixed in the page and recorded with
  MediaRecorder, in `mixed.webm`.

The bot joins **without microphone or camera**: the script denies both
permissions, so it cannot send audio into the call.

Everything goes to `--out-dir` (default `./meet-spike-out`):
- `spike.log`: the log;
- `mixed.webm`: the audio;
- a `NNNN-<state>.png` screenshot and a `.txt` page-text dump at every state
  change, and one every 30 s in the call.

## Setup (once, on a Mac with a desktop)

```bash
cd recorder
npm ci                      # downloads Puppeteer's Chrome (~150MB)
# or reuse installed Chrome instead of the download:
#   PUPPETEER_SKIP_DOWNLOAD=1 npm ci
#   export PUPPETEER_EXECUTABLE_PATH="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
```

### Signed-in bot profile (once)

Use a dedicated bot Google account, never a personal one.

```bash
node spike/meet-spike.js --login --user-data-dir ~/meet-bot-profile
```

1. A Chrome window opens on accounts.google.com.
2. Sign the bot in and complete any 2FA.
3. Open meet.google.com once to check that you are signed in.
4. Close the window.

If Google says **"This browser or app may not be secure"**, that is itself a
finding: write it down. Then sign in with a plain, non-automated browser on the
same profile directory, and rerun the spike with `--user-data-dir ~/meet-bot-profile`:

```bash
"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" --user-data-dir="$HOME/meet-bot-profile"
```

Keep `~/meet-bot-profile` out of git. It holds the account's session cookies.

## The runs

Host a fresh Meet call, join it from a phone or a second browser, and talk
during the recording window. Use **two hosts**: (W) a Workspace account and
(E) an external or consumer account. For each host, run as many of these as you
have time for, in this order:

```bash
cd recorder
M='https://meet.google.com/xxx-yyyy-zzz'   # the real link — never commit it

# 1. anonymous guest, headless (the production shape)
node spike/meet-spike.js "$M" --seconds 90 --captions --out-dir /tmp/meet-W1
# 2. signed-in bot, headless
node spike/meet-spike.js "$M" --user-data-dir ~/meet-bot-profile --seconds 90 --captions --out-dir /tmp/meet-W2
# 3. only if 1 or 2 got blocked: the same run, headful (is it headless detection?)
node spike/meet-spike.js "$M" --user-data-dir ~/meet-bot-profile --headful --seconds 90 --out-dir /tmp/meet-W3
# 4. optional: stock Puppeteer, no evasions (do we need the evasions at all?)
node spike/meet-spike.js "$M" --plain --seconds 30 --out-dir /tmp/meet-W4
```

During each run:

- When the bot shows up in the lobby, admit it, with one exception. In one
  run, **deny** it instead, to capture the `denied` text.
- In another run, **end the call for everyone** while the bot is still
  recording, to capture the `ended` text.
- Have 2 or more humans speak, at least one of them in turns, and talk over
  each other once.

Ctrl-C stops a run cleanly. The audio recorded so far is kept.

Exit codes:

| Code | Meaning |
|---|---|
| 0 | recorded |
| 2 | bad arguments |
| 3 | never admitted (blocked, denied, timeout) |
| 4 | browser or script error |

### In Docker (the real deployment shape)

Optional. Run this after the Mac runs. The image already has Chromium and this
directory:

```bash
docker build -t jitsi-capture .
docker run --rm -v /tmp/meet-D1:/out --entrypoint node jitsi-capture \
  /app/recorder/spike/meet-spike.js "$M" --seconds 60 --out-dir /out
```

To reuse the signed-in profile in Docker, add `-v ~/meet-bot-profile:/profile`
and `--user-data-dir /profile`. A profile made by Mac Chrome may not decrypt its
cookies on Linux Chromium. If it doesn't, that is a finding too.

## What to paste back (into the bead notes)

Redact real names, emails and the meeting code first. This repo and its bead
history are public.

1. For each run, the host type (W/E), the command flags, and the final
   `SUMMARY` line.
2. Every `STATE` line.
3. A few `media` lines from a stretch where 2 or more people spoke. Note which
   SSRC levels moved for which speaker.
4. A few `names` lines. Include one with captions on, if you used `--captions`.
5. Whether `mixed.webm` plays, and whether every speaker is audible. Check
   with `ffprobe` or `afplay`, or open it in a browser.
6. Anything odd: an "unknown" state, a sign-in challenge, how long the profile
   stayed signed in. Attach the screenshot and `.txt` of that state.
