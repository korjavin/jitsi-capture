#!/usr/bin/env node
'use strict';

// THROWAWAY SPIKE (bead jitsi2outline-8kp) — do not fold into record.js. The Go
// service runs it on a "meet-spike" DM (spike.go).
// Tries to join a Google Meet call with Puppeteer Chromium and logs everything
// the go/no-go report needs: page state transitions with the text that drove
// them, every incoming WebRTC audio track, per-SSRC audio levels, visible
// participant names, optional caption text, and N seconds of the mixed remote
// audio recorded page-side. See spike/README.md for how to run it.
//
// Meet's DOM is obfuscated and changes often: every selector/phrase below is a
// guess from open-source bots, which is exactly what the live run verifies.

const fs = require('node:fs');
const path = require('node:path');

const USAGE = `usage: meet-spike.js <meet-url> [--user-data-dir <dir>] [--headful]
                    [--seconds <record sec, default 120>] [--join-timeout <sec, default 300>]
                    [--name <guest name, default NoteTaker>] [--out-dir <dir, default ./meet-spike-out>]
                    [--captions] [--plain]
       meet-spike.js --login --user-data-dir <dir>     (headful; sign the bot account in, then close the window)

  --plain   disable the anti-automation tweaks (UA fix, AutomationControlled,
            --enable-automation) to see whether Meet blocks a stock Puppeteer.
  --captions  turn Meet captions on after admission and log caption text.`;

function parseArgs(argv) {
  const o = {
    url: '',
    userDataDir: '',
    headful: false,
    login: false,
    plain: false,
    captions: false,
    seconds: 120,
    joinTimeout: 300,
    name: 'NoteTaker',
    outDir: 'meet-spike-out',
  };
  const val = { '--user-data-dir': 'userDataDir', '--name': 'name', '--out-dir': 'outDir' };
  const num = { '--seconds': 'seconds', '--join-timeout': 'joinTimeout' };
  const bool = { '--headful': 'headful', '--login': 'login', '--plain': 'plain', '--captions': 'captions' };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (bool[a]) o[bool[a]] = true;
    else if (val[a] || num[a]) {
      const v = argv[++i];
      if (v === undefined) throw new Error(`${a} needs a value`);
      if (num[a]) {
        o[num[a]] = Number(v);
        if (!(o[num[a]] > 0)) throw new Error(`${a} must be a positive number`);
      } else o[val[a]] = v;
    } else if (!a.startsWith('--') && !o.url) o.url = a;
    else throw new Error(`unknown argument ${a}`);
  }
  if (o.login && !o.userDataDir) throw new Error('--login needs --user-data-dir');
  if (!o.login && !/^https:\/\/meet\.google\.com\//.test(o.url)) throw new Error('need a https://meet.google.com/... URL');
  return o;
}

