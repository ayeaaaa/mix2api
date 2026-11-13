#!/bin/sh
# Portable launcher. Runs in the foreground with MIX2API_FOREGROUND=1;
# otherwise it daemonizes and writes mix2api.pid/mix2api.log next to the binary.
set -eu

DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
cd "$DIR"

export ACCOUNTS_DIR="${ACCOUNTS_DIR:-$DIR/accounts}"
export USAGE_DIR="${USAGE_DIR:-$DIR/usage}"
export WB_MEDIA_DIR="${WB_MEDIA_DIR:-$DIR/media}"
mkdir -p "$ACCOUNTS_DIR" "$USAGE_DIR" "$WB_MEDIA_DIR"

if [ "${MIX2API_FOREGROUND:-0}" = "1" ]; then
	exec "$DIR/mix2api" "$@"
fi

if [ -f "$DIR/mix2api.pid" ]; then
	pid=$(cat "$DIR/mix2api.pid" 2>/dev/null || true)
	if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
		echo "mix2api already running (pid $pid)" >&2
		exit 0
	fi
	rm -f "$DIR/mix2api.pid"
fi

nohup "$DIR/mix2api" "$@" >>"$DIR/mix2api.log" 2>&1 &
echo $! >"$DIR/mix2api.pid"
echo "mix2api started (pid $(cat "$DIR/mix2api.pid")); log: $DIR/mix2api.log"
