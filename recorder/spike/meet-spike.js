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
                    [--captions [--lang <code>]] [--plain]
       meet-spike.js --login --user-data-dir <dir>     (headful; sign the bot account in, then close the window)

  --plain   disable the anti-automation tweaks (UA fix, AutomationControlled,
            --enable-automation) to see whether Meet blocks a stock Puppeteer.
  --captions  turn Meet captions on after admission and save caption lines
            (speaker, text, time) to captions.jsonl in the out dir.
  --lang    with --captions: pick this caption (spoken) language in Meet's
            settings, e.g. en-US or de-DE. Best effort; default leaves Meet's.`;

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
    lang: '',
  };
  const val = { '--user-data-dir': 'userDataDir', '--name': 'name', '--out-dir': 'outDir', '--lang': 'lang' };
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
  // It ends up inside a CSS selector: letters, digits and dashes only.
  if (o.lang && !/^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})*$/.test(o.lang)) throw new Error('--lang must be a language code like en-US');
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

const CAPTION_SETTLE_MS = 3000;

/**
 * Fold one captions poll into finished utterances. Meet rewrites the newest
 * block while its speaker talks (and corrects older ones for a moment), so a
 * block is final once it left the DOM, or a newer block exists and it has not
 * changed for CAPTION_SETTLE_MS, or the run ends (`flush`). A block whose text
 * shrinks to under half is Meet restarting a long turn in the same element: the
 * old text is final. Returns [{ts, speaker, text}] to append, each one once.
 * `st` = { open: Map(id -> utterance), done: Set(id) }.
 * ponytail: corrections Meet makes after a block is final are dropped.
 */
function foldCaptions(st, blocks, now, flush = false) {
  const out = [];
  const final = (id) => {
    const u = st.open.get(id);
    st.open.delete(id);
    st.done.add(id);
    out.push({ ts: new Date(u.first).toISOString(), speaker: u.speaker, text: u.text });
  };
  const present = new Set();
  for (const b of blocks) {
    if (!b.text) {
      // Still on screen but cleared: its turn is over and Meet may reuse the
      // element for the next one, so its id may start a new utterance.
      present.add(b.id);
      if (st.open.has(b.id)) final(b.id);
      st.done.delete(b.id);
      continue;
    }
    if (st.done.has(b.id)) continue;
    present.add(b.id);
    const u = st.open.get(b.id);
    if (!u) st.open.set(b.id, { speaker: b.name || '?', text: b.text, first: now, changed: now });
    else if (b.text !== u.text) {
      if (b.text.length < u.text.length / 2) {
        out.push({ ts: new Date(u.first).toISOString(), speaker: u.speaker, text: u.text });
        Object.assign(u, { first: now });
      }
      Object.assign(u, { speaker: b.name || u.speaker, text: b.text, changed: now });
    }
  }
  const withText = blocks.filter((b) => b.text);
  const newest = withText.length ? withText[withText.length - 1].id : null;
  for (const [id, u] of [...st.open]) {
    if (flush || !present.has(id) || (id !== newest && now - u.changed >= CAPTION_SETTLE_MS)) final(id);
  }
  return out;
}

// --- page-side code (runs in Meet; no closures — puppeteer serializes these) --

/** Injected before any Meet script: hooks RTCPeerConnection to see every
 * remote audio track and mixes them all into one MediaStreamDestination. */
