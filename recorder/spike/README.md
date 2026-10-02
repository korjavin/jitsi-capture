# Google Meet join spike (throwaway — do not fold into record.js)

The deployed bot runs this script on a `meet-spike <url> [seconds=N]` DM (guest
mode, see the main README), so no local setup is needed for the guest runs.

Bead `jitsi2outline-8kp`. `meet-spike.js` tries to join a real Google Meet call
with Puppeteer Chromium and logs what the go/no-go report needs:

- every page state change (`loading`, `prejoin`, `lobby`, `admitted`, `denied`,
  `blocked`, `invalid`, `removed`, `ended`, `signin`, `unknown`), with the
  phrase that matched and the first 400 characters of page text;
- every incoming WebRTC track (`rtc` lines) and, every 5 s while in the call,
  the live remote audio track count plus, per inbound SSRC, `lvl` (audioLevel),
  `nrg` (totalAudioEnergy), `smp` (totalSamplesReceived) and `kB` (`media`
  lines). Also how many of Meet's `<audio>` elements and of the script's own
  are playing (`meetEls`/`ourEls`), `recRms` (peak RMS of what MediaRecorder
  actually records since the last line) and the saved caption count. Reading
  them: `kB` grows but `smp` stays 0 = nothing pulls (decodes) the track;
  `smp` grows with `nrg` 0 = Meet sends silence; `nrg` grows but `recRms` is 0 =
  the page-side mix is silent;
- visible participant names, by three selector guesses, plus caption stats with
  `--captions` (`names` lines: `on`, which parse `strategy` worked, `blocks`
  on screen, `saved` utterances);
- with `--captions`: captions turned on (toolbar button, then the `c`
  shortcut), each finished utterance saved once to `captions.jsonl` as
  `{"ts","speaker","text"}` (Meet rewrites the last line while someone talks;
  a line is saved once it stops changing or leaves the screen). If the
  captions region has text but nothing parses, one `markup sample` log line
  shows its HTML for fixing the selectors. `--lang <code>` tries to pick the
  caption language in Meet's settings (`captions lang:` lines). **Meet
  captions one spoken language per meeting**: a mixed-language call is
  captioned as if everything were in that one language;
- N seconds of all remote audio, mixed in the page and recorded with
  MediaRecorder, in `mixed.webm`. Every remote audio track is also played by a
  detached `<audio>` element: Chromium leaves a remote WebRTC track that only
  feeds WebAudio undecoded, which recorded digital silence in run 1.

### Audio: three capture methods in one run

Runs 1–2 recorded digital silence: inbound RTP flowed but `smp` stayed 0, and
Meet's prejoin said "Speaker not found" (the container has no audio output
device). Every run now tries all four at once, and the `AUDIO` line at the
end gives each one's level in dBFS (`null` = digital silence):

| Method | What | File |
|---|---|---|
| `pulse` | The script starts a private PulseAudio whose only output is a null sink, launches Chromium on it (`PULSE_SERVER`, without Puppeteer's default `--mute-audio`), and records the sink's monitor with `parec` — the PulseAudio + ffmpeg approach of most open-source and commercial meeting bots, with `parec` writing the WAV directly. Meet now sees one speaker. `rmsDb` is the whole file, `peakDb` the loudest 200 ms. | `monitor.wav` (16 kHz mono, ~1.9 MB/min) |
| `mix` | The page-side mix above, now with a real output device present. `peakDb` = loudest 200 ms. | `mixed.webm` |
| `tab` | Tab capture: `getDisplayMedia({audio: true, preferCurrentTab: true})` auto-accepted by `--auto-accept-this-tab-capture` (how screenappai/meeting-bot records Meet), recorded with MediaRecorder. `state` says whether it started. `peakDb` = loudest 200 ms. | `tab.webm` |
| `meetCtx` | Taps on every AudioContext of the page's own (Meet's) that sends audio to its speakers: run 2 saw no `<audio>` elements, so Meet may play through WebAudio. `ctxs` = how many such contexts. Level only. | — |

Extra diagnostics in the `media` lines: `tabRms` (the `tab` method), `pcs` (peer connections), `outs`
(audio output devices the page sees; 0 = "Speaker not found"), `conc`
(concealedSamples) and `jbe` (jitterBufferEmittedCount) per inbound SSRC,
`pageEls` (media elements the page played a stream with, attached to the DOM
or not), `taps`/`tapRms` (the `meetCtx` method). An `rtc` line per peer
connection says whether Meet asked for encoded insertable streams
(`encoded: true`). The bot DM uploads `mixed.webm`, `monitor.wav` and `tab.webm` (each up to 24 MB).

