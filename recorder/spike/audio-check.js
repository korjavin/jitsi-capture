#!/usr/bin/env node
'use strict';

// Offline check of meet-spike.js's media setup and audio capture paths, no Meet
// needed. Launches Chromium exactly as a Meet run does (the silent fake mic and
// black fake camera), then:
//   devices: what enumerateDevices / permissions report (Meet's "Mic not found"
//            / "Speaker not found" came from these), and that the fake mic is
//            digital silence with every captured track disabled and locked —
//            the bot must never send sound or picture into a call;
//   media/AUDIO: a localhost page sends a beeping WebAudio tone over a local
//            RTCPeerConnection pair and plays the remote track through
//            WebAudio, like Meet might, while meet-spike.js's own hooks,
//            recorder and PulseAudio null sink capture it.
// Run it in the Docker image:
//   docker run --rm --entrypoint node <image> recorder/spike/audio-check.js [--no-pulse] [--seconds N]

const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const os = require('node:os');
const { installHooks, startRecorder, startTabCapture, pollMedia, deviceInfo, rtpInfo, startPulse, wavLevel, db, speechFrac, writeFakeMedia, launchOpts } = require('./meet-spike.js');

/** Page side: capture mic + camera, try to re-enable them (what Meet does on
 * "Turn on microphone"), and measure the mic's peak. On a hooked page the
 * tracks must stay disabled; on an unhooked page the peak shows what the fake
 * mic file itself carries (Chromium's default fake mic beeps). */
async function fakeMic() {
  const s = await navigator.mediaDevices.getUserMedia({ audio: true, video: true });
  const [mic] = s.getAudioTracks();
  s.getTracks().forEach((t) => (t.enabled = true));
  const ctx = new AudioContext();
  const an = ctx.createAnalyser();
  ctx.createMediaStreamSource(new MediaStream([mic])).connect(an);
  const buf = new Float32Array(an.fftSize);
  let peak = 0;
  for (let i = 0; i < 20; i++) {
    await new Promise((r) => setTimeout(r, 100));
    an.getFloatTimeDomainData(buf);
    for (const v of buf) peak = Math.max(peak, Math.abs(v));
  }
  const out = { tracks: s.getTracks().map((t) => `${t.kind}:${t.label}:enabled=${t.enabled}`), micPeak: peak };
  s.getTracks().forEach((t) => t.stop());
  ctx.close();
  return out;
}

/** Page side: loop a beeping tone through two peer connections; play the
 * remote track through a WebAudio graph into the speakers. */
async function loopback() {
  const src = new AudioContext();
  const osc = src.createOscillator();
  const gain = src.createGain();
  const dst = src.createMediaStreamDestination();
  osc.connect(gain).connect(dst);
  osc.start();
  setInterval(() => (gain.gain.value = gain.gain.value ? 0 : 0.5), 500); // 0.5 s beep, 0.5 s silence
  const a = new RTCPeerConnection();
  const b = new RTCPeerConnection();
  a.onicecandidate = (e) => e.candidate && b.addIceCandidate(e.candidate);
  b.onicecandidate = (e) => e.candidate && a.addIceCandidate(e.candidate);
  b.ontrack = (e) => {
    const ctx = new AudioContext();
    ctx.createMediaStreamSource(new MediaStream([e.track])).connect(ctx.destination);
  };
  a.addTrack(dst.stream.getAudioTracks()[0], dst.stream);
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
  const opts = launchOpts({ plain: true, fakeMedia: writeFakeMedia(outDir) });
  const browser = await require('puppeteer').launch({ ...opts, env: pulse ? pulse.env : process.env });
  let peak = { mix: 0, tab: 0, meetCtx: 0 };
  let speech = {};
  let tab = '';
  try {
    const page = await browser.newPage();
    await page.evaluateOnNewDocument(installHooks);
    // Recordings go nowhere here: only their levels count.
    await page.exposeFunction('__spikeChunk', () => {});
    await page.exposeFunction('__spikeTabChunk', () => {});
    const origin = `http://127.0.0.1:${srv.address().port}`;
    // The same grant as the Meet run.
    await browser.defaultBrowserContext().overridePermissions(origin, ['camera', 'microphone']);
    // The fake mic file itself, on a page without the hooks: must be digital silence.
    const raw = await browser.newPage();
    await raw.goto(`${origin}/`);
    const file = await raw.evaluate(fakeMic);
    await raw.close();
    say('fake mic file (unhooked page)', { ...file, micPeakDb: db(file.micPeak) });
    await page.goto(`${origin}/`);
    const mic = await page.evaluate(fakeMic);
    say('devices (hooked page)', { ...(await page.evaluate(deviceInfo)), ...mic, micPeakDb: db(mic.micPeak) });
    await page.evaluate(loopback);
    await page.evaluate(startRecorder);
    tab = await page.evaluate(startTabCapture);
    for (let s = 0; s < seconds; s += 2) {
      await new Promise((r) => setTimeout(r, 2000));
      const m = await page.evaluate(pollMedia);
      speech = m.speech || speech;
      peak = { mix: Math.max(peak.mix, m.recRms), tab: Math.max(peak.tab, m.tabRms), meetCtx: Math.max(peak.meetCtx, m.tapRms) };
      say('media', { outs: m.outs, inbound: m.inbound, pageEls: m.pageEls, ourEls: m.ourEls, recRms: m.recRms, tabRms: m.tabRms, taps: m.taps, tapRms: m.tapRms });
    }
    const rtp = await page.evaluate(rtpInfo);
    say('rtp', rtp);
  } finally {
    await browser.close();
    if (pulse) await pulse.stop();
    srv.close();
  }
  say('AUDIO', {
    pulse: pulse ? (fs.existsSync(wav) && wavLevel(fs.readFileSync(wav))) || 'no wav' : 'off',
    mix: { peakDb: db(peak.mix), speech: speechFrac(speech.mix) },
    tab: { peakDb: db(peak.tab), speech: speechFrac(speech.tab), state: tab },
    meetCtx: { peakDb: db(peak.meetCtx), speech: speechFrac(speech.meetCtx) },
    wav: pulse ? wav : null,
  });
}

main().catch((e) => {
  process.stderr.write(`fatal: ${e.stack}\n`);
  process.exit(1);
});
