#!/usr/bin/env node
'use strict';

// Offline check of meet-spike.js's audio capture paths, no Meet needed: a
// localhost page sends Chromium's fake microphone (a beep every second) over a
// local RTCPeerConnection pair, and the receiving side plays it like Meet
// might (a WebAudio graph to the speakers) while meet-spike.js's own hooks,
// recorder and PulseAudio null sink capture it. Prints the same media and
// AUDIO lines as a Meet run. Run it in the Docker image:
//   docker run --rm --entrypoint node <image> recorder/spike/audio-check.js [--no-pulse] [--seconds N]

const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const os = require('node:os');
const { installHooks, startRecorder, startTabCapture, pollMedia, startPulse, wavLevel, db, launchOpts } = require('./meet-spike.js');

/** Page side: loop the fake mic through two peer connections; play the remote
 * track through a WebAudio graph into the speakers. */
async function loopback() {
  const mic = await navigator.mediaDevices.getUserMedia({ audio: true });
  const a = new RTCPeerConnection();
  const b = new RTCPeerConnection();
  a.onicecandidate = (e) => e.candidate && b.addIceCandidate(e.candidate);
  b.onicecandidate = (e) => e.candidate && a.addIceCandidate(e.candidate);
  b.ontrack = (e) => {
    const ctx = new AudioContext();
    ctx.createMediaStreamSource(new MediaStream([e.track])).connect(ctx.destination);
  };
  a.addTrack(mic.getAudioTracks()[0], mic);
  await a.setLocalDescription();
  await b.setRemoteDescription(a.localDescription);
  await b.setLocalDescription();
  await a.setRemoteDescription(b.localDescription);
}

async function main() {
  const noPulse = process.argv.includes('--no-pulse');
  const i = process.argv.indexOf('--seconds');
  const seconds = i > 0 ? Number(process.argv[i + 1]) : 10;
  const j = process.argv.indexOf('--out-dir');
  const outDir = j > 0 ? process.argv[j + 1] : fs.mkdtempSync(path.join(os.tmpdir(), 'audio-check-'));
  const wav = path.join(outDir, 'monitor.wav');
  const say = (msg, data) => process.stdout.write(`${msg} ${JSON.stringify(data)}\n`);

  // localhost is a secure context: getUserMedia works there.
  const srv = http.createServer((_, res) => res.end('<!doctype html><title>audio-check</title>'));
  await new Promise((r) => srv.listen(0, '127.0.0.1', r));
  const pulse = noPulse ? null : await startPulse(wav);
  const opts = launchOpts({ plain: true });
  opts.args.push('--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream');
  const browser = await require('puppeteer').launch({ ...opts, env: pulse ? pulse.env : process.env });
  let peak = { mix: 0, tab: 0, meetCtx: 0 };
  let tab = '';
  try {
    const page = await browser.newPage();
    await page.evaluateOnNewDocument(installHooks);
    // Recordings go nowhere here: only their levels count.
    await page.exposeFunction('__spikeChunk', () => {});
    await page.exposeFunction('__spikeTabChunk', () => {});
    const origin = `http://127.0.0.1:${srv.address().port}`;
    // Like the Meet run's empty override: nothing but the fake mic is granted.
    await browser.defaultBrowserContext().overridePermissions(origin, ['microphone']);
    await page.goto(`${origin}/`);
    await page.evaluate(loopback);
    await page.evaluate(startRecorder);
    tab = await page.evaluate(startTabCapture);
    for (let s = 0; s < seconds; s += 2) {
      await new Promise((r) => setTimeout(r, 2000));
      const m = await page.evaluate(pollMedia);
      peak = { mix: Math.max(peak.mix, m.recRms), tab: Math.max(peak.tab, m.tabRms), meetCtx: Math.max(peak.meetCtx, m.tapRms) };
      say('media', { outs: m.outs, inbound: m.inbound, pageEls: m.pageEls, ourEls: m.ourEls, recRms: m.recRms, tabRms: m.tabRms, taps: m.taps, tapRms: m.tapRms });
    }
  } finally {
    await browser.close();
    if (pulse) await pulse.stop();
    srv.close();
  }
  say('AUDIO', {
    pulse: pulse ? (fs.existsSync(wav) && wavLevel(fs.readFileSync(wav))) || 'no wav' : 'off',
    mix: { peakDb: db(peak.mix) },
    tab: { peakDb: db(peak.tab), state: tab },
    meetCtx: { peakDb: db(peak.meetCtx) },
    wav: pulse ? wav : null,
  });
}

main().catch((e) => {
  process.stderr.write(`fatal: ${e.stack}\n`);
  process.exit(1);
});
