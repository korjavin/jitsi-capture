#!/bin/sh
# Fake recorder/spike/meet-spike.js for spike_test.go: records its arguments,
# prints log lines in the script's format and writes a non-empty mixed.webm.
# FAKE_SPIKE_SLEEP holds it open so a second run can find it busy.
out=""
for a in "$@"; do
	[ "$prev" = "--out-dir" ] && out="$a"
	prev="$a"
done
mkdir -p "$out"
echo "$@" >"$out/args"
sleep "${FAKE_SPIKE_SLEEP:-0}"
echo '[+0.1s] launch {"mode":"anonymous guest"}' >&2
echo '[+2.0s] STATE - -> prejoin {"why":"ask to join"}' >&2
echo '[+9.0s] STATE prejoin -> admitted {"why":"Leave call button"}' >&2
for i in 1 2 3 4; do echo "[+1$i.0s] media {\"liveAudioTracks\":$i}" >&2; done
echo '[+15.0s] names {"tiles":["Alice"]}' >&2
echo '[+16.0s] noise that is not reported' >&2
printf 'fake-audio' >"$out/mixed.webm"
echo '[+20.0s] SUMMARY {"result":"admitted"}' >&2
