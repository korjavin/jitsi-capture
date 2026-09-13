#!/bin/sh
# Fake recorder: never admitted to the conference (exit 3), nothing written.
echo "fake recorder: join timeout, never admitted" >&2
exit 3
