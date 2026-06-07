# shellcheck shell=sh

export FLOW_UID="${FLOW_UID:-1000}"
export NVM_DIR="${NVM_DIR:-/usr/local/share/nvm}"
export NODE_VERSION="${NODE_VERSION:-24.17.0}"
export RUSTUP_HOME="${RUSTUP_HOME:-/usr/local/rustup}"
export CARGO_HOME="${CARGO_HOME:-/usr/local/cargo}"
export JAVA_HOME="${JAVA_HOME:-/usr/local/share/temurin-jdk}"
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/${FLOW_UID}}"
export DOCKER_HOST="${DOCKER_HOST:-unix://${XDG_RUNTIME_DIR}/docker.sock}"

if [ -s "$NVM_DIR/nvm.sh" ]; then
  # shellcheck source=/dev/null
  . "$NVM_DIR/nvm.sh"
fi

flow_prepend_path() {
  case ":$PATH:" in
  *":$1:"*) ;;
  *) PATH="$1${PATH:+:$PATH}" ;;
  esac
}

flow_prepend_path /usr/local/go/bin
flow_prepend_path "$NVM_DIR/versions/node/v$NODE_VERSION/bin"
flow_prepend_path "$CARGO_HOME/bin"
export PATH
unset -f flow_prepend_path
