#!/usr/bin/env bash
set -euo pipefail

: "${VERSION:?VERSION is required}"
: "${GOARCH:?GOARCH is required}"

repo_root="$(pwd -P)"

abs_path() {
	case "$1" in
		/*) printf '%s\n' "$1" ;;
		*) printf '%s/%s\n' "$repo_root" "$1" ;;
	esac
}

stage_dir="$(abs_path "${STAGE_DIR:-dist/bin/linux-${GOARCH}}")"
dist_dir="$(abs_path "${DIST_DIR:-dist}")"
version="${VERSION#v}"

case "$GOARCH" in
	amd64) rpm_arch="x86_64" ;;
	arm64) rpm_arch="aarch64" ;;
	*) echo "unsupported GOARCH for rpm: ${GOARCH}" >&2; exit 2 ;;
esac

topdir="$(abs_path "${WORK_DIR:-dist/package-rpm}")/rpmbuild"
readme_path="$(abs_path README.md)"
license_path="$(abs_path LICENSE)"
docs_path="$(abs_path docs)"
examples_path="$(abs_path examples)"
rm -rf "$topdir"
mkdir -p "${topdir}/BUILD" "${topdir}/BUILDROOT" "${topdir}/RPMS" "${topdir}/SOURCES" "${topdir}/SPECS" "$dist_dir"

spec="${topdir}/SPECS/flow.spec"
cat >"$spec" <<SPEC
Name: flow
Version: ${version}
Release: 1%{?dist}
Summary: Local coordinator for issue-driven agent work
License: MIT
URL: https://github.com/ClarifiedLabs/flow
BuildArch: ${rpm_arch}
AutoReqProv: no

%description
Flow is a local coordinator for issue-driven agent work. It keeps durable state
in SQLite, uses a private git exchange remote for worker branches, and exposes a CLI and browser UI.

%prep

%build

%install
mkdir -p %{buildroot}/usr/bin
mkdir -p %{buildroot}/usr/share/doc/flow
mkdir -p %{buildroot}/usr/share/doc/flow/docs
mkdir -p %{buildroot}/usr/share/flow/examples
mkdir -p %{buildroot}/usr/share/licenses/flow
install -m 0755 "${stage_dir}/flow" %{buildroot}/usr/bin/flow
install -m 0755 "${stage_dir}/flow-server" %{buildroot}/usr/bin/flow-server
install -m 0755 "${stage_dir}/flow-worker" %{buildroot}/usr/bin/flow-worker
install -m 0644 "${readme_path}" %{buildroot}/usr/share/doc/flow/README.md
install -m 0644 "${docs_path}"/*.md %{buildroot}/usr/share/doc/flow/docs/
cp -R "${examples_path}"/. %{buildroot}/usr/share/flow/examples/
install -m 0644 "${license_path}" %{buildroot}/usr/share/licenses/flow/LICENSE

%files
/usr/bin/flow
/usr/bin/flow-server
/usr/bin/flow-worker
%doc /usr/share/doc/flow/README.md
%doc /usr/share/doc/flow/docs/*.md
/usr/share/flow/examples
%license /usr/share/licenses/flow/LICENSE
SPEC

rpmbuild --define "_topdir ${topdir}" --target "${rpm_arch}" -bb "$spec"
rpm_path="$(find "${topdir}/RPMS" -type f -name '*.rpm' -print | sort | awk 'END {print}')"
if [[ -z "$rpm_path" ]]; then
	echo "rpmbuild did not produce an rpm" >&2
	exit 1
fi
cp "$rpm_path" "${dist_dir}/flow-${version}-1.${rpm_arch}.rpm"
