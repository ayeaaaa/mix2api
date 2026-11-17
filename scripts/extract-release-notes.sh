#!/bin/sh
# Print only the CHANGELOG section for one release version.
#
# Usage: scripts/extract-release-notes.sh VERSION [CHANGELOG]
# VERSION is the semantic version without a leading v, for example 2.8.8.
set -eu

version=${1:-}
changelog=${2:-CHANGELOG.md}
if [ -z "$version" ]; then
	echo "usage: $0 VERSION [CHANGELOG]" >&2
	exit 2
fi
if [ ! -f "$changelog" ]; then
	echo "changelog not found: $changelog" >&2
	exit 2
fi

prefix="## [$version]"
awk -v prefix="$prefix" '
	index($0, prefix) == 1 { found = 1 }
	found && /^## \[/ && index($0, prefix) != 1 { exit }
	found { print }
	END { if (!found) exit 1 }
' "$changelog"
