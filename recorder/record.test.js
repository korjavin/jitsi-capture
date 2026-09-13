'use strict';

const test = require('node:test');
const assert = require('node:assert');

// Requiring must not launch a browser or run the CLI.
const { USAGE, parseArgs, buildUrl, roomName, shouldStop } = require('./record.js');

const MIN = ['--url', 'https://jitsi.example.com/room-abc', '--out', '/tmp/a.webm'];

test('usage names the required flags', () => {
  assert.match(USAGE, /--url/);
  assert.match(USAGE, /--out/);
});

test('parseArgs applies the contract defaults', () => {
  assert.deepStrictEqual(parseArgs(MIN), {
    url: 'https://jitsi.example.com/room-abc',
    out: '/tmp/a.webm',
    joinTimeout: 600,
    maxDuration: 14400,
    emptyGrace: 60,
    displayName: 'NoteTaker',
  });
});

test('parseArgs overrides every flag', () => {
  const opts = parseArgs([
    ...MIN,
    '--join-timeout', '30',
    '--max-duration', '120',
    '--empty-grace', '5',
    '--display-name', 'Someone Else',
  ]);
  assert.strictEqual(opts.joinTimeout, 30);
  assert.strictEqual(opts.maxDuration, 120);
  assert.strictEqual(opts.emptyGrace, 5);
  assert.strictEqual(opts.displayName, 'Someone Else');
});

test('parseArgs rejects bad input', () => {
  assert.throws(() => parseArgs(['--out', '/tmp/a.webm']), /--url/);
  assert.throws(() => parseArgs(['--url', 'https://jitsi.example.com/r']), /--out/);
  assert.throws(() => parseArgs([...MIN, '--nope', '1']), /unknown argument/);
  assert.throws(() => parseArgs([...MIN, '--join-timeout']), /missing value/);
  assert.throws(() => parseArgs([...MIN, '--empty-grace', 'soon']), /positive number/);
  assert.throws(() => parseArgs([...MIN, '--max-duration', '0']), /positive number/);
});

test('buildUrl puts the join config in the hash and replaces any existing one', () => {
  const url = buildUrl('https://jitsi.example.com/room-abc#stale=1', 'NoteTaker');
  assert.strictEqual(
    url,
    'https://jitsi.example.com/room-abc' +
      '#config.prejoinConfig.enabled=false' +
      '&config.startWithAudioMuted=true' +
      '&config.startWithVideoMuted=true' +
      '&userInfo.displayName=%22NoteTaker%22'
  );
});

test('roomName logs the room, never the credentials in the URL', () => {
  assert.strictEqual(roomName('https://jitsi.example.com/room-abc?jwt=secret'), 'room-abc');
  assert.strictEqual(roomName('not a url'), '(unparseable-url)');
});

const BASE = { now: 10_000, emptyGrace: 60, startedAt: 0, maxDuration: 14400 };

test('shouldStop keeps recording while others are in the room', () => {
  assert.deepStrictEqual(shouldStop({ ...BASE, membersCount: 3, aloneSince: null }), {
    reason: null,
    aloneSince: null,
  });
});

test('shouldStop starts the alone timer and waits out the grace period', () => {
  const started = shouldStop({ ...BASE, membersCount: 1, aloneSince: null });
  assert.deepStrictEqual(started, { reason: null, aloneSince: 10_000 });

  const almost = shouldStop({ ...BASE, now: 69_000, membersCount: 1, aloneSince: 10_000 });
  assert.strictEqual(almost.reason, null);

  const expired = shouldStop({ ...BASE, now: 70_000, membersCount: 1, aloneSince: 10_000 });
  assert.strictEqual(expired.reason, 'empty_room');
});

test('shouldStop resets the alone timer when somebody rejoins', () => {
  const rejoined = shouldStop({ ...BASE, now: 69_000, membersCount: 2, aloneSince: 10_000 });
  assert.deepStrictEqual(rejoined, { reason: null, aloneSince: null });

  // ...and the grace period restarts from the new alone moment.
  const aloneAgain = shouldStop({ ...BASE, now: 70_000, membersCount: 1, aloneSince: null });
  assert.deepStrictEqual(aloneAgain, { reason: null, aloneSince: 70_000 });
});

test('shouldStop stops at max duration, even with a full room', () => {
  const hit = shouldStop({ ...BASE, now: 14_400_000, membersCount: 5, aloneSince: null });
  assert.strictEqual(hit.reason, 'max_duration');
});

test('shouldStop prefers max_duration over empty_room when both fire', () => {
  const both = shouldStop({ ...BASE, now: 14_400_000, membersCount: 1, aloneSince: 0 });
  assert.strictEqual(both.reason, 'max_duration');
});