function installHooks() {
  const S = (window.__spike = { events: [], pcs: [], els: [], pageEls: [], taps: new Map(), tapPeak: 0, ctx: null, dest: null, recs: [], peaks: { mix: 0, tab: 0 }, pending: new Set() });
  const Orig = window.RTCPeerConnection;
  if (!Orig) return;
  const rms = (an, buf) => {
    an.getFloatTimeDomainData(buf);
    let sum = 0;
    for (let i = 0; i < buf.length; i++) sum += buf[i] * buf[i];
    return Math.sqrt(sum / buf.length);
  };
  // Record `stream` with MediaRecorder, shipping 1 s chunks to the exposed node
  // function `fn`; S.peaks[key] = the loudest 200 ms of exactly what it records
  // since the last poll.
  S.record = (stream, fn, key) => {
    const rec = new MediaRecorder(stream, { mimeType: 'audio/webm;codecs=opus' });
    rec.ondataavailable = (e) => {
      if (!e.data || !e.data.size) return;
      const p = e.data.arrayBuffer().then((b) => {
        const u8 = new Uint8Array(b);
        let bin = '';
        for (let i = 0; i < u8.length; i++) bin += String.fromCharCode(u8[i]);
        return window[fn](btoa(bin)); // exposeFunction only carries strings
      });
      S.pending.add(p);
      p.finally(() => S.pending.delete(p));
    };
    rec.start(1000);
    S.recs.push(rec);
    const an = S.ctx.createAnalyser();
    S.ctx.createMediaStreamSource(stream).connect(an);
    const buf = new Float32Array(an.fftSize);
    setInterval(() => (S.peaks[key] = Math.max(S.peaks[key], rms(an, buf))), 200);
  };
  // Method "meetCtx": run 2 saw no <audio> elements, so Meet may play remote
  // audio through its own WebAudio graph. Tap whatever any AudioContext other
  // than ours sends to its speakers (level only, no file).
  const connect = AudioNode.prototype.connect;
  AudioNode.prototype.connect = function (target, ...rest) {
    const r = connect.call(this, target, ...rest);
    try {
      if (target instanceof AudioDestinationNode && this.context !== S.ctx) {
        let tap = S.taps.get(this.context);
        if (!tap) {
          tap = this.context.createAnalyser();
          tap.buf = new Float32Array(tap.fftSize);
          S.taps.set(this.context, tap);
          S.events.push({ type: 'page-ctx', rate: this.context.sampleRate, state: this.context.state });
        }
        connect.call(this, tap);
      }
    } catch (e) {
      S.events.push({ type: 'tap-error', msg: String(e) });
    }
    return r;
  };
  setInterval(() => {
    for (const an of S.taps.values()) S.tapPeak = Math.max(S.tapPeak, rms(an, an.buf));
  }, 200);
  // Media elements the page plays a MediaStream with, attached to the DOM or not.
  const play = HTMLMediaElement.prototype.play;
  HTMLMediaElement.prototype.play = function () {
    if (this.srcObject && !S.els.includes(this) && !S.pageEls.includes(this)) S.pageEls.push(this);
    return play.apply(this, arguments);
  };
  const mix = (track) => {
    // Chromium only decodes (pulls) a remote WebRTC audio track while a media
    // element plays it; a track fed to WebAudio alone can stay digital silence.
    // Run 1 heard -91 dB with getStats audioLevel=0, so play every track too.
    // No mic is granted, so this playout never reaches the call.
    try {
      const el = new Audio();
      el.autoplay = true;
      el.srcObject = new MediaStream([track]);
      S.els.push(el);
      el.play().then(
        () => S.events.push({ type: 'el-play', id: track.id }),
        (e) => S.events.push({ type: 'el-play-error', id: track.id, msg: String(e) }),
      );
    } catch (e) {
      S.events.push({ type: 'el-error', msg: String(e) });
    }
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
    // encoded: Meet asked for insertable streams (it may transform the audio frames itself).
    S.events.push({ type: 'pc-created', pcIndex, encoded: !!(args[0] && args[0].encodedInsertableStreams) });
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
    // The lobby shows a Leave call button too (run 1): the lobby's own phrases win over it.
    ['lobby', () => has(/please wait until a meeting host brings you into the call|asking to be let in|you'll join the call when someone lets you in/i)],
    ['admitted', () => (leave ? 'Leave call button' : null)],
    // Weaker phrases ("X is asking to join" is also an in-call notice) only count without the button.
    ['lobby', () => has(/please wait until a meeting host|someone will let you in|waiting for the host|asking to join/i)],
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
  return { tiles, selfName, panel };
}

/** Turn Meet captions on by the button; returns what it saw/did. The 'c'
 * shortcut is the node-side fallback. Selectors as used by open-source Meet
 * bots and caption extensions (aria-label, then the Material icon name). */
function captionsOn(mayClick) {
  const on = document.querySelector('button[aria-label*="Turn off captions" i], button[aria-label*="aption" i][aria-pressed="true"]');
  if (on) return { on: true, did: null };
  const b =
    document.querySelector('button[aria-label*="Turn on captions" i]') ||
    [...document.querySelectorAll('button')].find((x) => /closed_caption_off/.test(x.innerText || ''));
  if (!b) return { on: false, did: null };
  if (!mayClick) return { on: false, did: null, seen: (b.getAttribute('aria-label') || '').trim() };
  b.click();
  return { on: false, did: `clicked "${(b.getAttribute('aria-label') || b.innerText || '').trim()}"` };
}

/** Read the caption blocks (one per speaker turn) from the captions region.
 * Meet's classes are obfuscated and rotate, so: known classes first, then a
 * structural guess (an element whose first child is a short one-line name and
 * whose other children hold the text). Every block gets a stable id so node
 * can tell a rewrite of the same block from a new one. */
function readCaptions() {
  const region =
    document.querySelector('[role="region"][aria-label*="aption" i]') ||
    document.querySelector('div[role="region"][tabindex="0"]');
  if (!region) return { region: false, strategy: null, blocks: [] };
  const txt = (el) => ((el && el.innerText) || '').replace(/\s+/g, ' ').trim();
  window.__spikeCapId = window.__spikeCapId || 0;
  const id = (el) => el.dataset.spikeCap || (el.dataset.spikeCap = String(++window.__spikeCapId));
  let strategy = 'classes';
  let blocks = [...region.querySelectorAll('.nMcdL')].map((b) => ({
    id: id(b),
    name: txt(b.querySelector('.NWpY1d, .KcIKyf, .zs7s8d')),
    text: txt(b.querySelector('.ygicle, .bh44bd, .iTTPOb')),
  }));
  // Known blocks with a name count even while cleared (empty text): foldCaptions needs them.
  if (!blocks.some((b) => b.name)) {
    strategy = 'structural';
    const cand = [...region.querySelectorAll('*')].filter((e) => {
      if (e.children.length < 2) return false;
      const name = (e.children[0].innerText || '').trim();
      return name && name.length <= 80 && !name.includes('\n') && txt(e).length > name.length;
    });
    // A wrapper with two or more candidate children is the caption list, not a
    // turn; of the rest the outermost wins (a text div of several spans can
    // pass the test inside its turn).
    const turns = cand.filter((e) => [...e.children].filter((c) => cand.includes(c)).length < 2);
    blocks = turns
      .filter((e) => !turns.some((o) => o !== e && o.contains(e)))
      .map((e) => ({
        id: id(e),
        name: txt(e.children[0]),
        text: [...e.children].slice(1).map(txt).filter(Boolean).join(' '),
      }));
  }
  // Text-less blocks stay: a cleared block on screen differs from one that left (foldCaptions).
  // Region with text but nothing parsed: a markup sample to fix the selectors.
  const sample = strategy === 'structural' && !blocks.some((b) => b.text) && txt(region) ? region.innerHTML.slice(0, 600) : null;
  return { region: true, strategy, blocks, sample };
}

/** Click the first element matching a CSS selector, else the first
 * button/menuitem/tab/option whose text or aria-label matches `re` (a regex
 * source). Returns what it clicked, or null. Used to walk Meet's settings. */
function clickFirst(css, re) {
  let el = css ? document.querySelector(css) : null;
  if (!el && re) {
    const rx = new RegExp(re, 'i');
    el = [...document.querySelectorAll('button, [role="menuitem"], [role="tab"], [role="option"], li')].find((e) =>
      rx.test((e.getAttribute('aria-label') || e.innerText || '').trim()),
    );
  }
  if (!el) return null;
  el.click();
  return (el.getAttribute('aria-label') || el.innerText || el.tagName).trim().slice(0, 60);
}

/** Start the page-side MediaRecorder on the mixed destination. */
function startRecorder() {
  const S = window.__spike;
  if (!S.dest) {
    S.ctx = new AudioContext();
    S.dest = S.ctx.createMediaStreamDestination();
  }
  if (S.ctx.state === 'suspended') S.ctx.resume();
  S.record(S.dest.stream, '__spikeChunk', 'mix');
  return S.ctx.state;
}

/** Method "tab": capture this tab's own audio output with getDisplayMedia
 * (needs --auto-accept-this-tab-capture; Puppeteer's evaluate counts as a user
 * gesture). Call after startRecorder. Returns what happened. */
async function startTabCapture() {
  const S = window.__spike;
  try {
    const s = await navigator.mediaDevices.getDisplayMedia({ video: true, audio: true, preferCurrentTab: true });
    const audio = s.getAudioTracks();
    if (!audio.length) return 'no audio track';
    S.record(new MediaStream(audio), '__spikeTabChunk', 'tab');
    return 'recording';
  } catch (e) {
    return 'error: ' + String(e);
  }
}

function stopRecorder() {
  const S = window.__spike;
  const live = S.recs.filter((r) => r.state !== 'inactive');
  const stopped = live.map(
    (r) =>
      new Promise((done) => {
        r.onstop = done;
        setTimeout(done, 4000);
        r.stop();
      }),
  );
  return Promise.all(stopped).then(() => Promise.race([Promise.allSettled([...S.pending]), new Promise((r) => setTimeout(r, 4000))]));
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
        // Short keys: the media line is cut at 600 chars in the Zulip report.
        // samples (totalSamplesReceived) stuck at 0 = nothing decodes/pulls the
        // track; samples growing with energy 0 = Meet really sends silence.
        if (r.type === 'inbound-rtp' && r.kind === 'audio')
          inbound.push({
            pc: i,
            ssrc: r.ssrc,
            lvl: r.audioLevel === undefined ? null : Number(r.audioLevel.toFixed(3)),
            nrg: r.totalAudioEnergy === undefined ? null : Number(r.totalAudioEnergy.toFixed(4)),
            smp: r.totalSamplesReceived === undefined ? null : r.totalSamplesReceived,
            conc: r.concealedSamples === undefined ? null : r.concealedSamples,
            jbe: r.jitterBufferEmittedCount === undefined ? null : r.jitterBufferEmittedCount,
            kB: Math.round((r.bytesReceived || 0) / 1024),
          });
      });
    } catch {}
  }
  const liveAudio = S.pcs.reduce(
    (n, pc) => n + pc.getReceivers().filter((r) => r.track && r.track.kind === 'audio' && r.track.readyState === 'live').length,
    0,
  );
  // Meet's own <audio> elements vs ours: is anything actually playing?
  const els = (a) => ({ n: a.length, playing: a.filter((e) => !e.paused).length, muted: a.filter((e) => e.muted).length });
  const recRms = Number(S.peaks.mix.toPrecision(3)); // toPrecision: -93 dB must not round to silence
  const tabRms = Number(S.peaks.tab.toPrecision(3));
  const tapRms = Number(S.tapPeak.toPrecision(3));
  // Audio output devices the page sees (0 = Meet's "Speaker not found").
  const outs = await navigator.mediaDevices.enumerateDevices().then((d) => d.filter((x) => x.kind === 'audiooutput').length, () => null);
  S.peaks.mix = S.peaks.tab = S.tapPeak = 0;
  return {
    events,
    inbound,
    liveAudio,
    pcs: S.pcs.length,
    outs,
    ctx: S.ctx && S.ctx.state,
    meetEls: els([...document.querySelectorAll('audio')]),
    pageEls: els(S.pageEls),
    ourEls: els(S.els),
    recRms,
    tabRms,
    taps: S.taps.size,
    tapRms,
  };
}

