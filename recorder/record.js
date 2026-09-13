#!/usr/bin/env node
'use strict';

// Headless Jitsi audio recorder: joins muted, waits (in the lobby if a human
// moderator has to admit it), records the tab's incoming audio to WebM/Opus and
// exits with a contract-defined code. With --tracks-dir it additionally records
// one WebM per remote participant. See recorder/README.md.
//
// Jitsi's `window.APP` is an internal global, not a public API. Every read of it
// lives in readJitsiState() and pollTracks() below — the two page-side
// functions — so a Jitsi UI change stays a one-place fix.
// Last checked against live Jitsi on 2026-09-13: the lobby/moderator-wall state
// on meet.jit.si, the joined/recording path on an open public deployment.

const fs = require('node:fs');
const path = require('node:path');
const { once } = require('node:events');

const USAGE = `usage: record.js --url <jitsi-meeting-url> --out <audio.webm>
                 [--tracks-dir <dir>]
                 [--join-timeout <sec, default 600>]
                 [--max-duration <sec, default 14400>]
                 [--empty-grace <sec, default 60>]
                 [--display-name <str, default NoteTaker>]
`;

const DEFAULTS = {
  url: '',
  out: '',
  joinTimeout: 600,
  maxDuration: 14400,
  emptyGrace: 60,
  displayName: 'NoteTaker',
};

// --tracks-dir is deliberately absent from DEFAULTS: without the flag the key
// stays undefined, which is both the "off" switch and what keeps the stdout
// line byte-identical to the mixed-audio-only contract.
const FLAGS = {
  '--url': 'url',
  '--out': 'out',
  '--tracks-dir': 'tracksDir',
  '--join-timeout': 'joinTimeout',
  '--max-duration': 'maxDuration',
  '--empty-grace': 'emptyGrace',
  '--display-name': 'displayName',
};

const NUMERIC = new Set(['joinTimeout', 'maxDuration', 'emptyGrace']);

const POLL_MS = 2000;
const FLUSH_MS = 5000;
// Grace for the last MediaRecorder chunk of every per-participant track to make
// it back over page.exposeFunction after we stop the recorders.
// ponytail: a fixed wait; the alternative is an ack protocol across the bridge.
const TRACK_FLUSH_MS = 1500;

/** Parse argv (without node/script). Throws on anything the contract rejects. */
function parseArgs(argv) {
  const opts = { ...DEFAULTS };
  for (let i = 0; i < argv.length; i++) {
    const flag = argv[i];
    const key = FLAGS[flag];
    if (!key) throw new Error(`unknown argument: ${flag}`);
    const value = argv[++i];
    if (value === undefined) throw new Error(`missing value for ${flag}`);
    if (NUMERIC.has(key)) {
      const n = Number(value);
      if (!Number.isFinite(n) || n <= 0) throw new Error(`${flag} must be a positive number`);
      opts[key] = n;
    } else {
      opts[key] = value;
    }
  }
  if (!opts.url) throw new Error('missing required --url');
  if (!opts.out) throw new Error('missing required --out');
  return opts;
}

/** Jitsi config goes in the URL hash, so the bot never has to click the UI. */
function buildUrl(url, displayName) {
  const hash = [
    'config.prejoinConfig.enabled=false',
    'config.startWithAudioMuted=true',
    'config.startWithVideoMuted=true',
    `userInfo.displayName=${encodeURIComponent(JSON.stringify(displayName))}`,
  ].join('&');
  return `${url.split('#')[0]}#${hash}`;
}

/** Room name only — the URL may carry a JWT or a password we must not log. */
function roomName(url) {
  try {
    return new URL(url).pathname.split('/').filter(Boolean).pop() || '(root)';
  } catch {
    return '(unparseable-url)';
  }
}

/**
 * Pure stop decision. Times (`now`, `aloneSince`, `startedAt`) are epoch ms;
 * `emptyGrace` / `maxDuration` are seconds. Returns the stop reason (or null)
 * plus the carried-forward `aloneSince`, which resets as soon as somebody else
 * is in the room again.
 */
