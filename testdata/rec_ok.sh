#!/bin/sh
# Fake recorder standing in for recorder/record.js in runner_test.go:
# a successful 120 s recording. Follows the CLI contract in recorder/README.md.
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
echo "fake recorder: stopped (empty_room)" >&2
printf '{"out":"%s","duration_s":120,"reason":"empty_room","participants":["Alice","Bob"]}\n' "$out"
