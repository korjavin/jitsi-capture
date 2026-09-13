#!/bin/sh
# Fake recorder standing in for recorder/record.js in runner_test.go:
# a successful 120 s recording with one per-participant track. Follows the CLI
# contract in recorder/README.md.
set -e

out=""
tracks_dir=""
while [ $# -gt 0 ]; do
	case "$1" in
	--out)
		out="$2"
		shift 2
		;;
	--tracks-dir)
		tracks_dir="$2"
		shift 2
		;;
	*) shift ;;
	esac
done

if [ -z "$tracks_dir" ]; then
	echo "fake recorder: the runner did not pass --tracks-dir" >&2
	exit 2
fi

echo "fake recorder: joined" >&2
mkdir -p "$(dirname "$out")" "$tracks_dir"
printf 'fake-audio' >"$out"
printf 'fake-track' >"$tracks_dir/p1.webm"
echo "fake recorder: stopped (empty_room)" >&2
printf '{"out":"%s","duration_s":120,"reason":"empty_room","participants":["Alice","Bob"],"tracks":[{"id":"p1","name":"Alice","path":"%s/p1.webm","offset_s":1.5,"ended_s":119}]}\n' "$out" "$tracks_dir"
