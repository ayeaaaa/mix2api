#!/bin/sh
# Run the manual v2 baseline in a private network namespace against the shared
# oracle fake upstream. The v2 test normally starts an in-process fake; this
# wrapper points it at the same Python fake used by the v1 baseline.
#
# Usage: tests/load/run-v2-baseline.sh /tmp/v2-baseline.json
set -eu

if [ "$#" -ne 1 ]; then
	echo "usage: $0 EVIDENCE.json" >&2
	exit 2
fi

cd "$(dirname "$0")/../.."
evidence=$1
commit=$(git rev-parse HEAD)
exec unshare -n -- sh -c '
	set -eu
	ip link set lo up 2>/dev/null || ifconfig lo up 2>/dev/null || true
	tmp=$(mktemp -d)
	python3 tests/oracle/fakes/fake_upstream.py --info "$tmp/fake.json" \
		>"$tmp/fake.log" 2>&1 &
	fake_pid=$!
	trap "kill $fake_pid 2>/dev/null || true" EXIT
	for _ in $(seq 1 200); do
		[ -s "$tmp/fake.json" ] && break
		sleep 0.05
	done
	test -s "$tmp/fake.json"
	wb=$(python3 -c "import json,sys;print(json.load(open(sys.argv[1]))[\"workbuddy\"])" "$tmp/fake.json")
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost \
		MIX2API_LOAD=1 MIX2API_LOAD_UPSTREAM="$wb" MIX2API_LOAD_EVIDENCE="$1" \
		MIX2API_LOAD_TARGET_VERSION=2.0.0-rc.1 MIX2API_LOAD_TARGET_COMMIT="$2" \
		go test ./tests/load -run TestSSELoad -v -count=1 -timeout 5m
' sh "$evidence" "$commit"
