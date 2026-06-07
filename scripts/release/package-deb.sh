#!/usr/bin/env bash
set -euo pipefail

: "${VERSION:?VERSION is required}"
: "${GOARCH:?GOARCH is required}"

stage_dir="${STAGE_DIR:-dist/bin/linux-${GOARCH}}"
dist_dir="${DIST_DIR:-dist}"
version="${VERSION#v}"

case "$GOARCH" in
	amd64) deb_arch="amd64" ;;
	arm64) deb_arch="arm64" ;;
	*) echo "unsupported GOARCH for deb: ${GOARCH}" >&2; exit 2 ;;
esac

pkgroot="${WORK_DIR:-dist/package-deb}/flow_${version}_${deb_arch}"
rm -rf "$pkgroot"
mkdir -p "${pkgroot}/DEBIAN" "${pkgroot}/usr/bin" "${pkgroot}/usr/share/doc/flow/docs" "${pkgroot}/usr/share/flow/examples"

install -m 0755 "${stage_dir}/flow" "${pkgroot}/usr/bin/flow"
install -m 0755 "${stage_dir}/flow-server" "${pkgroot}/usr/bin/flow-server"
install -m 0755 "${stage_dir}/flow-worker" "${pkgroot}/usr/bin/flow-worker"
install -m 0644 README.md "${pkgroot}/usr/share/doc/flow/README.md"
install -m 0644 LICENSE "${pkgroot}/usr/share/doc/flow/LICENSE"
install -m 0644 docs/*.md "${pkgroot}/usr/share/doc/flow/docs/"
cp -R examples/. "${pkgroot}/usr/share/flow/examples/"

installed_size="$(du -sk "${pkgroot}/usr" | awk '{print $1}')"
cat >"${pkgroot}/DEBIAN/control" <<CONTROL
Package: flow
Version: ${version}
Section: utils
Priority: optional
Architecture: ${deb_arch}
Maintainer: Clarified Labs <opensource@clarifiedlabs.com>
Installed-Size: ${installed_size}
Homepage: https://github.com/ClarifiedLabs/flow
Description: Local coordinator for issue-driven agent work
 Flow is a local coordinator for issue-driven agent work. It keeps durable state
 in SQLite, uses a private git exchange remote for worker branches, and exposes a CLI and browser UI.
CONTROL

mkdir -p "$dist_dir"
dpkg-deb --build --root-owner-group "$pkgroot" "${dist_dir}/flow_${version}_${deb_arch}.deb"
