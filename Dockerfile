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
    && CGO_ENABLED=1 go build -tags sqlite_fts5 -trimpath -ldflags="$ldflags" -o /out/ ./cmd/flow ./cmd/flow-server ./cmd/flow-worker ./cmd/flow-orchestrator

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

COPY --from=build /out/flow /out/flow-server /out/flow-worker /out/flow-orchestrator /usr/local/bin/
COPY examples /usr/share/flow/examples
VOLUME ["/var/lib/flow"]
EXPOSE 8421 8422
USER flow
CMD ["flow-server", "serve", "--config", "/usr/share/flow/examples/docker/flow-server.yaml"]

FROM flow-server AS flow-orchestrator

ARG VERSION
ARG COMMIT
ARG DATE

LABEL org.opencontainers.image.source="https://github.com/ClarifiedLabs/flow" \
      org.opencontainers.image.title="Flow Orchestrator" \
      org.opencontainers.image.description="Durable assignment reconciler for one-job workers" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${DATE}" \
      org.opencontainers.image.licenses="MIT"

CMD ["flow-orchestrator"]

FROM debian:trixie-slim AS flow-worker

ARG FLOW_UID=1000
ARG FLOW_GID=1000
ARG VERSION
ARG COMMIT
ARG DATE

LABEL org.opencontainers.image.source="https://github.com/ClarifiedLabs/flow" \
      org.opencontainers.image.title="Flow Worker" \
      org.opencontainers.image.description="Flow worker supervisor and agent runtime with the Flow and Harness CLIs (minimal base image; extend it with your own toolchain)" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${DATE}" \
      org.opencontainers.image.licenses="MIT"

ENV HOME=/home/flow \
    FLOW_UID=${FLOW_UID} \
    FLOW_GID=${FLOW_GID}

ARG TARGETARCH
ARG HARNESS_VERSION=v0.5.12
ARG HARNESS_DEB_SHA256_AMD64=1eb6a264d06d4f728b3915794c2ba5cfa81513a51cde8efd7b5b46bc6b0e42b0
ARG HARNESS_DEB_SHA256_ARM64=dd18787b5bbd755349b1a66feb16c64a4386d941fff324bf6031038b435eb0b0

RUN set -eux \
    && apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl git tmux \
    && groupadd --gid "$FLOW_GID" flow \
    && useradd --uid "$FLOW_UID" --gid "$FLOW_GID" --home-dir /home/flow --create-home --shell /bin/bash flow \
    && case "$TARGETARCH" in \
        amd64) harness_deb_arch="amd64"; harness_deb_sha256="$HARNESS_DEB_SHA256_AMD64" ;; \
        arm64) harness_deb_arch="arm64"; harness_deb_sha256="$HARNESS_DEB_SHA256_ARM64" ;; \
        *) echo "unsupported TARGETARCH for worker image: $TARGETARCH" >&2; exit 1 ;; \
    esac \
    && harness_package_version="${HARNESS_VERSION#v}" \
    && harness_deb="/tmp/harness_${harness_package_version}_${harness_deb_arch}.deb" \
    && curl -fsSL "https://github.com/ClarifiedLabs/harness/releases/download/${HARNESS_VERSION}/harness_${harness_package_version}_${harness_deb_arch}.deb" -o "$harness_deb" \
    && printf '%s  %s\n' "$harness_deb_sha256" "$harness_deb" | sha256sum -c - \
    && apt-get install -y --no-install-recommends "$harness_deb" \
    && rm -f "$harness_deb" \
    && rm -rf /var/lib/apt/lists/* \
    && harness --version

COPY --from=build /out/flow /out/flow-worker /usr/local/bin/
EXPOSE 8422
USER flow
# Workers are assignment-created one-shot processes. Orchestrator providers
# override this command with the generated assignment config; standalone runs
# mount an assignment-scoped config at /etc/flow/worker.yaml.
CMD ["flow-worker", "run", "--one-shot", "--config", "/etc/flow/worker.yaml"]
