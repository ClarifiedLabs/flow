#!/usr/bin/env bash
set -euo pipefail

dist_dir="${DIST_DIR:-dist}"
out="${CHECKSUMS_FILE:-${dist_dir}/checksums.txt}"

tmp="${out}.tmp"
# The output and temporary basenames are explicitly excluded from the input set.
# shellcheck disable=SC2094
find "$dist_dir" -maxdepth 1 -type f ! -name "$(basename "$out")" ! -name "$(basename "$tmp")" -print |
	sort |
	while IFS= read -r file; do
		(cd "$(dirname "$file")" && shasum -a 256 "$(basename "$file")")
	done >"$tmp"
mv "$tmp" "$out"
