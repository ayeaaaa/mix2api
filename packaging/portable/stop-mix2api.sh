#!/bin/sh
# Stop a process started by start-mix2api.sh. Never signals by name.
set -eu

DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
PIDFILE="$DIR/mix2api.pid"

if [ ! -f "$PIDFILE" ]; then
	echo "mix2api is not running (no $PIDFILE)" >&2
	exit 1
fi

pid=$(cat "$PIDFILE" 2>/dev/null || true)
if [ -z "$pid" ] || ! kill -0 "$pid" 2>/dev/null; then
	rm -f "$PIDFILE"
	echo "mix2api is not running (stale pid file removed)" >&2
	exit 1
fi

kill "$pid"
i=0
while kill -0 "$pid" 2>/dev/null && [ "$i" -lt 50 ]; do
	sleep 0.1
	i=$((i + 1))
done
if kill -0 "$pid" 2>/dev/null; then
	echo "mix2api did not stop within 5s; pid $pid left running" >&2
	exit 1
fi
rm -f "$PIDFILE"
echo "mix2api stopped"
