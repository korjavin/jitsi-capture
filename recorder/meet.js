#!/usr/bin/env node
'use strict';

// Headless Google Meet audio recorder with record.js's CLI contract (flags,
// exit codes, the stdout JSON line, the stderr milestones), so the Go runner
// drives either script the same way. Joins as a guest (no Google account),
// knocks, waits in the lobby, and records the call audio as a 16 kHz mono WAV.
// See recorder/README.md, "Google Meet (meet.js)".
//
// How the audio is captured (proven live in the Meet spike, runs 4-5): Meet
// only plays call audio to a participant that has media devices, so Chromium
// gets a fake silent mic and a fake black camera, both switched off before
// joining; it plays the call into a private PulseAudio null sink, and parec
// records that sink's monitor. Page-side capture (WebAudio/MediaRecorder on the
// RTP tracks, tab capture) does NOT work on Meet: it decodes in its own graph.
//
// Hard rule: the bot never sends audio or video into the call. The fake devices
// are digital silence and a black frame, Meet's mic/camera toggles are turned
// off before joining and re-checked in the call, and lockMedia() disables every
// captured track so Meet cannot re-enable it.

const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawn } = require('node:child_process');
const { parseArgs: parseRecordArgs, shouldStop, resultLine } = require('./record.js');

const USAGE = `usage: meet.js --url <https://meet.google.com/xxx-xxxx-xxx> --out <audio.wav>
               [--join-timeout <sec, default 1200>]  (lobby wait incl. re-knocks)
               [--max-duration <sec, default 14400>]
               [--empty-grace <sec, default 60>]
               [--display-name <str, default NoteTaker>]
               [--tracks-dir <dir>]  (accepted and ignored: Meet has no per-participant audio)
`;

const POLL_MS = 2000;
// A guest knocking on a meeting nobody has opened yet gets "No one responded to
// your request" after a while; it knocks again this often until --join-timeout.
const REKNOCK_MS = 60000;
// Prejoin "Ask to join" clicked but the page still reads prejoin: click again.
const RECLICK_MS = 10000;
const RATE = 16000; // parec output: 16 kHz mono s16le, ~1.9 MB/min

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const log = (msg) => process.stderr.write(`${new Date().toISOString()} ${msg}\n`);
const scrub = (msg) => String(msg).replace(/https?:\/\/\S+/g, '<url>');

// Lobby wait including re-knocks when --join-timeout is not given: owner
// decision, the Go side passes its MEET_JOIN_TIMEOUT_S (same default).
const JOIN_TIMEOUT_S = 1200;

/** record.js's flags and checks, plus: the URL must be a Meet meeting, and the
 * --join-timeout default is JOIN_TIMEOUT_S. */
function parseArgs(argv) {
  const opts = parseRecordArgs(argv);
  // record.js parses strict flag/value pairs, so flags sit at even indices.
  if (!argv.some((a, i) => i % 2 === 0 && a === '--join-timeout')) opts.joinTimeout = JOIN_TIMEOUT_S;
  if (!/^https:\/\/meet\.google\.com\/./.test(opts.url)) throw new Error('--url must be a https://meet.google.com/... URL');
  return opts;
}

/** The meeting code only — never log the URL (it may carry an authuser or a pwd). */
function meetingCode(url) {
  const m = String(url).match(/meet\.google\.com\/([a-z]{3}-[a-z]{4}-[a-z]{3})\b/);
  return m ? m[1] : '(meeting)';
}

/**
 * The stop decision on Meet's visible roster. `others` = names other than the
 * bot, or null when the roster could not be read at all (no tile on screen —
 * a DOM change, not an empty room): unknown never counts as alone.
 * Delegates to record.js's rule, whose membersCount includes the bot.
 */
function meetShouldStop({ others, aloneSince, now, emptyGrace, startedAt, maxDuration }) {
  const membersCount = others === null ? 2 : others.length + 1;
  return shouldStop({ membersCount, aloneSince, now, emptyGrace, startedAt, maxDuration });
}