// --- node side ---------------------------------------------------------------

function launchOpts(o) {
  // English UI on purpose: every phrase matched below is English.
  const args = ['--no-sandbox', '--autoplay-policy=no-user-gesture-required', '--auto-accept-this-tab-capture', '--window-size=1280,800', '--lang=en-US', '--no-first-run', '--no-default-browser-check'];
  const opts = {
    headless: o.headful || o.login ? false : true, // true == new headless in puppeteer >= 22
    executablePath: process.env.PUPPETEER_EXECUTABLE_PATH || undefined,
    userDataDir: o.userDataDir ? path.resolve(o.userDataDir) : undefined,
    defaultViewport: null,
    args,
    handleSIGINT: false,
    // Puppeteer mutes headless audio by default: the null sink would hear nothing.
    ignoreDefaultArgs: ['--mute-audio'],
  };
  if (!o.plain) {
    // The usual evasions open-source Meet bots ship with (instead of
    // puppeteer-extra-plugin-stealth, which is not a dependency here).
    args.push('--disable-blink-features=AutomationControlled');
    opts.ignoreDefaultArgs.push('--enable-automation');
  }
  return opts;
}

/**
 * Method "pulse": start a private PulseAudio whose only (so default) output is
 * a null sink, so Chromium has a real audio device that pulls WebRTC playout
 * (run 2: prejoin said "Speaker not found" and no inbound sample was ever
 * decoded), and record the sink's monitor with parec to `wavPath` (16 kHz mono
 * s16 WAV: ~1.9 MB/min). Returns { env, stop } — `env` for Chromium — or null
 * when pulseaudio is not installed. The Jitsi recorder never uses this.
 */
