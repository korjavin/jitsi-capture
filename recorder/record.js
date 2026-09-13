#!/usr/bin/env node
'use strict';

// Headless Jitsi audio recorder: joins muted, waits (in the lobby if a human
// moderator has to admit it), records the tab's incoming audio to WebM/Opus and
// exits with a contract-defined code. See recorder/README.md.
//
// Jitsi's `window.APP` is an internal global, not a public API. Every read of it
// lives in readJitsiState() below so a Jitsi UI change is a one-place fix.
// Last checked against live Jitsi on 2026-09-13: the lobby/moderator-wall state
// on meet.jit.si, the joined/recording path on an open public deployment.

const fs = require('node:fs');
const path = require('node:path');
const { once } = require('node:events');

const USAGE = `usage: record.js --url <jitsi-meeting-url> --out <audio.webm>
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

const FLAGS = {
  '--url': 'url',
  '--out': 'out',
  '--join-timeout': 'joinTimeout',
  '--max-duration': 'maxDuration',
  '--empty-grace': 'emptyGrace',
  '--display-name': 'displayName',
};

const NUMERIC = new Set(['joinTimeout', 'maxDuration', 'emptyGrace']);

const POLL_MS = 2000;
const FLUSH_MS = 5000;

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

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const log = (msg) => process.stderr.write(`${new Date().toISOString()} ${msg}\n`);

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
    });
    page = await browser.newPage();
    log(`joining room ${roomName(opts.url)} as ${opts.displayName}`);
    await page.goto(buildUrl(opts.url, opts.displayName), {
      waitUntil: 'domcontentloaded',
      timeout: 60000,
    });
  } catch (e) {
    log(`browser launch/page failure: ${e.message}`);
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
        log(`state probe failed: ${e.message}`);
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
    stream.pipe(file);
    const startedAt = Date.now();
    log(`recording -> ${opts.out}`);

    let aloneSince = null;
    const participants = new Set(); // insertion order == first-seen order
    while (!reason) {
      await sleep(POLL_MS);
      let membersCount = 0; // page gone == nobody left to record
      try {
        const state = await page.evaluate(readJitsiState);
        membersCount = state.membersCount;
        for (const name of state.participants) participants.add(name);
      } catch (e) {
        log(`state probe failed: ${e.message}`);
      }
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
    log(`stopping: ${reason}`);
    await stream.stop().catch(() => {});
    await Promise.race([once(file, 'finish'), sleep(FLUSH_MS)]);
    if (!file.writableFinished) {
      stream.unpipe(file);
      file.end();
      await Promise.race([once(file, 'finish'), sleep(FLUSH_MS)]);
    }

    const size = fs.statSync(opts.out, { throwIfNoEntry: false })?.size ?? 0;
    if (size === 0) {
      log(`output missing or empty: ${opts.out}`);
      return 5;
    }
    log(`wrote ${size} bytes in ${durationS.toFixed(1)}s, ${participants.size} participant(s)`);
    process.stdout.write(
      `${JSON.stringify({
        out: opts.out,
        duration_s: Math.round(durationS * 10) / 10,
        reason,
        participants: [...participants],
      })}\n`
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

module.exports = { USAGE, parseArgs, buildUrl, roomName, shouldStop };