// --- page side ----------------------------------------------------------------
// Every Meet DOM read lives in the functions below (no closures: puppeteer
// serializes them). Meet's markup is obfuscated and changes without notice, so
// a Meet UI change stays a fix in this block. English UI is forced at launch;
// every phrase is English.
// Last checked against live Meet on 2026-10-02 (spike jitsi2outline-8kp, runs
// 4-5, Workspace-hosted and externally hosted meetings).

/** Injected before any Meet script: every captured mic/camera track starts
 * disabled and stays so — Meet setting track.enabled = true hits a no-op.
 * ponytail: a track Meet clone()s loses the lock; it is still disabled and
 * still the silent/black fake file. */
function lockMedia() {
  if (!window.MediaDevices || !MediaDevices.prototype.getUserMedia) return;
  const gum = MediaDevices.prototype.getUserMedia;
  MediaDevices.prototype.getUserMedia = function (...a) {
    return gum.apply(this, a).then((stream) => {
      for (const t of stream.getTracks()) {
        t.enabled = false;
        Object.defineProperty(t, 'enabled', { get: () => false, set: () => {} });
      }
      return stream;
    });
  };
}

/** Classify the page: {state, why}. Order matters: in the call, terminal
 * phrases are ignored; without tiles they win over a lingering Leave button. */
