#!/bin/sh
# Run the manual v1.6.5 baseline in a private network namespace.
#
# Usage: tests/load/run-v1-baseline.sh /tmp/v1-baseline.json
set -eu

if [ "$#" -ne 1 ]; then
	echo "usage: $0 EVIDENCE.json" >&2
	exit 2
fi

cd "$(dirname "$0")/../.."
evidence=$1
exec unshare -n -- sh -c '
	set -eu
	ip link set lo up 2>/dev/null || ifconfig lo up 2>/dev/null || true
	MIX2API_LOAD_V1=1 MIX2API_LOAD_NETNS=1 MIX2API_LOAD_EVIDENCE="$1" \
		go test ./tests/load -run TestV1Baseline -v -count=1 -timeout 5m
' sh "$evidence"