async function startPulse(wavPath) {
  const { spawn } = require('node:child_process');
  const dir = fs.mkdtempSync(path.join(require('node:os').tmpdir(), 'spike-pulse-'));
  const sock = path.join(dir, 'native');
  const env = { ...process.env, HOME: dir, XDG_RUNTIME_DIR: dir, PULSE_RUNTIME_PATH: dir };
  const procs = [];
  const run = (cmd, args) => {
    const p = spawn(cmd, args, { env, stdio: ['ignore', 'ignore', 'pipe'] });
    p.err = '';
    p.stderr.on('data', (d) => (p.err = (p.err + d).slice(-500)));
    p.on('error', (e) => (p.err = e.message));
    procs.push(p);
    return p;
  };
  // A forced exit (second SIGINT, the 15 s fallback) skips stop(): unlike
  // Chromium these children have no exit hook, and parec would grow the WAV forever.
  process.on('exit', () => procs.forEach((p) => p.exitCode === null && !p.signalCode && p.kill('SIGINT')));
  const stop = async () => {
    for (const p of procs.reverse()) {
      if (p.exitCode !== null || p.signalCode) continue;
      p.kill('SIGINT'); // parec finishes the WAV header on SIGINT
      await new Promise((r) => {
        p.once('exit', r);
        setTimeout(r, 3000);
      });
      p.kill('SIGKILL');
    }
    fs.rmSync(dir, { recursive: true, force: true });
  };
  // -n: no default.pa, so nothing but these two modules (no real hardware probing).
  const pa = run('pulseaudio', ['-n', '--daemonize=no', '--exit-idle-time=-1', '--use-pid-file=no', '--log-target=stderr',
    '-L', `module-native-protocol-unix socket=${sock} auth-anonymous=1`, '-L', 'module-null-sink sink_name=spike rate=48000']);
  for (let i = 0; i < 50 && !fs.existsSync(sock) && pa.exitCode === null; i++) await sleep(100);
  if (!fs.existsSync(sock)) {
    log('pulse: unavailable', { err: pa.err.trim().split('\n').pop() });
    await stop();
    return null;
  }
  const server = `unix:${sock}`;
  run('parec', ['-s', server, '-d', 'spike.monitor', '--rate=16000', '--channels=1', '--format=s16le', '--file-format=wav', wavPath]);
  log('pulse: null sink up, recording its monitor');
  return { env: { ...process.env, PULSE_SERVER: server }, stop };
}