function shouldStop({ membersCount, aloneSince, now, emptyGrace, startedAt, maxDuration }) {
  if (now - startedAt >= maxDuration * 1000) return { reason: 'max_duration', aloneSince };
  // APP.conference.membersCount counts the bot itself.
  const alone = membersCount <= 1;
  const since = alone ? (aloneSince ?? now) : null;
  if (alone && now - since >= emptyGrace * 1000) return { reason: 'empty_room', aloneSince: since };
  return { reason: null, aloneSince: since };
}

/**
 * Runs inside the page. No closures: puppeteer serializes this function.
 * Everything it touches is pre-join-undefined or throws until the conference
 * exists, so every read is guarded — a throw out of here means a dead page.
 */
function readJitsiState() {
  const app = window.APP;
  const conference = app && app.conference;
  const state = app && app.store && app.store.getState ? app.store.getState() : null;
  const lobby = state ? state['features/lobby'] : null;
  const read = (fn, fallback) => {
    try {
      const v = fn();
      return v === undefined || v === null ? fallback : v;
    } catch {
      return fallback;
    }
  };
  return {
    joined: read(() => !!conference.isJoined(), false),
    knocking: !!(lobby && lobby.knocking),
    // membersCount counts the bot itself; listMembers() is remote-only.
    membersCount: read(() => conference.membersCount, 0),
    participants: read(
      () =>
        conference
          .listMembers()
          .filter((m) => !m.isHidden())
          .map((m) => m.getDisplayName())
          .filter(Boolean),
      []
    ),
  };
}

/**
 * Runs inside the page; the second and last place that touches window.APP.
 * No closures: puppeteer serializes this function, so it is called afresh on
 * every poll and keeps its state on `window.__jc`.
 *
 * It attaches a MediaRecorder to each remote participant's audio track, drops
 * the ones whose owner left, and returns the events queued since the previous
 * call: {type:'start'|'end'|'name'|'speaker', id, name?, t?} with `t` in
 * seconds since the mixed recording started. `stop` finalizes every recorder.
 *
 * Every read is guarded the same way readJitsiState() is: a throw out of here
 * would mean a dead page, and per-participant audio must never cost us the
 * mixed recording.
 */
