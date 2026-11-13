#!/bin/sh
# Build the deterministic release asset set. All inputs may be overridden from
# the environment; the Go tool owns the target matrix and archive format.
set -eu

cd "$(dirname "$0")/.."

VERSION=${VERSION:-}
COMMIT=${COMMIT:-$(git rev-parse HEAD 2>/dev/null || echo unknown)}
BUILD_DATE=${BUILD_DATE:-$(git show -s --format=%cI HEAD 2>/dev/null || true)}
OUT=${OUT:-dist}

set -- "$@" --out "$OUT" --commit "$COMMIT"
[ -n "$VERSION" ] && set -- "$@" --version "$VERSION"
[ -n "$BUILD_DATE" ] && set -- "$@" --date "$BUILD_DATE"
# Let SOURCE_DATE_EPOCH set the archive timestamp when provided; otherwise the
# tool uses 1980-01-01. Archive timestamps must never be "now".
exec go run ./tools/release "$@"