/** Level of a 16-bit PCM WAV: overall RMS and the loudest 200 ms window, in
 * dBFS (null = digital silence), plus its length. */
function wavLevel(buf) {
  const i = buf.indexOf('data');
  if (buf.length < 44 || buf.toString('latin1', 0, 4) !== 'RIFF' || i < 0) return null;
  const rate = buf.readUInt32LE(24);
  const ch = buf.readUInt16LE(22);
  const n = Math.floor((buf.length - i - 8) / 2);
  const win = Math.max(1, Math.round(rate * ch * 0.2));
  let sum = 0;
  let wsum = 0;
  let peak = 0;
  for (let k = 0; k < n; k++) {
    const v = buf.readInt16LE(i + 8 + k * 2) / 32768;
    sum += v * v;
    wsum += v * v;
    if ((k + 1) % win === 0 || k === n - 1) {
      peak = Math.max(peak, Math.sqrt(wsum / ((k % win) + 1)));
      wsum = 0;
    }
  }
  return { s: Math.round(n / ch / rate), rmsDb: db(n ? Math.sqrt(sum / n) : 0), peakDb: db(peak) };
}

const db = (rms) => (rms > 0 ? Number((20 * Math.log10(rms)).toFixed(1)) : null);

async function login(o) {
  const puppeteer = require('puppeteer');
  const browser = await puppeteer.launch(launchOpts(o));
  const [page] = await browser.pages();
  await page.goto('https://accounts.google.com/', { waitUntil: 'domcontentloaded' });
  log('sign the bot account in, open https://meet.google.com once to check it, then CLOSE the browser window');
  await new Promise((r) => browser.once('disconnected', r));
  log(`profile saved in ${path.resolve(o.userDataDir)}`);
}

