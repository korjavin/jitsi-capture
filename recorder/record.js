#!/usr/bin/env node
'use strict';

// ponytail: stub only - the Jitsi/Puppeteer recorder and its real CLI contract
// land in the recorder bead. Until then: usage on stderr, exit 2.
const USAGE = 'usage: record.js --url <jitsi-meeting-url> --out <audio.wav> [--max-seconds <n>]\n';

if (require.main === module) {
  process.stderr.write(USAGE);
  process.exit(2);
}

module.exports = { USAGE };
