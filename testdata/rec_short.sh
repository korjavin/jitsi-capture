#!/bin/sh
# Fake recorder: a successful but too-short recording (5 s, under MIN_RECORDING_S).
set -e

out=""
while [ $# -gt 0 ]; do
	case "$1" in
	--out)
		out="$2"
		shift 2
		;;
	*) shift ;;
	esac
done

echo "fake recorder: joined" >&2
mkdir -p "$(dirname "$out")"
printf 'fake-audio' >"$out"
printf '{"out":"%s","duration_s":5,"reason":"empty_room","participants":["Alice"]}\n' "$out"
