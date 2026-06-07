# syntax=docker/dockerfile:1

ARG GO_VERSION=1.26.4
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown

FROM golang:${GO_VERSION}-trixie AS build

ARG VERSION
ARG COMMIT
ARG DATE

WORKDIR /src
RUN apt-get update \
    && apt-get install -y --no-install-recommends gcc libc6-dev \
    && rm -rf /var/lib/apt/lists/*
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN mkdir -p /out \
    && ldflags="-s -w -X github.com/ClarifiedLabs/flow/internal/version.Version=${VERSION} -X github.com/ClarifiedLabs/flow/internal/version.Commit=${COMMIT} -X github.com/ClarifiedLabs/flow/internal/version.Date=${DATE}" \
    && CGO_ENABLED=1 go build -trimpath -ldflags="$ldflags" -o /out/ ./cmd/flow ./cmd/flow-server ./cmd/flow-worker

FROM debian:trixie-slim AS flow-server

ARG FLOW_UID=1000
ARG FLOW_GID=1000
ARG VERSION
ARG COMMIT
ARG DATE

LABEL org.opencontainers.image.source="https://github.com/ClarifiedLabs/flow" \
      org.opencontainers.image.title="Flow Server" \
      org.opencontainers.image.description="Flow coordinator API and web UI" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${DATE}" \
      org.opencontainers.image.licenses="MIT"

ENV FLOW_DATA_DIR=/var/lib/flow \
    XDG_CONFIG_HOME=/var/lib/flow/config \
    HOME=/home/flow \
    FLOW_UID=${FLOW_UID} \
    FLOW_GID=${FLOW_GID}

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl git \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid "$FLOW_GID" flow \
    && useradd --uid "$FLOW_UID" --gid "$FLOW_GID" --home-dir /home/flow --create-home --shell /bin/bash flow \
    && mkdir -p /var/lib/flow/config \
    && chown -R flow:flow /home/flow /var/lib/flow

COPY --from=build /out/flow /out/flow-server /out/flow-worker /usr/local/bin/
COPY examples /usr/share/flow/examples
VOLUME ["/var/lib/flow"]
EXPOSE 8421
USER flow
CMD ["flow-server", "serve", "--config", "/usr/share/flow/examples/docker/flow-server.yaml"]

FROM flow-server AS flow-worker

ARG VERSION
ARG COMMIT
ARG DATE

LABEL org.opencontainers.image.source="https://github.com/ClarifiedLabs/flow" \
      org.opencontainers.image.title="Flow Worker" \
      org.opencontainers.image.description="Flow worker supervisor and agent runtime" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${DATE}" \
      org.opencontainers.image.licenses="MIT"

USER root
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

ARG TARGETARCH
ARG TEMURIN_JDK_VERSION=25
ARG NVM_VERSION=0.40.5
ARG NODE_VERSION=24.17.0
ARG RUST_VERSION=1.96.0
ARG TTYD_VERSION=1.7.7
ARG HARNESS_VERSION=v0.0.13
ARG HARNESS_DEB_SHA256_AMD64=2bb421212690ebbf5022765a86db390081786dee8ea13c90b5ef68e2e24116e6
ARG HARNESS_DEB_SHA256_ARM64=047a4fa7683fbf5117b69480e15b1b956db0dfbb576eec610ac5463a3155bacf
ARG CLAUDE_CODE_VERSION=2.1.185-1
ARG CLAUDE_CODE_DEB_SHA256_AMD64=1517d2fc507e6f448c7ee894f549194c5b74420b7f5c52abd1eef5f43c6bb701
ARG CLAUDE_CODE_DEB_SHA256_ARM64=31b0da94355f5f62baf67e94843659c92ce794db819ecec532827c877cdf008d
ARG CODEX_VERSION=0.141.0
ARG CODEX_PACKAGE_SHA256_AMD64=091c8a2e27370c41407fa1cb647fe905bd4fd70e4689c13effee0a2dce1b2b07
ARG CODEX_PACKAGE_SHA256_ARM64=b70030338592de3e361f3cde83d624f88061df300abe31b62075a5c5a058a6fc

ENV NVM_DIR=/usr/local/share/nvm \
    NODE_VERSION=${NODE_VERSION} \
    RUSTUP_HOME=/usr/local/rustup \
    CARGO_HOME=/usr/local/cargo \
    JAVA_HOME=/usr/local/share/temurin-jdk \
    BASH_ENV=/etc/profile.d/nvm.sh \
    XDG_RUNTIME_DIR=/run/user/${FLOW_UID} \
    DOCKER_HOST=unix:///run/user/${FLOW_UID}/docker.sock
ENV PATH=/usr/local/go/bin:${NVM_DIR}/versions/node/v${NODE_VERSION}/bin:${CARGO_HOME}/bin:${PATH}

COPY --from=build /usr/local/go /usr/local/go

RUN set -eux \
    && apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl gpg \
    && install -m 0755 -d /etc/apt/keyrings \
    && curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc \
    && chmod a+r /etc/apt/keyrings/docker.asc \
    && curl -fsSL https://packages.adoptium.net/artifactory/api/gpg/key/public -o /tmp/adoptium.asc \
    && gpg --batch --dearmor -o /etc/apt/keyrings/adoptium.gpg /tmp/adoptium.asc \
    && chmod a+r /etc/apt/keyrings/adoptium.gpg \
    && . /etc/os-release \
    && printf 'Types: deb\nURIs: https://download.docker.com/linux/debian\nSuites: %s\nComponents: stable\nArchitectures: %s\nSigned-By: /etc/apt/keyrings/docker.asc\n' "$VERSION_CODENAME" "$(dpkg --print-architecture)" > /etc/apt/sources.list.d/docker.sources \
    && printf 'deb [signed-by=/etc/apt/keyrings/adoptium.gpg] https://packages.adoptium.net/artifactory/deb %s main\n' "$VERSION_CODENAME" > /etc/apt/sources.list.d/adoptium.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        age \
        autoconf \
        automake \
        bash \
        bat \
        bind9-dnsutils \
        bison \
        build-essential \
        clang \
        cmake \
        containerd.io \
        dbus-user-session \
        docker-buildx-plugin \
        docker-ce \
        docker-ce-cli \
        docker-ce-rootless-extras \
        docker-compose-plugin \
        fd-find \
        file \
        flex \
        fuse-overlayfs \
        fzf \
        gdb \
        gh \
        gnupg \
        hyperfine \
        iproute2 \
        iptables \
        iputils-ping \
        jq \
        less \
        libcurl4-openssl-dev \
        libffi-dev \
        libsqlite3-dev \
        libssl-dev \
        libtool \
        lld \
        llvm \
        lsof \
        netcat-openbsd \
        ninja-build \
        openssh-client \
        pipx \
        pkg-config \
        procps \
        psmisc \
        python3 \
        python3-dev \
        python3-pip \
        python3-venv \
        python3-yaml \
        ripgrep \
        rsync \
        shellcheck \
        shfmt \
        slirp4netns \
        sqlite3 \
        strace \
        temurin-${TEMURIN_JDK_VERSION}-jdk \
        tini \
        tmux \
        tree \
        uidmap \
        unzip \
        vim-tiny \
        wget \
        xz-utils \
        yq \
        zip \
        zlib1g-dev \
        zsh \
    && ln -sfn /usr/bin/batcat /usr/local/bin/bat \
    && ln -sfn /usr/bin/fdfind /usr/local/bin/fd \
    && if ! command -v docker-compose >/dev/null 2>&1 && [ -x /usr/libexec/docker/cli-plugins/docker-compose ]; then \
        ln -sfn /usr/libexec/docker/cli-plugins/docker-compose /usr/local/bin/docker-compose; \
    fi \
    && jdk_home="$(dirname "$(dirname "$(readlink -f "$(command -v javac)")")")" \
    && ln -sfn "$jdk_home" "$JAVA_HOME" \
    && mkdir -p "$XDG_RUNTIME_DIR" /home/flow/.local/share/flow /home/flow/.local/share/docker /run/flow-worker/codex \
    && chmod 0700 "$XDG_RUNTIME_DIR" \
    && if ! grep -q '^flow:' /etc/subuid; then echo 'flow:100000:65536' >> /etc/subuid; fi \
    && if ! grep -q '^flow:' /etc/subgid; then echo 'flow:100000:65536' >> /etc/subgid; fi \
    && case "$TARGETARCH" in \
        amd64) \
            ttyd_arch="x86_64"; \
            harness_deb_arch="amd64"; \
            harness_deb_sha256="$HARNESS_DEB_SHA256_AMD64"; \
            claude_deb_arch="amd64"; \
            claude_deb_sha256="$CLAUDE_CODE_DEB_SHA256_AMD64"; \
            codex_target="x86_64-unknown-linux-musl"; \
            codex_package_sha256="$CODEX_PACKAGE_SHA256_AMD64" \
            ;; \
        arm64) \
            ttyd_arch="aarch64"; \
            harness_deb_arch="arm64"; \
            harness_deb_sha256="$HARNESS_DEB_SHA256_ARM64"; \
            claude_deb_arch="arm64"; \
            claude_deb_sha256="$CLAUDE_CODE_DEB_SHA256_ARM64"; \
            codex_target="aarch64-unknown-linux-musl"; \
            codex_package_sha256="$CODEX_PACKAGE_SHA256_ARM64" \
            ;; \
        *) echo "unsupported TARGETARCH for worker image: $TARGETARCH" >&2; exit 1 ;; \
    esac \
    && harness_package_version="${HARNESS_VERSION#v}" \
    && harness_deb="/tmp/harness_${harness_package_version}_${harness_deb_arch}.deb" \
    && curl -fsSL "https://github.com/ClarifiedLabs/harness/releases/download/${HARNESS_VERSION}/harness_${harness_package_version}_${harness_deb_arch}.deb" -o "$harness_deb" \
    && printf '%s  %s\n' "$harness_deb_sha256" "$harness_deb" | sha256sum -c - \
    && apt-get install -y --no-install-recommends "$harness_deb" \
    && rm -f "$harness_deb" \
    && curl -fsSL "https://github.com/tsl0922/ttyd/releases/download/${TTYD_VERSION}/ttyd.${ttyd_arch}" -o /usr/local/bin/ttyd \
    && chmod 0755 /usr/local/bin/ttyd \
    && claude_deb="/tmp/claude-code_${CLAUDE_CODE_VERSION}_${claude_deb_arch}.deb" \
    && curl -fsSL "https://downloads.claude.ai/claude-code/apt/latest/pool/main/c/claude-code/claude-code_${CLAUDE_CODE_VERSION}_${claude_deb_arch}.deb" -o "$claude_deb" \
    && printf '%s  %s\n' "$claude_deb_sha256" "$claude_deb" | sha256sum -c - \
    && apt-get install -y --no-install-recommends "$claude_deb" \
    && rm -f "$claude_deb" \
    && codex_archive="/tmp/codex-package-${codex_target}.tar.gz" \
    && codex_release_dir="/usr/local/share/codex/packages/standalone/releases/${CODEX_VERSION}-${codex_target}" \
    && curl -fsSL "https://github.com/openai/codex/releases/download/rust-v${CODEX_VERSION}/codex-package-${codex_target}.tar.gz" -o "$codex_archive" \
    && printf '%s  %s\n' "$codex_package_sha256" "$codex_archive" | sha256sum -c - \
    && mkdir -p "$codex_release_dir" /run/flow-worker/codex \
    && tar -xzf "$codex_archive" -C "$codex_release_dir" \
    && chmod 0755 "$codex_release_dir/bin/codex" "$codex_release_dir/codex-path/rg" \
    && chmod 0755 "$codex_release_dir/codex-resources/bwrap" \
    && ln -sfn "$codex_release_dir" /usr/local/share/codex/packages/standalone/current \
    && ln -sfn /usr/local/share/codex/packages/standalone/current/bin/codex /usr/local/bin/codex \
    && rm -f "$codex_archive" \
    && rm -f /tmp/adoptium.asc \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p "$NVM_DIR" \
    && curl -fsSL "https://raw.githubusercontent.com/nvm-sh/nvm/v${NVM_VERSION}/install.sh" -o /tmp/install-nvm.sh \
    && PROFILE=/dev/null bash /tmp/install-nvm.sh \
    && . "$NVM_DIR/nvm.sh" \
    && nvm install "$NODE_VERSION" \
    && nvm alias default "$NODE_VERSION" \
    && nvm use default \
    && corepack enable \
    && printf 'export NVM_DIR="%s"\n[ -s "$NVM_DIR/nvm.sh" ] && . "$NVM_DIR/nvm.sh"\n' "$NVM_DIR" > /etc/profile.d/nvm.sh \
    && chmod 0644 /etc/profile.d/nvm.sh \
    && rm -f /tmp/install-nvm.sh \
    && curl -fsSL https://sh.rustup.rs -o /tmp/rustup-init.sh \
    && sh /tmp/rustup-init.sh -y --no-modify-path --profile default --default-toolchain "$RUST_VERSION" \
    && rustup component add clippy rustfmt rust-src \
    && rm -f /tmp/rustup-init.sh \
    && chown -R flow:flow \
        "$NVM_DIR" \
        "$RUSTUP_HOME" \
        "$CARGO_HOME" \
        /home/flow \
        /run/flow-worker \
        "$XDG_RUNTIME_DIR" \
    && go version \
    && bash -lc 'nvm --version && node --version && npm --version' \
    && rustc --version \
    && cargo --version \
    && java -version \
    && clang --version \
    && docker --version \
    && docker compose version \
    && age --version \
    && gpg --version \
    && ssh -V \
    && claude --version \
    && codex --version

COPY --chmod=0644 docker/flow-worker-profile.sh /etc/profile.d/flow-dev-tools.sh
COPY --chmod=0755 docker/flow-worker-entrypoint.sh /usr/local/bin/flow-worker-entrypoint

ENV BASH_ENV=/etc/profile.d/flow-dev-tools.sh

RUN printf '\n[ -r /etc/profile.d/flow-dev-tools.sh ] && . /etc/profile.d/flow-dev-tools.sh\n' >> /home/flow/.bashrc \
    && chown flow:flow /home/flow/.bashrc

VOLUME ["/home/flow/.local/share/flow", "/home/flow/.local/share/docker"]
USER flow
ENTRYPOINT ["tini", "-g", "--", "flow-worker-entrypoint"]
CMD ["flow-worker", "-c", "/usr/share/flow/examples/docker/flow-worker.yaml"]
