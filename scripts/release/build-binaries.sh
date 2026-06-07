#!/usr/bin/env bash
set -euo pipefail

: "${VERSION:?VERSION is required}"
: "${COMMIT:?COMMIT is required}"
: "${DATE:?DATE is required}"
: "${GOOS:?GOOS is required}"
: "${GOARCH:?GOARCH is required}"

out_dir="${OUT_DIR:-dist/bin/${GOOS}-${GOARCH}}"
mkdir -p "$out_dir"

ldflags="-s -w"
ldflags+=" -X github.com/ClarifiedLabs/flow/internal/version.Version=${VERSION}"
ldflags+=" -X github.com/ClarifiedLabs/flow/internal/version.Commit=${COMMIT}"
ldflags+=" -X github.com/ClarifiedLabs/flow/internal/version.Date=${DATE}"

build_one() {
	local name="$1"
	local pkg="$2"
	CGO_ENABLED="${CGO_ENABLED:-1}" GOOS="$GOOS" GOARCH="$GOARCH" go build -trimpath -ldflags "$ldflags" -o "${out_dir}/${name}" "$pkg"
}

build_one flow ./cmd/flow
build_one flow-server ./cmd/flow-server
build_one flow-worker ./cmd/flow-worker