`audio-check.js` exercises these paths offline, without Meet: it launches
Chromium with the same fake devices as a Meet run, a localhost page loops a
beeping WebAudio tone through two peer connections and plays the remote
track through WebAudio, and the same hooks, recorder and null sink capture it.

```bash
docker run --rm --entrypoint node jitsi-capture /app/recorder/spike/audio-check.js            # with the null sink
docker run --rm --entrypoint node jitsi-capture /app/recorder/spike/audio-check.js --no-pulse # without
```

### Devices: fake, silent, and switched off (v4)

Run 3 showed call audio never reaching the decoder while prejoin said "Mic not
found / Speaker not found": the guest had no devices and no permissions. The
bot now looks like a person's browser with its mic and camera off:

- Chromium gets `--use-fake-ui-for-media-stream --use-fake-device-for-media-stream`
  with `--use-file-for-fake-audio-capture=<out>/fake-mic-silence.wav` (2 s of
  digital silence, looped) and `--use-file-for-fake-video-capture=<out>/fake-cam-black.y4m`
  (one black frame), and Meet's origin is granted `camera` + `microphone`.
  The page then sees 3 audio inputs, 1 video input and 3 audio outputs, even
  without PulseAudio.
- On the prejoin screen the script clicks every "Turn off microphone/camera"
  toggle and **never clicks join while either still reads on**. It checks
  again every 5 s in the call (`devices` lines: `mic`/`cam` = `on`/`off`/`?`,
  the enumerateDevices counts, permission states, and the page's own phrases
  about mic/camera/speaker).
- Belt and braces: every `getUserMedia` track is disabled and its `enabled`
  property is locked (Meet setting it to `true` is a no-op), so even a leaked
  track carries silence or a black frame. `devices in-call` lists every
  sender track and the bytes sent per kind.

Other bots: attendee and Vexa also pass `--use-fake-ui-for-media-stream`
(attendee adds `--use-fake-device-for-media-stream`; Vexa points the fake
camera at `/dev/null`) and turn the mic/camera off by their
`aria-label="Turn off ..."` buttons; screenappai/meeting-bot grants nothing
for Meet and clicks "Continue without microphone and camera".

**Caveat:** under `--use-fake-device-for-media-stream`, `getDisplayMedia`
returns Chromium's fake capture too, so the `tab` method records "Fake audio",
not Meet (its `state` shows the track labels). `pulse` already records
everything the browser plays, which includes the tab. (The pre-v4 offline
check had the same flag, so its `tab` level was the fake beep as well.)

More v4 diagnostics:

- `rtp` every second in the call: inbound bytes per second per peer
  connection and SSRC (`a…` audio, `v…` video) plus bytes sent; `rtp
  transceivers` whenever they change (mid, direction, what each receiver and
  sender holds).
- `meet audio`: Meet's own `<audio>` elements (muted, `muted` attribute,
  paused, volume, sinkId, track state) right after admission, where muted
  ones get unmuted (local playback only), and again 10 s later.
- `audio settings`: the text of Meet's Settings -> Audio dialog (selected
  speaker/microphone).
- `captions lang:` logs every step of the language switch (now opening the
  language list first, and listing the options on screen when the wanted one
  is missing), then whether captions are still on (`after switch`; if not,
  they are turned back on). `captions text` every 5 s: the raw caption
  region text (200 chars) and the block count.
- `AUDIO` gives each method a `speech` value: the fraction of seconds above
  -45 dBFS (pulse: the whole file; the others: since admission). Join chimes
  light up a second or two; speech lights up many.
- The bot DM uploads the whole `spike.log` too.

`audio-check.js` also proves the device setup offline: the fake mic file read
on a page without the hooks is digital silence, and on a hooked page the
tracks stay disabled after `enabled = true`.

Everything goes to `--out-dir` (default `./meet-spike-out`):
- `spike.log`: the log;
- `mixed.webm`: the page-side mix;
- `monitor.wav`: the PulseAudio null-sink monitor (when `pulseaudio` is installed, as in the image);
- `tab.webm`: the tab-capture audio;
- `captions.jsonl`: the captions (with `--captions`);
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
