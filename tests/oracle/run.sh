#!/bin/sh
# Thin wrapper around the oracle harness. The target must be loopback.
set -eu

if [ "$#" -lt 1 ]; then
	echo "usage: tests/oracle/run.sh record|compare [flags]" >&2
	exit 2
fi

cmd=$1
shift
cd "$(dirname "$0")/../.."
exec go run ./tests/oracle/cmd/oracle "$cmd" "$@"
