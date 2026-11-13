#!/bin/sh
# Reject release workflow versions that are not strict semantic versions
# without a leading v. The tag and image tag add exactly one v prefix.
set -eu

version=${1:-}
if [ -z "$version" ]; then
	echo "release-version: version is required" >&2
	exit 1
fi
case "$version" in
	v* | V*)
		echo "release-version: must not start with v (got $version)" >&2
		exit 1
		;;
esac
case "$version" in
	*[!0-9A-Za-z.-]*)
		echo "release-version: contains characters outside the semantic-version alphabet (got $version)" >&2
		exit 1
		;;
esac
if ! printf '%s\n' "$version" | grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$'; then
	echo "release-version: invalid semantic version without v (got $version)" >&2
	exit 1
fi