/** Captions on: the toolbar button, then the 'c' shortcut. Never clicks once
 * captions read as on, so it cannot toggle them back off. */
async function enableCaptions(page) {
  let pressed = false;
  let clicks = 0;
  for (let i = 0; i < 10; i++) {
    // Two clicks at most: a button whose label never flips must not be toggled forever.
    const r = await page.evaluate(captionsOn, clicks < 2).catch((e) => ({ on: false, did: null, error: e.message }));
    if (r.on) {
      log('captions on', { attempt: i, via: pressed ? 'c shortcut' : 'button' });
      return true;
    }
    if (r.did) clicks++;
    if (r.did || r.error) log('captions', r);
    if (!clicks && !pressed && i >= 3) {
      await page.keyboard.press('c'); // Meet shortcut: toggle captions
      pressed = true;
      log('captions: no button found, pressed "c"');
    }
    await sleep(1500);
  }
  log('captions: not confirmed on (no "Turn off captions" button); still reading the captions region');
  return false;
}

/** Best effort: More options -> Settings -> Captions -> the language option
 * (clicked even while its dropdown is closed, as attendee does). */
async function pickCaptionLang(page, lang) {
  const steps = [
    ['more options', 'button[aria-label*="More options" i]', null],
    ['settings', null, '(^|\\s)settings$'],
    ['captions tab', '[role="tab"][aria-label="Captions" i], button[aria-label="Captions" i]', '(^|\\s)captions$'],
    ['language option', `li[data-value="${lang}"], [role="option"][data-value="${lang}"]`, null],
  ];
  let ok = true;
  for (const [what, css, re] of steps) {
    const r = await page.evaluate(clickFirst, css, re).catch((e) => `error: ${e.message}`);
    log(`captions lang: ${what}`, r);
    if (!r || r.startsWith('error')) {
      ok = false;
      break;
    }
    await sleep(1500);
  }
  await page.keyboard.press('Escape'); // close the settings dialog / menu
  return ok;
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
  const tabPath = path.join(o.outDir, 'tab.webm');
  fs.writeFileSync(tabPath, '');
  let audioBytes = 0;
  let tabBytes = 0;

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

  // Every capture method runs at once; the AUDIO line compares their levels.
  const monitorPath = path.join(o.outDir, 'monitor.wav');
  const pulse = await startPulse(monitorPath);
  const peaks = { mix: 0, tab: 0, meetCtx: 0, taps: 0 };
  let tabState = 'not started';
  const puppeteer = require('puppeteer');
  const browser = await puppeteer.launch({ ...launchOpts(o), env: pulse ? pulse.env : process.env }).catch(async (e) => {
    if (pulse) await pulse.stop();
    throw e;
  });
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
    await page.exposeFunction('__spikeTabChunk', (b64) => {
      const buf = Buffer.from(b64, 'base64');
      tabBytes += buf.length;
      fs.appendFileSync(tabPath, buf);
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
    log('recorder start, AudioContext', await page.evaluate(startRecorder));
    tabState = await page.evaluate(startTabCapture).catch((e) => 'error: ' + e.message);
    log('tab capture', tabState);
    const cap = { st: { open: new Map(), done: new Set() }, on: false, region: false, strategy: null, blocks: 0, saved: 0, sampled: false };
    const capPath = path.join(o.outDir, 'captions.jsonl');
    const pollCaptions = async (flush) => {
      const r = await page.evaluate(readCaptions).catch((e) => ({ region: false, strategy: null, blocks: [], error: e.message }));
      // A failed read is not "every block left": keep the open lines for the next poll.
      if (r.error && !flush) return;
      Object.assign(cap, { region: r.region, strategy: r.strategy, blocks: r.blocks.length });
      if (r.sample && !cap.sampled) {
        cap.sampled = true;
        log('captions: region has text but no block parsed; markup sample', r.sample);
      }
      for (const u of foldCaptions(cap.st, r.blocks, Date.now(), flush)) {
        fs.appendFileSync(capPath, JSON.stringify(u) + '\n');
        cap.saved++;
      }
    };
    const capStats = () => ({ on: cap.on, region: cap.region, strategy: cap.strategy, blocks: cap.blocks, saved: cap.saved });
    if (o.captions) {
      fs.writeFileSync(capPath, '');
      cap.on = await enableCaptions(page);
      if (o.lang) summary.captionLang = (await pickCaptionLang(page, o.lang)) ? o.lang : 'not set';
    }
    const track = (m) => {
      peaks.mix = Math.max(peaks.mix, m.recRms || 0);
      peaks.tab = Math.max(peaks.tab, m.tabRms || 0);
      peaks.meetCtx = Math.max(peaks.meetCtx, m.tapRms || 0);
      peaks.taps = Math.max(peaks.taps, m.taps || 0);
      return m;
    };
    const recEnd = Date.now() + o.seconds * 1000;
    let tick = 0;
    while (!stop && Date.now() < recEnd) {
      await sleep(1000); // captions every second: Meet drops old blocks from the DOM
      if (o.captions) await pollCaptions(false);
      if (++tick % 5) continue;
      const s = await page.evaluate(readState).catch(() => ({ state: 'probe-error', why: '', text: '' }));
      if (s.state !== 'admitted') {
        log(`STATE admitted -> ${s.state}`, { why: s.why, text: s.text });
        await shot(s.state);
        if (['denied', 'blocked', 'removed', 'ended'].includes(s.state)) {
          summary.result = s.state + ' (after admission)';
          break;
        }
      }
      const m = track(await media());
      log('media', {
        liveAudioTracks: m.liveAudio,
        pcs: m.pcs,
        outs: m.outs,
        ctx: m.ctx,
        inbound: m.inbound,
        meetEls: m.meetEls,
        pageEls: m.pageEls,
        ourEls: m.ourEls,
        recRms: m.recRms,
        tabRms: m.tabRms,
        taps: m.taps,
        tapRms: m.tapRms,
        recordedKB: Math.round(audioBytes / 1024),
        captions: cap.saved,
      });
      const names = await page.evaluate(readNames).catch((e) => ({ error: e.message }));
      log('names', { ...names, captions: capStats() });
      if (tick % 30 === 5) await shot('in-call');
    }
    if (o.captions) {
      await pollCaptions(true);
      summary.captions = capStats();
    }
    track(await media()); // the levels since the last media line
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
    if (pulse) await pulse.stop();
    // Per-method level in dBFS (null = digital silence): pulse = the null-sink
    // monitor (monitor.wav), mix = the page-side MediaRecorder mix (mixed.webm,
    // loudest 200 ms), tab = getDisplayMedia tab audio (tab.webm), meetCtx = taps on the page's own AudioContexts (no file).
    log('AUDIO', {
      pulse: pulse ? (fs.existsSync(monitorPath) && wavLevel(fs.readFileSync(monitorPath))) || 'no wav' : 'unavailable',
      mix: { peakDb: db(peaks.mix), kB: Math.round(audioBytes / 1024) },
      tab: { peakDb: db(peaks.tab), kB: Math.round(tabBytes / 1024), state: tabState.slice(0, 80) },
      meetCtx: { peakDb: db(peaks.meetCtx), ctxs: peaks.taps },
    });
    log('SUMMARY', summary);
  }
}

if (require.main === module)
  main().then(
    (code) => process.exit(code),
    (e) => {
      process.stderr.write(`fatal: ${e.stack}\n`);
      process.exit(4);
    },
  );

module.exports = { parseArgs, foldCaptions, readState, readCaptions, installHooks, startRecorder, startTabCapture, pollMedia, startPulse, wavLevel, db, launchOpts, CAPTION_SETTLE_MS };
