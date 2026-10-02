'use strict';

const test = require('node:test');
const assert = require('node:assert');

// Requiring must not launch a browser.
const { parseArgs, foldCaptions, readState, wavLevel, CAPTION_SETTLE_MS } = require('./meet-spike.js');

const URL = 'https://meet.google.com/abc-defg-hij';

test('parseArgs takes --lang and rejects one that is not a language code', () => {
  assert.strictEqual(parseArgs([URL, '--captions', '--lang', 'de-DE']).lang, 'de-DE');
  assert.strictEqual(parseArgs([URL]).lang, '');
  assert.throws(() => parseArgs([URL, '--lang', 'en"]']), /language code/);
});

test('foldCaptions stores each utterance once, with its final text', () => {
  const st = { open: new Map(), done: new Set() };
  const t = 1_000_000;
  const out = [];
  // Meet grows block 1 while Alice talks, then Bob starts block 2.
  out.push(...foldCaptions(st, [{ id: '1', name: 'Alice', text: 'Hello' }], t));
  out.push(...foldCaptions(st, [{ id: '1', name: 'Alice', text: 'Hello every' }], t + 1000));
  out.push(...foldCaptions(st, [{ id: '1', name: 'Alice', text: 'Hello everyone.' }, { id: '2', name: 'Bob', text: 'Hi' }], t + 2000));
  assert.deepStrictEqual(out, [], 'nothing is final while it may still change');
  // Block 1 settles while Bob's newest block keeps changing.
  out.push(...foldCaptions(st, [{ id: '1', name: 'Alice', text: 'Hello everyone.' }, { id: '2', name: 'Bob', text: 'Hi there' }], t + 2000 + CAPTION_SETTLE_MS));
  assert.deepStrictEqual(out, [{ ts: new Date(t).toISOString(), speaker: 'Alice', text: 'Hello everyone.' }]);
  // Seeing a finished block again does not repeat it; a block leaving the DOM finalizes it.
  out.push(...foldCaptions(st, [{ id: '1', name: 'Alice', text: 'Hello everyone.' }], t + 9000));
  assert.deepStrictEqual(out.map((u) => u.text), ['Hello everyone.', 'Hi there']);
  assert.strictEqual(out[1].speaker, 'Bob');
});

test('foldCaptions splits a block Meet restarts, and flush drains the rest', () => {
  const st = { open: new Map(), done: new Set() };
  const long = 'a long monologue that goes on and on';
  assert.deepStrictEqual(foldCaptions(st, [{ id: '7', name: 'Ann', text: long }], 0), []);
  const cut = foldCaptions(st, [{ id: '7', name: 'Ann', text: 'and more' }], 1000);
  assert.deepStrictEqual(cut.map((u) => u.text), [long]);
  const rest = foldCaptions(st, [{ id: '7', name: 'Ann', text: 'and more' }], 1500, true);
  assert.deepStrictEqual(rest, [{ ts: new Date(1000).toISOString(), speaker: 'Ann', text: 'and more' }]);
  assert.deepStrictEqual(foldCaptions(st, [], 2000, true), []);
});

test('foldCaptions: a block cleared on screen ends its turn and may start another', () => {
  const st = { open: new Map(), done: new Set() };
  foldCaptions(st, [{ id: '3', name: 'Ann', text: 'first turn' }], 0);
  const end = foldCaptions(st, [{ id: '3', name: 'Ann', text: '' }], 1000);
  assert.deepStrictEqual(end.map((u) => u.text), ['first turn']);
  foldCaptions(st, [{ id: '3', name: 'Ann', text: 'second turn' }], 2000);
  const rest = foldCaptions(st, [], 3000, true);
  assert.deepStrictEqual(rest.map((u) => u.text), ['second turn']);
});

test('readState: the lobby text wins over a visible Leave call button', () => {
  const page = (text, leave) => {
    global.location = { host: 'meet.google.com', origin: 'https://meet.google.com', pathname: '/abc-defg-hij' };
    global.document = {
      body: { innerText: text },
      querySelector: (sel) => (leave && sel.includes('Leave call') ? {} : null),
    };
  };
  try {
    page('Please wait until a meeting host brings you into the call', true);
    assert.strictEqual(readState().state, 'lobby');
    page('Alice is asking to join', true);
    assert.strictEqual(readState().state, 'admitted', 'an in-call knock notice is not our lobby');
    page('Asking to join...', false);
    assert.strictEqual(readState().state, 'lobby');
  } finally {
    delete global.document;
    delete global.location;
  }
});

test('wavLevel: silence is null, a constant half-scale signal is -6 dBFS, the loudest window wins', () => {
  // 16 kHz mono s16 WAV, as parec writes it: 1 s of silence then 0.2 s at 0.5.
  const wav = (samples) => {
    const h = Buffer.alloc(44);
    h.write('RIFF', 0, 'latin1');
    h.write('WAVEfmt ', 8, 'latin1');
    h.writeUInt16LE(1, 22);
    h.writeUInt32LE(16000, 24);
    h.write('data', 36, 'latin1');
    const d = Buffer.alloc(samples.length * 2);
    samples.forEach((v, i) => d.writeInt16LE(Math.round(v * 32767), i * 2));
    return Buffer.concat([h, d]);
  };
  assert.deepStrictEqual(wavLevel(wav(new Array(16000).fill(0))), { s: 1, rmsDb: null, peakDb: null });
  const l = wavLevel(wav([...new Array(16000).fill(0), ...new Array(3200).fill(0.5)]));
  assert.strictEqual(l.peakDb, -6);
  assert.ok(l.rmsDb < -13 && l.rmsDb > -15, `rmsDb ${l.rmsDb}`);
  assert.strictEqual(wavLevel(Buffer.from('not a wav')), null);
});
