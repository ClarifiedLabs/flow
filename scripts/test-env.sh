#!/bin/sh

set -eu

if [ "$#" -eq 0 ]; then
	echo "usage: scripts/test-env.sh <command> [args...]" >&2
	exit 2
fi

umask 077

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/.." && pwd)
cache_root="$repo_root/bin/.test-cache"
test_root=$(mktemp -d /tmp/flow-test.XXXXXX)

cleanup() {
	rm -rf "$test_root"
}

trap cleanup 0
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

mkdir -p \
	"$cache_root/go-build" \
	"$cache_root/go-mod" \
	"$test_root/home" \
	"$test_root/config" \
	"$test_root/data" \
	"$test_root/cache" \
	"$test_root/runtime" \
	"$test_root/tmp"

# Test-created files should have stable conventional permissions. The private
# setup above is protected by the temporary root's mode and the initial 077.
umask 022

status=0
/usr/bin/env -i \
	PATH="${PATH:-/usr/local/bin:/usr/bin:/bin}" \
	HOME="$test_root/home" \
	XDG_CONFIG_HOME="$test_root/config" \
	XDG_DATA_HOME="$test_root/data" \
	XDG_CACHE_HOME="$test_root/cache" \
	XDG_RUNTIME_DIR="$test_root/runtime" \
	TMPDIR="$test_root/tmp" \
	TMP="$test_root/tmp" \
	TEMP="$test_root/tmp" \
	GOCACHE="$cache_root/go-build" \
	GOMODCACHE="$cache_root/go-mod" \
	GOENV=off \
	GOWORK=off \
	GIT_CONFIG_GLOBAL=/dev/null \
	GIT_CONFIG_NOSYSTEM=1 \
	GIT_ATTR_NOSYSTEM=1 \
	GIT_TERMINAL_PROMPT=0 \
	LANG=C.UTF-8 \
	LC_ALL=C.UTF-8 \
	LC_CTYPE=C.UTF-8 \
	TZ=UTC \
	"$@" || status=$?

exit "$status"
