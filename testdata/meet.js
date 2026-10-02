#!/bin/sh
# Fake recorder/meet.js for runner_test.go (a shell script despite the name: the
# runner finds meet.js next to RECORDER_PATH, and tests run it with sh). Writes
# its arguments to <out>.args, a WAV-named --out and the final JSON line without
# tracks. FAKE_MEET_EXIT=3 plays a guest nobody admitted.
set -e

out=""
prev=""
for a in "$@"; do
	[ "$prev" = "--out" ] && out="$a"
	if [ "$a" = "--tracks-dir" ]; then
		echo "fake meet: the runner passed --tracks-dir" >&2
		exit 2
	fi
	prev="$a"
done

if [ "${FAKE_MEET_EXIT:-0}" = 3 ]; then
	echo "fake meet: join timeout, never admitted" >&2
	exit 3
fi

mkdir -p "$(dirname "$out")"
echo "$@" >"$out.args"
echo "fake meet: joined" >&2
printf 'RIFF-fake-wav' >"$out"
printf '{"out":"%s","duration_s":90,"reason":"ended","participants":["Alice"]}\n' "$out"