function pollTracks(startedAtMs, stop) {
  const st = (window.__jc = window.__jc || {
    active: new Map(),
    names: new Map(),
    events: [],
    dominant: null,
    ctx: null,
  });
  const at = () => (Date.now() - startedAtMs) / 1000;
  const read = (fn, fallback) => {
    try {
      const v = fn();
      return v === undefined || v === null ? fallback : v;
    } catch {
      return fallback;
    }
  };
  const state = () => window.APP.store.getState();
  const nameOf = (id) =>
    read(() => {
      const p = state()['features/base/participants'].remote.get(id);
      return p && (p.name || p.displayName);
    }, '') || '';

  const end = (id) => {
    const a = st.active.get(id);
    if (!a) return;
    st.active.delete(id);
    read(() => a.rec.stop());
    read(() => a.src.disconnect());
    st.events.push({ type: 'end', id, t: at() });
  };

  if (stop) {
    for (const id of [...st.active.keys()]) end(id);
    return st.events.splice(0);
  }

  // Remote audio streams by participant id. Somebody who joined muted has no
  // audio track yet — skip them and pick them up on a later poll.
  const streams = new Map();
  for (const t of read(() => state()['features/base/tracks'], []) || []) {
    if (!t || t.mediaType !== 'audio' || t.local || !t.participantId) continue;
    const s =
      read(() => t.jitsiTrack.getOriginalStream(), null) || read(() => t.jitsiTrack.stream, null);
    if (s && read(() => s.getAudioTracks().length, 0) > 0) streams.set(t.participantId, s);
  }

  for (const [id, s] of streams) {
    if (st.active.has(id)) continue;
    const started = read(() => {
      if (!st.ctx) st.ctx = new (window.AudioContext || window.webkitAudioContext)();
      // Needs --autoplay-policy=no-user-gesture-required, or it stays suspended.
      if (st.ctx.state === 'suspended') st.ctx.resume();
      const src = st.ctx.createMediaStreamSource(s);
      // Recording the remote track directly would stall the MediaRecorder clock
      // while the participant is muted; the AudioContext hop keeps it running.
      // Never connect to ctx.destination — that would echo into the tab capture.
      const dest = st.ctx.createMediaStreamDestination();
      src.connect(dest);
      const rec = new MediaRecorder(dest.stream, { mimeType: 'audio/webm;codecs=opus' });
      rec.ondataavailable = (e) => {
        if (!e.data || !e.data.size) return;
        e.data
          .arrayBuffer()
          .then((b) => {
            // exposeFunction only carries strings, so base64 it is.
            const u8 = new Uint8Array(b);
            let bin = '';
            for (let i = 0; i < u8.length; i++) bin += String.fromCharCode(u8[i]);
            return window.__trackChunk(id, btoa(bin));
          })
          .catch(() => {});
      };
      rec.start(1000);
      st.active.set(id, { rec, src });
      return true;
    }, false);
    if (started) st.events.push({ type: 'start', id, name: nameOf(id), t: at() });
  }
  for (const id of [...st.active.keys()]) if (!streams.has(id)) end(id);

  // A display name often lands after the track does; report it when it changes.
  for (const id of st.active.keys()) {
    const name = nameOf(id);
    if (name && name !== st.names.get(id)) {
      st.names.set(id, name);
      st.events.push({ type: 'name', id, name });
    }
  }

  const dom = read(() => state()['features/base/participants'].dominantSpeaker, null);
  if (dom && dom !== st.dominant) {
    st.dominant = dom;
    st.events.push({ type: 'speaker', id: dom, name: nameOf(dom), t: at() });
  }
  return st.events.splice(0);
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const log = (msg) => process.stderr.write(`${new Date().toISOString()} ${msg}\n`);

/**
 * Error messages from puppeteer quote the URL they failed on ("net::ERR_… at
 * https://…#…"), which may carry a JWT or a room password. Never log one raw.
 */
const scrub = (msg) => String(msg).replace(/https?:\/\/\S+/g, '<url>');

/** Milliseconds are enough for merge-by-offset alignment downstream. */
const round3 = (n) => Math.round(n * 1000) / 1000;

/**
 * Per-track file. The participant id comes from Jitsi, so it never becomes more
 * than one path segment here.
 * ponytail: two ids that sanitize alike would share a file — Jitsi ids are hex,
 * so they do not. Prefix with a counter if that ever stops being true.
 */
const trackFile = (dir, id) =>
  path.join(dir, `${String(id).replace(/[^A-Za-z0-9_-]/g, '_')}.webm`);

/**
 * Fold page events into `tracks` (id -> manifest record, first-seen order) and
 * return the dominant-speaker lines to append. Pure: no fs, no browser.
 */
function applyTrackEvents(tracks, events, dir) {
  const speakers = [];
  for (const e of events) {
    if (e.type === 'speaker') {
      speakers.push({ t_s: round3(e.t), id: e.id, name: e.name || '' });
      continue;
    }
    const rec = tracks.get(e.id);
    if (e.type === 'start') {
      // A rejoin gets a fresh participant id, so an id we already know keeps its
      // original offset and its file.
      if (!rec) {
        tracks.set(e.id, {
          id: e.id,
          name: e.name || '',
          path: path.resolve(trackFile(dir, e.id)),
          offset_s: round3(e.t),
          ended_s: round3(e.t),
        });
      }
    } else if (!rec) {
      continue;
    } else if (e.type === 'name') {
      rec.name = e.name || rec.name;
    } else if (e.type === 'end') {
      rec.ended_s = round3(e.t);
    }
  }
  return speakers;
}

/** JSONL body for a list of objects; '' for an empty list, never a bare "\n". */
const toJsonl = (rows) => rows.map((r) => `${JSON.stringify(r)}\n`).join('');

/** tracks.jsonl carries the manifest without the path — the caller has the dir. */
const manifestRow = (t) => ({ id: t.id, name: t.name, offset_s: t.offset_s, ended_s: t.ended_s });

/**
 * The single stdout line. `tracks` is omitted entirely when the feature is off,
 * which keeps the line byte-identical to the mixed-audio-only contract.
 */
function resultLine({ out, durationS, reason, participants, tracks }) {
  const res = {
    out,
    duration_s: Math.round(durationS * 10) / 10,
    reason,
    participants,
  };
  if (tracks) res.tracks = tracks;
  return `${JSON.stringify(res)}\n`;
}

/**
 * Wires per-participant capture onto an already-recording page, or returns null
 * when --tracks-dir was not given. Failures here are logged and downgrade to
 * null: losing per-speaker tracks must never cost us the mixed recording.
 */
async function setupTracks(page, tracksDir, startedAt) {
  if (!tracksDir) return null;
  const dir = path.resolve(tracksDir);
  const tracks = new Map();
  try {
    fs.mkdirSync(dir, { recursive: true });
    await page.exposeFunction('__trackChunk', (id, b64) => {
      try {
        // A chunked MediaRecorder WebM stays playable appended chunk by chunk —
        // the first one carries the header. Do not re-mux.
        fs.appendFileSync(trackFile(dir, id), Buffer.from(b64, 'base64'));
      } catch (e) {
        log(`track write failed: ${scrub(e.message)}`);
      }
    });
  } catch (e) {
    log(`per-participant capture disabled: ${scrub(e.message)}`);
    return null;
  }
  const speakersPath = path.join(dir, 'speakers.jsonl');
  const pump = async (stop) => {
    try {
      const events = await page.evaluate(pollTracks, startedAt, !!stop);
      const lines = toJsonl(applyTrackEvents(tracks, events, dir));
      if (lines) fs.appendFileSync(speakersPath, lines);
    } catch (e) {
      log(`track poll failed: ${scrub(e.message)}`);
    }
  };
  await pump(false); // attach to whoever is already in the room
  return {
    pump,
    async finish() {
      await pump(true);
      await sleep(TRACK_FLUSH_MS);
      const list = [...tracks.values()];
      try {
        fs.writeFileSync(path.join(dir, 'tracks.jsonl'), toJsonl(list.map(manifestRow)));
      } catch (e) {
        log(`track manifest failed: ${scrub(e.message)}`);
      }
      log(`per-participant tracks: ${list.length} in ${dir}`);
      return list;
    },
  };
}

async function main(argv) {
  let opts;
  try {
    opts = parseArgs(argv);
  } catch (e) {
    process.stderr.write(`error: ${e.message}\n\n${USAGE}`);
    return 2;
  }

  fs.mkdirSync(path.dirname(path.resolve(opts.out)), { recursive: true });

  // Stop reason is also the signal latch: a second signal exits immediately.
  let reason = null;
  const onSignal = (sig) => {
    if (reason) {
      log(`${sig} again — exiting immediately`);
      process.exit(0);
    }
    reason = 'signal';
    log(`${sig} received — stopping`);
  };
  process.on('SIGTERM', () => onSignal('SIGTERM'));
  process.on('SIGINT', () => onSignal('SIGINT'));

  const { launch, getStream } = require('puppeteer-stream');
  let browser;
  let page;
  try {
    // puppeteer-stream only honours headless when the value is literally 'new'
    // (it needs the capture extension, which legacy headless cannot load).
    // Passing the puppeteer module lets it find the bundled browser when
    // PUPPETEER_EXECUTABLE_PATH is unset.
    browser = await launch(require('puppeteer'), {
      headless: 'new',
      executablePath: process.env.PUPPETEER_EXECUTABLE_PATH || undefined,
      args: ['--no-sandbox', '--autoplay-policy=no-user-gesture-required'],
      // Puppeteer's own handlers would kill Chromium (and exit 130 on SIGINT)
      // before we finalize the file. Shutdown is onSignal's job.
      handleSIGINT: false,
      handleSIGTERM: false,
      handleSIGHUP: false,
    });
    page = await browser.newPage();
    log(`joining room ${roomName(opts.url)} as ${opts.displayName}`);
    await page.goto(buildUrl(opts.url, opts.displayName), {
      waitUntil: 'domcontentloaded',
      timeout: 60000,
    });
  } catch (e) {
    log(`browser launch/page failure: ${scrub(e.message)}`);
    if (browser) await browser.close().catch(() => {});
    return 4;
  }

  try {
    // --- join phase -------------------------------------------------------
    // Anything that is not "joined" counts as waiting, including a moderator
    // wall we cannot tell apart from a lobby; --join-timeout is the backstop.
    // ponytail: no explicit rejected/kicked detection — Jitsi signals it only
    // through a transient notification. Add one if exit 3 proves too slow.
    const joinDeadline = Date.now() + opts.joinTimeout * 1000;
    let joined = false;
    let lastPhase = '';
    while (!reason && Date.now() < joinDeadline) {
      let state;
      try {
        state = await page.evaluate(readJitsiState);
      } catch (e) {
        log(`state probe failed: ${scrub(e.message)}`);
        state = { joined: false, knocking: false, membersCount: 0 };
      }
      const phase = state.joined ? 'joined' : state.knocking ? 'waiting_in_lobby' : 'waiting';
      if (phase !== lastPhase) {
        log(`state: ${phase}`);
        lastPhase = phase;
      }
      if (state.joined) {
        joined = true;
        break;
      }
      await sleep(POLL_MS);
    }
    if (!joined) {
      log(reason === 'signal' ? 'stopped before joining' : 'join timeout');
      return 3;
    }

    // --- record phase -----------------------------------------------------
    const stream = await getStream(page, { audio: true, video: false });
    const file = fs.createWriteStream(opts.out);
    // An unhandled 'error' here (cannot open, disk full) would crash the process
    // with exit 1, skipping cleanup and the contract's exit codes.
    let fileError = null;
    file.on('error', (e) => {
      fileError = e;
    });
    // The capture stream only ends on its own if the extension's MediaRecorder
    // died — we end it deliberately after the loop, so an end during the loop
    // means the rest of the call was never recorded.
    let captureDied = false;
    const onCaptureEnd = () => {
      captureDied = true;
    };
    stream.once('end', onCaptureEnd);
    stream.once('close', onCaptureEnd);
    stream.pipe(file);
    const startedAt = Date.now();
    log(`recording -> ${opts.out}`);
    const trackCap = await setupTracks(page, opts.tracksDir, startedAt);

    let aloneSince = null;
    const participants = new Set(); // insertion order == first-seen order
    while (!reason && !fileError && !captureDied) {
      await sleep(POLL_MS);
      let membersCount = 0; // page gone == nobody left to record
      try {
        const state = await page.evaluate(readJitsiState);
        membersCount = state.membersCount;
        for (const name of state.participants) participants.add(name);
      } catch (e) {
        log(`state probe failed: ${scrub(e.message)}`);
      }
      if (trackCap) await trackCap.pump(false);
      const next = shouldStop({
        membersCount,
        aloneSince,
        now: Date.now(),
        emptyGrace: opts.emptyGrace,
        startedAt,
        maxDuration: opts.maxDuration,
      });
      aloneSince = next.aloneSince;
      if (next.reason) reason = next.reason;
    }

    const durationS = (Date.now() - startedAt) / 1000;
    const failure = fileError
      ? `output write failed: ${scrub(fileError.message)}`
      : captureDied
        ? 'audio capture ended before the call did — the recording is truncated'
        : null;
    log(`stopping: ${failure ? 'failed' : reason}`);
    const trackList = trackCap ? await trackCap.finish() : null;
    await stream.stop().catch(() => {});
    const flushed = () => Promise.race([once(file, 'finish').catch(() => {}), sleep(FLUSH_MS)]);
    if (!fileError) {
      await flushed();
      if (!file.writableFinished) {
        // The extension's websocket never closed; end the file ourselves.
        stream.unpipe(file);
        file.end();
        await flushed();
      }
    }
    if (failure) {
      log(failure);
      return 5;
    }

    const size = fs.statSync(opts.out, { throwIfNoEntry: false })?.size ?? 0;
    if (size === 0) {
      log(`output missing or empty: ${opts.out}`);
      return 5;
    }
    log(`wrote ${size} bytes in ${durationS.toFixed(1)}s, ${participants.size} participant(s)`);
    process.stdout.write(
      resultLine({
        out: opts.out,
        durationS,
        reason,
        participants: [...participants],
        tracks: trackList,
      })
    );
    return 0;
  } finally {
    await browser.close().catch(() => {});
  }
}

if (require.main === module) {
  main(process.argv.slice(2)).then(
    (code) => process.exit(code),
    (e) => {
      log(`fatal: ${e && e.stack ? e.stack : e}`);
      process.exit(4);
    }
  );
}

module.exports = {
  USAGE,
  parseArgs,
  buildUrl,
  roomName,
  shouldStop,
  // pollTracks and setupTracks are exported so the per-participant capture can
  // be driven against a stubbed window.APP in a browser, which is how the audio
  // path is verified without a live conference.
  pollTracks,
  setupTracks,
  trackFile,
  applyTrackEvents,
  toJsonl,
  manifestRow,
  resultLine,
};
