#!/usr/bin/env bash
set -euo pipefail

: "${VERSION:?VERSION is required}"
: "${GOOS:?GOOS is required}"
: "${GOARCH:?GOARCH is required}"

stage_dir="${STAGE_DIR:-dist/bin/${GOOS}-${GOARCH}}"
dist_dir="${DIST_DIR:-dist}"
work_dir="${WORK_DIR:-dist/package-tarball}"
name="flow_${VERSION}_${GOOS}_${GOARCH}"
root="${work_dir}/${name}"

rm -rf "$root"
mkdir -p "$root" "$dist_dir"

install -m 0755 "${stage_dir}/flow" "${root}/flow"
install -m 0755 "${stage_dir}/flow-server" "${root}/flow-server"
install -m 0755 "${stage_dir}/flow-worker" "${root}/flow-worker"
install -m 0644 README.md "${root}/README.md"
install -m 0644 LICENSE "${root}/LICENSE"
mkdir -p "${root}/docs"
install -m 0644 docs/*.md "${root}/docs/"
mkdir -p "${root}/examples"
cp -R examples/. "${root}/examples/"

tar -C "$work_dir" -czf "${dist_dir}/${name}.tar.gz" "$name"