const t0 = Date.now();
let logFile = null;
function log(msg, data) {
  const line = `[+${((Date.now() - t0) / 1000).toFixed(1)}s] ${msg}${data === undefined ? '' : ' ' + JSON.stringify(data)}`;
  process.stderr.write(line + '\n');
  if (logFile) fs.appendFileSync(logFile, line + '\n');
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// --- page-side code (runs in Meet; no closures — puppeteer serializes these) --

/** Injected before any Meet script: hooks RTCPeerConnection to see every
 * remote audio track and mixes them all into one MediaStreamDestination. */
function installHooks() {
  const S = (window.__spike = { events: [], pcs: [], ctx: null, dest: null, rec: null, pending: new Set() });
  const Orig = window.RTCPeerConnection;
  if (!Orig) return;
  const mix = (track) => {
    try {
      if (!S.ctx) {
        S.ctx = new AudioContext();
        S.dest = S.ctx.createMediaStreamDestination();
      }
      if (S.ctx.state === 'suspended') S.ctx.resume();
      // Never connect to ctx.destination — we only want it in the recording.
      S.ctx.createMediaStreamSource(new MediaStream([track])).connect(S.dest);
    } catch (e) {
      S.events.push({ type: 'mix-error', msg: String(e) });
    }
  };
  function Hooked(...args) {
    const pc = new Orig(...args);
    const pcIndex = S.pcs.push(pc) - 1;
    S.events.push({ type: 'pc-created', pcIndex });
    pc.addEventListener('track', (ev) => {
      const t = ev.track;
      S.events.push({
        type: 'track',
        pcIndex,
        kind: t.kind,
        id: t.id,
        mid: ev.transceiver && ev.transceiver.mid,
        streams: ev.streams.map((s) => s.id),
      });
      t.addEventListener('ended', () => S.events.push({ type: 'track-ended', pcIndex, kind: t.kind, id: t.id }));
      if (t.kind === 'audio') mix(t);
    });
    pc.addEventListener('connectionstatechange', () =>
      S.events.push({ type: 'pc-state', pcIndex, state: pc.connectionState }),
    );
    return pc;
  }
  Hooked.prototype = Orig.prototype;
  Object.setPrototypeOf(Hooked, Orig);
  window.RTCPeerConnection = Hooked;
  window.webkitRTCPeerConnection = Hooked;
}

/** Classify the current page. Order matters: terminal states win over "the
 * Leave button is still in the DOM". */
function readState() {
  // Captions are participant speech: "the call has ended" said aloud must not end the run.
  const cap = document.querySelector('[role="region"][aria-label*="aption" i]');
  let text = (document.body && document.body.innerText) || '';
  if (cap && cap.innerText) text = text.replace(cap.innerText, '');
  const flat = text.replace(/\s+/g, ' ').trim();
  const has = (re) => {
    const m = flat.match(re);
    return m ? m[0] : null;
  };
  const leave = !!document.querySelector('button[aria-label*="Leave call" i], [aria-label*="Leave call" i]');
  const rules = [
    ['signin', () => (/accounts\.google\.com/.test(location.host) ? location.host : null)],
    ['denied', () => has(/denied your request|someone in the call denied|no one responded to your request|you can't join this call/i)],
    ['blocked', () => has(/you can't join this video call|not allowed to join|this meeting has been locked/i)],
    ['invalid', () => has(/check your meeting code|invalid video call name/i)],
    ['removed', () => has(/you've been removed from the meeting|removed you from the meeting/i)],
    ['ended', () => has(/you left the meeting|the call has ended|call ended|meeting has ended|return to home screen/i)],
    ['admitted', () => (leave ? 'Leave call button' : null)],
    ['lobby', () => has(/asking to be let in|please wait until a meeting host|someone will let you in|waiting for the host|asking to join|you'll join the call when someone lets you in/i)],
    // Meet's pre-check page: "Getting ready... System info will be sent to confirm you're not a bot."
    ['loading', () => has(/getting ready\.\.\./i)],
    ['prejoin', () => has(/ask to join|join now|what's your name|ready to join|other ways to join/i)],
  ];
  for (const [state, test] of rules) {
    const why = test();
    if (why) return { state, why, url: location.origin + location.pathname.replace(/[a-z]{3}-[a-z]{4}-[a-z]{3}/, '<code>'), text: flat.slice(0, 400) };
  }
  return { state: 'unknown', why: '', url: location.origin, text: flat.slice(0, 400) };
}

/** Best-effort prejoin actions; returns what it did. */
function prejoinActions(name) {
  const did = [];
  const buttons = [...document.querySelectorAll('button, [role="button"]')];
  const byText = (re) => buttons.find((b) => re.test((b.innerText || b.getAttribute('aria-label') || '').trim()));
  // Device prompts first: we join without mic/camera on purpose.
  for (const re of [/continue without microphone and camera/i, /^got it$/i, /^dismiss$/i]) {
    const b = byText(re);
    if (b) {
      b.click();
      did.push(`clicked "${re.source}"`);
    }
  }
  for (const b of buttons) {
    const l = b.getAttribute('aria-label') || '';
    if (/turn off (microphone|camera)/i.test(l)) {
      b.click();
      did.push(`clicked "${l}"`);
    }
  }
  const input = document.querySelector('input[aria-label="Your name" i], input[placeholder="Your name" i], input[type="text"][autocomplete="name"]');
  const join = byText(/^(ask to join( anyway)?|join( the call)? now|join anyway|join here too)$/i);
  return { did, needsName: !!(input && !input.value && name), joinLabel: join ? join.innerText.trim() : null };
}

function clickJoin() {
  const b = [...document.querySelectorAll('button, [role="button"]')].find((x) =>
    /^(ask to join( anyway)?|join( the call)? now|join anyway|join here too)$/i.test((x.innerText || '').trim()) &&
    !x.disabled && x.getAttribute('aria-disabled') !== 'true', // a disabled click is a no-op: retry next poll
  );
  if (b) b.click();
  return b ? b.innerText.trim() : null;
}

/** Names visible in the call, by several independent guesses. */
function readNames() {
  const uniq = (a) => [...new Set(a.map((s) => s.trim()).filter(Boolean))];
  const firstLine = (el) => ((el.innerText || '').split('\n')[0] || '').trim();
  const tiles = uniq([...document.querySelectorAll('[data-participant-id]')].map(firstLine));
  const selfName = uniq([...document.querySelectorAll('[data-self-name]')].map((e) => e.getAttribute('data-self-name')));
  const panel = uniq(
    [...document.querySelectorAll('[role="list"][aria-label*="articipant" i] [role="listitem"]')].map(
      (e) => e.getAttribute('aria-label') || firstLine(e),
    ),
  );
  const captionsEl = document.querySelector('[role="region"][aria-label*="aption" i]');
  const captions = captionsEl ? captionsEl.innerText.replace(/\s+/g, ' ').slice(-400) : null;
  return { tiles, selfName, panel, captions };
}

/** Start the page-side MediaRecorder on the mixed destination. */
function startRecorder() {
  const S = window.__spike;
  if (!S.dest) {
    S.ctx = new AudioContext();
    S.dest = S.ctx.createMediaStreamDestination();
  }
  if (S.ctx.state === 'suspended') S.ctx.resume();
  S.rec = new MediaRecorder(S.dest.stream, { mimeType: 'audio/webm;codecs=opus' });
  S.rec.ondataavailable = (e) => {
    if (!e.data || !e.data.size) return;
    const p = e.data.arrayBuffer().then((b) => {
      const u8 = new Uint8Array(b);
      let bin = '';
      for (let i = 0; i < u8.length; i++) bin += String.fromCharCode(u8[i]);
      return window.__spikeChunk(btoa(bin)); // exposeFunction only carries strings
    });
    S.pending.add(p);
    p.finally(() => S.pending.delete(p));
  };
  S.rec.start(1000);
  return S.ctx.state;
}

function stopRecorder() {
  const S = window.__spike;
  if (!S.rec || S.rec.state === 'inactive') return Promise.resolve();
  const stopped = new Promise((r) => {
    S.rec.onstop = r;
    setTimeout(r, 4000);
  });
  S.rec.stop();
  return stopped.then(() => Promise.race([Promise.allSettled([...S.pending]), new Promise((r) => setTimeout(r, 4000))]));
}

/** Drain hook events and sample per-SSRC inbound audio (are the ~3 streams
 * really different speakers? watch which ssrc's audioLevel moves). */
async function pollMedia() {
  const S = window.__spike;
  const events = S.events.splice(0);
  const inbound = [];
  for (let i = 0; i < S.pcs.length; i++) {
    try {
      const stats = await S.pcs[i].getStats();
      stats.forEach((r) => {
        if (r.type === 'inbound-rtp' && r.kind === 'audio')
          inbound.push({
            pc: i,
            ssrc: r.ssrc,
            track: r.trackIdentifier,
            level: r.audioLevel === undefined ? null : Number(r.audioLevel.toFixed(3)),
            kB: Math.round((r.bytesReceived || 0) / 1024),
          });
      });
    } catch {}
  }
  const liveAudio = S.pcs.reduce(
    (n, pc) => n + pc.getReceivers().filter((r) => r.track && r.track.kind === 'audio' && r.track.readyState === 'live').length,
    0,
  );
  return { events, inbound, liveAudio, ctx: S.ctx && S.ctx.state };
}

// --- node side ---------------------------------------------------------------

function launchOpts(o) {
  // English UI on purpose: every phrase matched below is English.
  const args = ['--no-sandbox', '--autoplay-policy=no-user-gesture-required', '--window-size=1280,800', '--lang=en-US', '--no-first-run', '--no-default-browser-check'];
  const opts = {
    headless: o.headful || o.login ? false : true, // true == new headless in puppeteer >= 22
    executablePath: process.env.PUPPETEER_EXECUTABLE_PATH || undefined,
    userDataDir: o.userDataDir ? path.resolve(o.userDataDir) : undefined,
    defaultViewport: null,
    args,
    handleSIGINT: false,
  };
  if (!o.plain) {
    // The usual evasions open-source Meet bots ship with (instead of
    // puppeteer-extra-plugin-stealth, which is not a dependency here).
    args.push('--disable-blink-features=AutomationControlled');
    opts.ignoreDefaultArgs = ['--enable-automation'];
  }
  return opts;
}

async function login(o) {
  const puppeteer = require('puppeteer');
  const browser = await puppeteer.launch(launchOpts(o));
  const [page] = await browser.pages();
  await page.goto('https://accounts.google.com/', { waitUntil: 'domcontentloaded' });
  log('sign the bot account in, open https://meet.google.com once to check it, then CLOSE the browser window');
  await new Promise((r) => browser.once('disconnected', r));
  log(`profile saved in ${path.resolve(o.userDataDir)}`);
}

async function main() {
  let o;
  try {
    o = parseArgs(process.argv.slice(2));
  } catch (e) {
    process.stderr.write(`error: ${e.message}\n\n${USAGE}\n`);
    return 2;
  }
  if (o.login) return login(o).then(() => 0);

  fs.mkdirSync(o.outDir, { recursive: true });
  logFile = path.join(o.outDir, 'spike.log');
  fs.writeFileSync(logFile, '');
  const audioPath = path.join(o.outDir, 'mixed.webm');
  fs.writeFileSync(audioPath, '');
  let audioBytes = 0;

  let stop = null;
  process.on('SIGINT', () => {
    if (stop) process.exit(130);
    stop = 'signal';
    log('SIGINT — stopping (again to force)');
    // A wedged await never reaches the cleanup. process.exit still runs
    // Puppeteer's exit hook, which kills Chromium; a SIGKILL from the Go side
    // (its WaitDelay is 20 s) would orphan it.
    setTimeout(() => process.exit(130), 15000).unref();
  });

  const puppeteer = require('puppeteer');
  const browser = await puppeteer.launch(launchOpts(o));
  const summary = { mode: o.userDataDir ? 'signed-in profile' : 'anonymous guest', headless: !o.headful, plain: o.plain };
  try {
    const browserUA = await browser.userAgent();
    const page = (await browser.pages())[0] || (await browser.newPage());
    if (!o.plain && /HeadlessChrome/.test(browserUA)) await page.setUserAgent(browserUA.replace('HeadlessChrome', 'Chrome'));
    summary.userAgent = await page.evaluate(() => navigator.userAgent);
    await page.exposeFunction('__spikeChunk', (b64) => {
      const buf = Buffer.from(b64, 'base64');
      audioBytes += buf.length;
      fs.appendFileSync(audioPath, buf);
    });
    await page.setExtraHTTPHeaders({ 'Accept-Language': 'en-US,en;q=0.9' });
    await page.evaluateOnNewDocument(installHooks);
    // Deny camera/mic outright: the bot must never send audio into the call.
    await browser.defaultBrowserContext().overridePermissions('https://meet.google.com', []);

    log(`launch ${JSON.stringify(summary)}`);
    await page.goto(o.url, { waitUntil: 'domcontentloaded', timeout: 60000 });
    summary.webdriver = await page.evaluate(() => navigator.webdriver);
    log('navigator.webdriver', summary.webdriver);

    const shot = async (label) => {
      const f = path.join(o.outDir, `${String(Math.round((Date.now() - t0) / 1000)).padStart(4, '0')}-${label}.png`);
      await page.screenshot({ path: f }).catch(() => {});
      fs.writeFileSync(f.replace(/\.png$/, '.txt'), await page.evaluate(() => document.body.innerText).catch(() => ''));
    };
    const media = async () => {
      const m = await page.evaluate(pollMedia).catch((e) => ({ error: e.message }));
      for (const ev of m.events || []) log('rtc', ev);
      return m;
    };

    // --- join phase ---
    let last = '';
    let nameTyped = false;
    let joinClicked = false;
    let admittedAt = null;
    const deadline = Date.now() + o.joinTimeout * 1000;
    while (!stop && Date.now() < deadline) {
      const s = await page.evaluate(readState).catch((e) => ({ state: 'probe-error', why: e.message, text: '' }));
      if (s.state !== last) {
        log(`STATE ${last || '-'} -> ${s.state}`, { why: s.why, url: s.url, text: s.text });
        last = s.state;
        await shot(s.state);
      }
      if (['denied', 'blocked', 'invalid', 'removed', 'ended', 'signin'].includes(s.state)) {
        summary.result = s.state;
        break;
      }
      if (s.state === 'admitted') {
        admittedAt = Date.now();
        break;
      }
      if (s.state === 'prejoin' && !joinClicked) {
        const a = await page.evaluate(prejoinActions, o.userDataDir ? '' : o.name);
        if (a.did.length) log('prejoin actions', a.did);
        if (a.needsName && !nameTyped) {
          await page.type('input[aria-label="Your name" i], input[placeholder="Your name" i], input[type="text"][autocomplete="name"]', o.name, { delay: 60 });
          nameTyped = true;
          log(`typed guest name "${o.name}"`);
          await sleep(500);
        }
        if (a.joinLabel) {
          await sleep(1000); // let device/name state settle like a human would
          const clicked = await page.evaluate(clickJoin);
          if (clicked) {
            joinClicked = true;
            log(`clicked "${clicked}"`);
          }
        }
      }
      await media();
      await sleep(1500);
    }
    if (!admittedAt) {
      summary.result = summary.result || (stop ? 'stopped' : 'join-timeout');
      await shot('final');
      return 3;
    }

    // --- record phase ---
    summary.result = 'admitted';
    summary.admittedAfterS = Math.round((admittedAt - t0) / 1000);
    if (o.captions) {
      await page.keyboard.press('c'); // Meet shortcut: toggle captions
      log('pressed "c" for captions');
    }
    log('recorder start, AudioContext', await page.evaluate(startRecorder));
    const recEnd = Date.now() + o.seconds * 1000;
    let tick = 0;
    while (!stop && Date.now() < recEnd) {
      await sleep(5000);
      const s = await page.evaluate(readState).catch(() => ({ state: 'probe-error', why: '', text: '' }));
      if (s.state !== 'admitted') {
        log(`STATE admitted -> ${s.state}`, { why: s.why, text: s.text });
        await shot(s.state);
        if (['denied', 'blocked', 'removed', 'ended'].includes(s.state)) {
          summary.result = s.state + ' (after admission)';
          break;
        }
      }
      const m = await media();
      log('media', { liveAudioTracks: m.liveAudio, ctx: m.ctx, inbound: m.inbound, recordedKB: Math.round(audioBytes / 1024) });
      log('names', await page.evaluate(readNames).catch((e) => e.message));
      if (++tick % 6 === 1) await shot('in-call');
    }
    await page.evaluate(stopRecorder).catch(() => {});
    summary.recordedS = Math.round((Date.now() - admittedAt) / 1000);
    return 0;
  } catch (e) {
    log(`fatal: ${e.message}`);
    summary.result = 'error: ' + e.message;
    return 4;
  } finally {
    summary.audio = { path: audioPath, bytes: audioBytes };
    await browser.close().catch(() => {});
    log('SUMMARY', summary);
  }
}

main().then(
  (code) => process.exit(code),
  (e) => {
    process.stderr.write(`fatal: ${e.stack}\n`);
    process.exit(4);
  },
);