function readState() {
  // Captions are participant speech: "the call has ended" said aloud must not end the run.
  const cap = document.querySelector('[role="region"][aria-label*="aption" i]');
  let text = (document.body && document.body.innerText) || '';
  if (cap && cap.innerText) text = text.replace(cap.innerText, '');
  const flat = text.replace(/\s+/g, ' ');
  const has = (re) => {
    const m = flat.match(re);
    return m ? m[0] : null;
  };
  const leave = !!document.querySelector('[aria-label*="Leave call" i]');
  const tiles = !!document.querySelector('[data-participant-id]');
  const rules = [
    ['signin', () => (/accounts\.google\.com/.test(location.host) ? 'sign-in page' : null)],
    // The lobby shows a Leave call button too: its own phrases win over it.
    ['lobby', () => has(/please wait until a meeting host brings you into the call|asking to be let in|you'll join the call when someone lets you in/i)],
    // In the call (Leave button + video tiles), page text is chat and names —
    // participant-controlled, so "the call has ended" typed in chat must not end the run.
    ['admitted', () => (leave && tiles ? 'Leave call button + tiles' : null)],
    // Nobody answered the knock (nobody in the meeting yet): re-knock.
    ['unanswered', () => has(/no one responded to your request/i)],
    ['denied', () => has(/denied your request|someone in the call denied|you can't join this call/i)],
    ['blocked', () => has(/you can't join this video call|not allowed to join|this meeting has been locked/i)],
    ['invalid', () => has(/check your meeting code|invalid video call name/i)],
    ['removed', () => has(/you've been removed from the meeting|removed you from the meeting/i)],
    ['ended', () => has(/you left the meeting|the call has ended|call ended|meeting has ended|return to home screen/i)],
    ['admitted', () => (leave ? 'Leave call button' : null)],
    // Weaker phrases ("X is asking to join" is also an in-call notice) only count without the button.
    ['lobby', () => has(/please wait until a meeting host|someone will let you in|waiting for the host|asking to join/i)],
    // Meet's pre-check page: "Getting ready... System info will be sent to confirm you're not a bot."
    ['loading', () => has(/getting ready\.\.\./i)],
    ['prejoin', () => has(/ask to join|join now|what's your name|ready to join|other ways to join/i)],
  ];
  for (const [state, test] of rules) {
    const why = test();
    if (why) return { state, why };
  }
  return { state: 'unknown', why: '' };
}

/** Prejoin screen, one pass: dismiss device prompts, switch every "Turn off
 * microphone/camera" toggle off, and report what is left to do. `mic`/`cam`:
 * 'on' | 'off' | '?' (no toggle) as read before this pass's clicks. Same
 * aria-labels on the prejoin screen and the in-call toolbar. */
function prejoin(name) {
  const btns = [...document.querySelectorAll('button, [role="button"]')];
  const label = (b) => b.getAttribute('aria-label') || '';
  const text = (b) => (b.innerText || label(b)).trim();
  for (const re of [/continue without microphone and camera/i, /^got it$/i, /^dismiss$/i]) {
    const b = btns.find((x) => re.test(text(x)));
    if (b) b.click();
  }
  const dev = (d) =>
    btns.some((b) => new RegExp('turn off ' + d, 'i').test(label(b))) ? 'on' : btns.some((b) => new RegExp('turn on ' + d, 'i').test(label(b))) ? 'off' : '?';
  const r = { mic: dev('microphone'), cam: dev('camera') };
  for (const b of btns) if (/turn off (microphone|camera)/i.test(label(b))) b.click();
  const input = document.querySelector('input[aria-label="Your name" i], input[placeholder="Your name" i], input[type="text"][autocomplete="name"]');
  r.needsName = !!(input && !input.value && name);
  return r;
}

/** Click "Ask to join" (or its variants) if it is enabled; returns its label. */
function clickJoin() {
  const b = [...document.querySelectorAll('button, [role="button"]')].find(
    (x) =>
      /^(ask to join( anyway)?|join( the call)? now|join anyway|join here too)$/i.test((x.innerText || '').trim()) &&
      !x.disabled &&
      x.getAttribute('aria-disabled') !== 'true', // a disabled click is a no-op: retry next poll
  );
  if (b) b.click();
  return b ? b.innerText.trim() : null;
}

/** Visible participant names: the video tiles, the people panel when open, and
 * the bot's own name as Meet marks it. `tiles` = number of tiles on screen
 * (self included) — 0 means the roster is unreadable, not empty. */
function readNames() {
  const uniq = (a) => [...new Set(a.map((s) => (s || '').trim()).filter(Boolean))];
  const firstLine = (el) => ((el.innerText || '').split('\n')[0] || '').trim();
  const tileEls = [...document.querySelectorAll('[data-participant-id]')];
  const panel = [...document.querySelectorAll('[role="list"][aria-label*="articipant" i] [role="listitem"]')];
  return {
    tiles: tileEls.length,
    names: uniq([...tileEls.map(firstLine), ...panel.map((e) => e.getAttribute('aria-label') || firstLine(e))]),
    self: uniq([...document.querySelectorAll('[data-self-name]')].map((e) => e.getAttribute('data-self-name'))),
  };
}

// --- node side ----------------------------------------------------------------

/** Names other than the bot's, or null when the roster is unreadable — no
 * tile on screen, or tiles without any name text (the bot's own included). */
function otherNames({ tiles, names, self }, displayName) {
  if (!tiles || !names.length) return null;
  const me = new Set([displayName, ...self].map((s) => s.toLowerCase()));
  return names.filter((n) => !me.has(n.toLowerCase()) && !/\(you\)$/i.test(n) && !/^you$/i.test(n));
}

/** `seconds` of digital silence as a 48 kHz mono s16 WAV: the fake mic. */
function silentWav(seconds) {
  const rate = 48000;
  const n = rate * seconds * 2;
  const h = Buffer.alloc(44);
  h.write('RIFF', 0, 'latin1');
  h.writeUInt32LE(36 + n, 4);
  h.write('WAVEfmt ', 8, 'latin1');
  h.writeUInt32LE(16, 16);
  h.writeUInt16LE(1, 20); // PCM
  h.writeUInt16LE(1, 22); // mono
  h.writeUInt32LE(rate, 24);
  h.writeUInt32LE(rate * 2, 28);
  h.writeUInt16LE(2, 32);
  h.writeUInt16LE(16, 34);
  h.write('data', 36, 'latin1');
  h.writeUInt32LE(n, 40);
  return Buffer.concat([h, Buffer.alloc(n)]);
}

/** One black 320x240 frame as Y4M (Chromium loops it): the fake camera. */
function blackY4m() {
  return Buffer.concat([
    Buffer.from('YUV4MPEG2 W320 H240 F15:1 Ip A1:1 C420jpeg\nFRAME\n', 'latin1'),
    Buffer.alloc(320 * 240, 16),
    Buffer.alloc(2 * 160 * 120, 128),
  ]);
}

/** Puppeteer launch options: fake silent mic + black camera from files in
 * `dir`, prompts auto-accepted, audio NOT muted (the null sink must hear it),
 * Chromium on the private PulseAudio `server`. */
function launchOpts(dir, server) {
  const audio = path.join(dir, 'fake-mic-silence.wav');
  const video = path.join(dir, 'fake-cam-black.y4m');
  fs.writeFileSync(audio, silentWav(2));
  fs.writeFileSync(video, blackY4m());
  return {
    headless: true, // new headless in puppeteer >= 22
    executablePath: process.env.PUPPETEER_EXECUTABLE_PATH || undefined,
    defaultViewport: null,
    args: [
      '--no-sandbox',
      '--autoplay-policy=no-user-gesture-required',
      '--window-size=1280,800',
      '--lang=en-US', // every phrase readState matches is English
      '--no-first-run',
      '--no-default-browser-check',
      '--use-fake-ui-for-media-stream',
      '--use-fake-device-for-media-stream',
      `--use-file-for-fake-audio-capture=${audio}`,
      `--use-file-for-fake-video-capture=${video}`,
      // The usual evasions open-source Meet bots ship with; the spike passed
      // Meet's bot check with these and no stealth plugin.
      '--disable-blink-features=AutomationControlled',
    ],
    // --mute-audio: puppeteer mutes headless audio by default.
    ignoreDefaultArgs: ['--mute-audio', '--enable-automation'],
    env: { ...process.env, PULSE_SERVER: server },
    // Shutdown is onSignal's job: the WAV must be finalized first.
    handleSIGINT: false,
    handleSIGTERM: false,
    handleSIGHUP: false,
  };
}

/** Spawn a child whose stderr tail is kept on `p.err`. */
function run(cmd, args, env) {
  const p = spawn(cmd, args, { env, stdio: ['ignore', 'ignore', 'pipe'] });
  p.err = '';
  p.stderr.on('data', (d) => (p.err = (p.err + d).slice(-500)));
  p.on('error', (e) => (p.err = e.message));
  // A forced exit (second signal) skips the stop path: parec would keep growing
  // the WAV and pulseaudio would outlive us.
  process.on('exit', () => alive(p) && p.kill('SIGKILL'));
  return p;
}

const alive = (p) => p.exitCode === null && !p.signalCode;

/** SIGINT (parec finalizes the WAV header on it), then SIGKILL after 3 s. */
async function stopProc(p) {
  if (!p || !alive(p)) return;
  p.kill('SIGINT');
  await new Promise((r) => {
    p.once('exit', r);
    setTimeout(r, 3000);
  });
  if (alive(p)) p.kill('SIGKILL');
}

/**
 * One private PulseAudio per run in `dir` (a fresh temp dir, so concurrent jobs
 * never share a server): its only — so default — output is a null sink named
 * `meet`, so Chromium has a real output device that pulls WebRTC playout.
 * Returns { server, proc } or throws.
 */
async function startPulse(dir) {
  const sock = path.join(dir, 'native');
  const env = { ...process.env, HOME: dir, XDG_RUNTIME_DIR: dir, PULSE_RUNTIME_PATH: dir };
  // -n: no default.pa, so nothing but these two modules (no hardware probing).
  const proc = run(
    'pulseaudio',
    ['-n', '--daemonize=no', '--exit-idle-time=-1', '--use-pid-file=no', '--log-target=stderr',
      '-L', `module-native-protocol-unix socket=${sock} auth-anonymous=1`,
      '-L', 'module-null-sink sink_name=meet rate=48000'],
    env,
  );
  for (let i = 0; i < 50 && !fs.existsSync(sock) && alive(proc); i++) await sleep(100);
  if (!fs.existsSync(sock)) {
    await stopProc(proc);
    throw new Error(`pulseaudio did not start: ${proc.err.trim().split('\n').pop() || 'no socket'}`);
  }
  return { server: `unix:${sock}`, proc };
}

/** parec on the null sink's monitor -> `out` as 16 kHz mono s16 WAV. */
function startParec(server, out) {
  return run('parec', ['-s', server, '-d', 'meet.monitor', `--rate=${RATE}`, '--channels=1', '--format=s16le', '--file-format=wav', out], process.env);
}

/**
 * Make the WAV header match the file whatever happened to parec (a SIGKILL
 * leaves placeholder sizes) and return the PCM byte count — 0 for a missing,
 * header-only or unparseable file.
 */
function finalizeWav(file) {
  let fd;
  try {
    fd = fs.openSync(file, 'r+');
  } catch {
    return 0;
  }
  try {
    const size = fs.fstatSync(fd).size;
    const head = Buffer.alloc(Math.min(size, 4096));
    fs.readSync(fd, head, 0, head.length, 0);
    const at = head.indexOf('data', 12, 'latin1');
    if (head.toString('latin1', 0, 4) !== 'RIFF' || at < 0) return 0;
    const pcm = size - (at + 8);
    if (pcm <= 0) return 0;
    const u32 = (n, pos) => {
      const b = Buffer.alloc(4);
      b.writeUInt32LE(n);
      fs.writeSync(fd, b, 0, 4, pos);
    };
    u32(size - 8, 4);
    u32(pcm, at + 4);
    return pcm;
  } finally {
    fs.closeSync(fd);
  }
}

async function main(argv) {
  let opts;
  try {
    opts = parseArgs(argv);
  } catch (e) {
    process.stderr.write(`error: ${e.message}\n\n${USAGE}`);
    return 2;
  }
  opts.out = path.resolve(opts.out);
  fs.mkdirSync(path.dirname(opts.out), { recursive: true });
  fs.rmSync(opts.out, { force: true }); // truncate semantics, like record.js

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

  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'meet-'));
  let pulse = null;
  let parec = null;
  let browser = null;
  try {
    let page;
    try {
      pulse = await startPulse(dir);
      browser = await require('puppeteer').launch(launchOpts(dir, pulse.server));
      page = (await browser.pages())[0] || (await browser.newPage());
      const ua = await browser.userAgent();
      if (/HeadlessChrome/.test(ua)) await page.setUserAgent(ua.replace('HeadlessChrome', 'Chrome'));
      await page.setExtraHTTPHeaders({ 'Accept-Language': 'en-US,en;q=0.9' });
      await page.evaluateOnNewDocument(lockMedia);
      // Meet may not play call audio to a guest without devices (spike runs 1-3).
      await browser.defaultBrowserContext().overridePermissions('https://meet.google.com', ['camera', 'microphone']);
      log(`joining room ${meetingCode(opts.url)} as ${opts.displayName}`);
      await page.goto(opts.url, { waitUntil: 'domcontentloaded', timeout: 60000 });
    } catch (e) {
      log(`browser launch/page failure: ${scrub(e.message)}`);
      return 4;
    }

    // --- join phase -------------------------------------------------------
    const joinDeadline = Date.now() + opts.joinTimeout * 1000;
    let last = '';
    let nameTyped = false;
    let clickedAt = 0; // last "Ask to join" click
    let knocks = 0;
    let admitted = false;
    while (!reason && Date.now() < joinDeadline) {
      const s = await page.evaluate(readState).catch((e) => ({ state: 'probe-error', why: scrub(e.message) }));
      if (s.state !== last) {
        const shown = s.state === 'lobby' ? 'waiting_in_lobby' : s.state === 'admitted' ? 'joined' : s.state;
        log(`state: ${shown} (${s.why})`);
        last = s.state;
      }
      if (s.state === 'admitted') {
        admitted = true;
        break;
      }
      if (['denied', 'blocked', 'invalid', 'removed', 'ended', 'signin'].includes(s.state)) {
        log(`not admitted: ${s.state}`);
        return 3;
      }
      if (s.state === 'unanswered' && Date.now() - clickedAt >= REKNOCK_MS) {
        knocks++;
        log(`state: waiting_in_lobby (no one responded; re-knock ${knocks})`);
        await page.reload({ waitUntil: 'domcontentloaded', timeout: 60000 }).catch((e) => log(`reload failed: ${scrub(e.message)}`));
        nameTyped = false;
        clickedAt = 0;
        last = '';
      } else if (s.state === 'prejoin' && Date.now() - clickedAt >= RECLICK_MS) {
        const p = await page.evaluate(prejoin, opts.displayName).catch((e) => ({ mic: '?', cam: '?', error: scrub(e.message) }));
        // Never join while the mic or camera reads on: the next pass confirms the clicks.
        if (p.mic !== 'on' && p.cam !== 'on' && !p.error) {
          if (p.needsName && !nameTyped) {
            await page.type('input[aria-label="Your name" i], input[placeholder="Your name" i], input[type="text"][autocomplete="name"]', opts.displayName, { delay: 60 }).catch(() => {});
            nameTyped = true;
            await sleep(500);
          }
          await sleep(1000); // let device/name state settle like a human would
          const clicked = await page.evaluate(clickJoin).catch(() => null);
          if (clicked) {
            clickedAt = Date.now();
            log(`clicked "${clicked}"`);
          }
        }
      }
      await sleep(POLL_MS);
    }
    if (!admitted) {
      log(reason === 'signal' ? 'stopped before joining' : 'join timeout');
      return 3;
    }

    // --- record phase -----------------------------------------------------
    // parec starts at admission: the WAV is the call, not the prejoin screen.
    parec = startParec(pulse.server, opts.out);
    const startedAt = Date.now();
    log(`recording -> ${opts.out}`);
    let aloneSince = null;
    let failure = null;
    const participants = new Set(); // first-seen order
    for (let tick = 0; !reason && !failure; tick++) {
      // Every 10 s (and right away): Meet's mic/camera toggles stay off.
      if (tick % 5 === 0) {
        const p = await page.evaluate(prejoin, '').catch(() => null);
        if (p && (p.mic === 'on' || p.cam === 'on')) log(`in-call mic=${p.mic} cam=${p.cam}: turned off`);
      }
      await sleep(POLL_MS);
      if (!alive(parec)) failure = `audio capture (parec) exited mid-call: ${parec.err.trim().split('\n').pop() || 'no error'}`;
      else if (!alive(pulse.proc)) failure = 'pulseaudio exited mid-call';
      else if (!browser.connected) failure = 'browser died mid-call';
      if (failure) break;
      const s = await page.evaluate(readState).catch(() => ({ state: 'probe-error' }));
      if (s.state === 'ended' || s.state === 'removed') {
        reason = s.state;
        break;
      }
      const names = await page.evaluate(readNames).catch(() => null);
      const others = names ? otherNames(names, opts.displayName) : null;
      for (const n of others || []) participants.add(n);
      const next = meetShouldStop({
        others,
        aloneSince,
        now: Date.now(),
        emptyGrace: opts.emptyGrace,
        startedAt,
        maxDuration: opts.maxDuration,
      });
      aloneSince = next.aloneSince;
      if (next.reason) reason = next.reason;
    }

    log(`stopping: ${failure ? 'failed' : reason}`);
    await stopProc(parec);
    const pcm = finalizeWav(opts.out);
    if (failure) {
      log(`${failure} — the recording is truncated`);
      return 5;
    }
    if (!pcm) {
      log(`output missing or empty: ${opts.out}`);
      return 5;
    }
    const durationS = pcm / (RATE * 2);
    const size = fs.statSync(opts.out).size;
    log(`wrote ${size} bytes in ${durationS.toFixed(1)}s, ${participants.size} participant(s)`);
    process.stdout.write(resultLine({ out: opts.out, durationS, reason, participants: [...participants] }));
    return 0;
  } finally {
    if (browser) await browser.close().catch(() => {});
    await stopProc(parec);
    if (pulse) await stopProc(pulse.proc);
    fs.rmSync(dir, { recursive: true, force: true });
  }
}

if (require.main === module) {
  main(process.argv.slice(2)).then(
    (code) => process.exit(code),
    (e) => {
      log(`fatal: ${scrub(e && e.stack ? e.stack : e)}`);
      process.exit(4);
    },
  );
}

module.exports = {
  USAGE,
  main,
  parseArgs,
  meetingCode,
  meetShouldStop,
  otherNames,
  readState,
  readNames,
  prejoin,
  lockMedia,
  launchOpts,
  startPulse,
  startParec,
  stopProc,
  finalizeWav,
  silentWav,
  RATE,
};
