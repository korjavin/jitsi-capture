'use strict';

const test = require('node:test');
const assert = require('node:assert');

// Requiring must not run the CLI (it would exit the test process).
const { USAGE } = require('./record.js');

test('usage names the required flags', () => {
  assert.match(USAGE, /--url/);
  assert.match(USAGE, /--out/);
});
