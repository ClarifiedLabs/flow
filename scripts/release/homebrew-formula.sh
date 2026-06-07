#!/usr/bin/env bash
set -euo pipefail

: "${TAG:?TAG is required}"
: "${SOURCE_SHA256:?SOURCE_SHA256 is required}"

tap_dir="${TAP_DIR:?TAP_DIR is required}"
formula_dir="${tap_dir}/Formula"
version="${TAG#v}"
source_url="${SOURCE_URL:-https://github.com/ClarifiedLabs/flow/archive/refs/tags/${TAG}.tar.gz}"

mkdir -p "$formula_dir"

write_binary_formula() {
	local formula="$1"
	local class_name="$2"
	local desc="$3"
	local binary="$4"
	local package="$5"
	local examples_install=""

	case "$binary" in
		flow-server)
			examples_install='    (pkgshare/"examples").install "examples/flow-server.yaml"
    (pkgshare/"examples/docker").install "examples/docker/flow-server.yaml"'
			;;
		flow-worker)
			examples_install='    (pkgshare/"examples").install "examples/flow-worker.yaml"
    (pkgshare/"examples/docker").install "examples/docker/flow-worker.yaml"'
			;;
	esac

	cat >"${formula_dir}/${formula}.rb" <<FORMULA
class ${class_name} < Formula
  desc "${desc}"
  homepage "https://github.com/ClarifiedLabs/flow"
  url "${source_url}"
  sha256 "${SOURCE_SHA256}"
  version "${version}"
  license "MIT"

  depends_on "go" => :build

  def install
    ldflags = %W[
      -s -w
      -X github.com/ClarifiedLabs/flow/internal/version.Version=v#{version}
    ]
    system "go", "build", "-trimpath", "-ldflags", ldflags.join(" "), "-o", bin/"${binary}", "${package}"
${examples_install}
  end

  test do
    assert_match "${binary} v#{version}", shell_output("#{bin}/${binary} --version")
  end
end
FORMULA
}

write_binary_formula flow Flow "CLI for local issue-driven agent work" flow ./cmd/flow
write_binary_formula flow-server FlowServer "Coordinator server for issue-driven agent work" flow-server ./cmd/flow-server
write_binary_formula flow-worker FlowWorker "Worker supervisor for issue-driven agent work" flow-worker ./cmd/flow-worker

cat >"${formula_dir}/flow-full.rb" <<FORMULA
class FlowFull < Formula
  desc "All Flow commands for issue-driven agent work"
  homepage "https://github.com/ClarifiedLabs/flow"
  url "${source_url}"
  sha256 "${SOURCE_SHA256}"
  version "${version}"
  license "MIT"

  depends_on "clarifiedlabs/tap/flow"
  depends_on "clarifiedlabs/tap/flow-server"
  depends_on "clarifiedlabs/tap/flow-worker"

  def install
    pkgshare.install "README.md"
  end

  test do
    flow_bin = Formula["clarifiedlabs/tap/flow"].bin/"flow"
    server_bin = Formula["clarifiedlabs/tap/flow-server"].bin/"flow-server"
    worker_bin = Formula["clarifiedlabs/tap/flow-worker"].bin/"flow-worker"

    assert_match "flow v#{version}", shell_output("#{flow_bin} --version")
    assert_match "flow-server v#{version}", shell_output("#{server_bin} --version")
    assert_match "flow-worker v#{version}", shell_output("#{worker_bin} --version")
  end
end
FORMULA
