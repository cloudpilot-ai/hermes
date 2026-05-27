#!/usr/bin/env bash
set -euo pipefail

HERMES_DAEMON_URL="${HERMES_DAEMON_URL:-}"
HERMES_DAEMON_SHA256="${HERMES_DAEMON_SHA256:-}"
HERMES_CONTROLLER_NODE_PORT="${HERMES_CONTROLLER_NODE_PORT:-30080}"
HERMES_ENDPOINT="${HERMES_ENDPOINT:-auto-nodeport}"
HERMES_PLATFORM="${HERMES_PLATFORM:-}"
HERMES_CONFIG_PATH="${HERMES_CONFIG_PATH:-/etc/hermes-daemon/config.toml}"
HERMES_DAEMON_ROOT="${HERMES_DAEMON_ROOT:-/var/lib/hermes-daemon}"
HERMES_DAEMON_ADDRESS="${HERMES_DAEMON_ADDRESS:-/run/hermes-daemon/hermes-daemon.sock}"
RESTART_CONTAINERD="${RESTART_CONTAINERD:-true}"
CONTAINERD_CONFIG="${CONTAINERD_CONFIG:-/etc/containerd/config.toml}"
KUBELET_ENV_FILE="${KUBELET_ENV_FILE:-/etc/eks/kubelet/environment}"

if [[ "${EUID}" -ne 0 ]]; then
  echo "install-hermes-daemon.sh must run as root. In userData this is already root; manually use: curl ... | sudo -E bash" >&2
  exit 1
fi

need_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "missing required command: $1" >&2
    exit 1
  fi
}

need_cmd tar
need_cmd systemctl

if [[ -z "${HERMES_DAEMON_URL}" ]]; then
  echo "HERMES_DAEMON_URL is required and must point to hermes-daemon-linux-<arch>.tar.gz" >&2
  exit 1
fi

case "${HERMES_DAEMON_URL}" in
  http://* | https://*)
    need_cmd curl
    ;;
  file://*)
    ;;
  *)
    if [[ ! -f "${HERMES_DAEMON_URL}" ]]; then
      echo "HERMES_DAEMON_URL must be an http(s) URL, file:// URL, or local path to a tar.gz archive" >&2
      exit 1
    fi
    ;;
esac

if [[ -n "${HERMES_DAEMON_SHA256}" ]]; then
  if [[ ! "${HERMES_DAEMON_SHA256}" =~ ^[0-9a-fA-F]{64}$ ]]; then
    echo "HERMES_DAEMON_SHA256 must be the 64-character hex digest only, not sha256sum output with a filename" >&2
    exit 1
  fi
  need_cmd sha256sum
fi

if [[ "${HERMES_ENDPOINT}" == "auto-nodeport" ]]; then
  need_cmd curl
fi

arch="$(uname -m)"
case "${arch}" in
  x86_64 | amd64)
    arch="amd64"
    ;;
  aarch64 | arm64)
    arch="arm64"
    ;;
  *)
    echo "unsupported architecture: ${arch}" >&2
    exit 1
    ;;
esac

if [[ -z "${HERMES_PLATFORM}" ]]; then
  HERMES_PLATFORM="linux/${arch}"
fi

if [[ "${HERMES_ENDPOINT}" == "auto-nodeport" ]]; then
  token="$(curl -fsS -X PUT "http://169.254.169.254/latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds: 60" || true)"
  if [[ -n "${token}" ]]; then
    node_ip="$(curl -fsS -H "X-aws-ec2-metadata-token: ${token}" http://169.254.169.254/latest/meta-data/local-ipv4)"
  else
    node_ip="$(curl -fsS http://169.254.169.254/latest/meta-data/local-ipv4)"
  fi
  HERMES_ENDPOINT="http://${node_ip}:${HERMES_CONTROLLER_NODE_PORT}"
fi

curl_headers=()
case "${HERMES_DAEMON_URL}" in
  https://github.com/* | https://raw.githubusercontent.com/*)
    if [[ -n "${GITHUB_TOKEN:-}" ]]; then
      curl_headers=(-H "Authorization: Bearer ${GITHUB_TOKEN}")
    fi
    ;;
esac

tmp="$(mktemp -d)"
cleanup() {
  rm -rf "${tmp}"
}
trap cleanup EXIT

archive="${tmp}/hermes-daemon.tar.gz"

echo "Installing Hermes daemon from ${HERMES_DAEMON_URL}"
case "${HERMES_DAEMON_URL}" in
  http://* | https://*)
    curl -fsSL "${curl_headers[@]}" "${HERMES_DAEMON_URL}" -o "${archive}"
    ;;
  file://*)
    cp "${HERMES_DAEMON_URL#file://}" "${archive}"
    ;;
  *)
    cp "${HERMES_DAEMON_URL}" "${archive}"
    ;;
esac

if [[ -n "${HERMES_DAEMON_SHA256}" ]]; then
  actual_sha256="$(sha256sum "${archive}" | awk '{print $1}')"
  if [[ "${actual_sha256}" != "${HERMES_DAEMON_SHA256,,}" ]]; then
    echo "daemon archive sha256 mismatch: expected ${HERMES_DAEMON_SHA256}, got ${actual_sha256}" >&2
    exit 1
  fi
fi

tar -xzf "${archive}" -C "${tmp}"
if [[ ! -f "${tmp}/hermes-daemon" ]]; then
  echo "daemon archive did not contain ./hermes-daemon" >&2
  exit 1
fi
install -m 0755 "${tmp}/hermes-daemon" /usr/local/bin/hermes-daemon

mkdir -p "$(dirname "${HERMES_CONFIG_PATH}")" "${HERMES_DAEMON_ROOT}" "$(dirname "${HERMES_DAEMON_ADDRESS}")"

cat >"${HERMES_CONFIG_PATH}" <<EOF_CONFIG
[pull_modes]
  [pull_modes.soci_v1]
    enable = true

[external_artifact_store]
  enable = true
  endpoint = "${HERMES_ENDPOINT}"
  timeout_sec = 5
  platform = "${HERMES_PLATFORM}"
  fallback_to_registry = true

[cri_keychain]
  enable_keychain = true
  image_service_path = "/run/containerd/containerd.sock"
EOF_CONFIG

cat >/etc/systemd/system/hermes-daemon.service <<EOF_SERVICE
[Unit]
Description=Hermes daemon containerd snapshotter plugin
After=network-online.target
Wants=network-online.target
Before=containerd.service

[Service]
Type=notify
Environment=HOME=/root
ExecStart=/usr/local/bin/hermes-daemon --config ${HERMES_CONFIG_PATH} --root ${HERMES_DAEMON_ROOT} --address ${HERMES_DAEMON_ADDRESS}
Restart=always
RestartSec=5
RuntimeDirectory=hermes-daemon
RuntimeDirectoryMode=0755

[Install]
WantedBy=multi-user.target
EOF_SERVICE

backup_containerd_config() {
  if [[ ! -f "${CONTAINERD_CONFIG}" ]]; then
    echo "containerd config not found: ${CONTAINERD_CONFIG}" >&2
    exit 1
  fi
  cp "${CONTAINERD_CONFIG}" "${CONTAINERD_CONFIG}.pre-hermes-$(date +%Y%m%d%H%M%S)"
}

ensure_proxy_plugin() {
  if grep -q '^\[proxy_plugins\.soci\]' "${CONTAINERD_CONFIG}"; then
    return 0
  fi

  cat >>"${CONTAINERD_CONFIG}" <<EOF_PROXY

[proxy_plugins.soci]
  type = "snapshot"
  address = "${HERMES_DAEMON_ADDRESS}"
  [proxy_plugins.soci.exports]
    root = "${HERMES_DAEMON_ROOT}"
    address = "${HERMES_DAEMON_ADDRESS}"
    enable_remote_snapshot_annotations = "true"
EOF_PROXY
}

enable_cri_snapshotter() {
  local table_path

  set_snapshotter_in_table() {
    local table_path="$1"
    local tmp_config="${CONTAINERD_CONFIG}.hermes"
    local rc

    awk -v table_path="${table_path}" '
      function normalize(line) {
        normalized = line
        gsub(/["\047[:space:]]/, "", normalized)
        return normalized
      }
      function is_section(line) {
        return line ~ /^[[:space:]]*\[/
      }
      function is_target_section(line) {
        return normalize(line) == "[plugins." table_path "]"
      }
      function write_snapshotter_if_missing() {
        if (in_target && !snapshotter_written) {
          print "  snapshotter = \"soci\""
          snapshotter_written = 1
        }
      }
      BEGIN {
        in_target = 0
        target_seen = 0
        snapshotter_written = 0
      }
      is_section($0) {
        write_snapshotter_if_missing()
        in_target = is_target_section($0)
        if (in_target) {
          target_seen = 1
          snapshotter_written = 0
        }
        print
        next
      }
      in_target && $0 ~ /^[[:space:]]*snapshotter[[:space:]]*=/ {
        print "  snapshotter = \"soci\""
        snapshotter_written = 1
        next
      }
      {
        print
      }
      END {
        write_snapshotter_if_missing()
        if (!target_seen) {
          exit 42
        }
      }
    ' "${CONTAINERD_CONFIG}" >"${tmp_config}" || rc=$?

    if [[ "${rc:-0}" -eq 0 ]]; then
      mv "${tmp_config}" "${CONTAINERD_CONFIG}"
      return 0
    fi

    rm -f "${tmp_config}"
    if [[ "${rc}" -eq 42 ]]; then
      return 1
    fi
    echo "failed to update containerd snapshotter table ${table_path}" >&2
    exit "${rc}"
  }

  set_disable_snapshot_annotations_in_table() {
    local table_path="$1"
    local tmp_config="${CONTAINERD_CONFIG}.hermes"
    local rc

    awk -v table_path="${table_path}" '
      function normalize(line) {
        normalized = line
        gsub(/["\047[:space:]]/, "", normalized)
        return normalized
      }
      function is_section(line) {
        return line ~ /^[[:space:]]*\[/
      }
      function is_target_section(line) {
        return normalize(line) == "[plugins." table_path "]"
      }
      function write_option_if_missing() {
        if (in_target && !option_written) {
          print "  disable_snapshot_annotations = false"
          option_written = 1
        }
      }
      BEGIN {
        in_target = 0
        target_seen = 0
        option_written = 0
      }
      is_section($0) {
        write_option_if_missing()
        in_target = is_target_section($0)
        if (in_target) {
          target_seen = 1
          option_written = 0
        }
        print
        next
      }
      in_target && $0 ~ /^[[:space:]]*disable_snapshot_annotations[[:space:]]*=/ {
        print "  disable_snapshot_annotations = false"
        option_written = 1
        next
      }
      {
        print
      }
      END {
        write_option_if_missing()
        if (!target_seen) {
          exit 42
        }
      }
    ' "${CONTAINERD_CONFIG}" >"${tmp_config}" || rc=$?

    if [[ "${rc:-0}" -eq 0 ]]; then
      mv "${tmp_config}" "${CONTAINERD_CONFIG}"
      return 0
    fi

    rm -f "${tmp_config}"
    if [[ "${rc}" -eq 42 ]]; then
      return 1
    fi
    echo "failed to update containerd snapshot annotation setting in ${table_path}" >&2
    exit "${rc}"
  }

  remove_legacy_cri_snapshotter_table() {
    local tmp_config="${CONTAINERD_CONFIG}.hermes"

    awk '
      function normalize(line) {
        normalized = line
        gsub(/["\047[:space:]]/, "", normalized)
        return normalized
      }
      normalize($0) == "[plugins.io.containerd.grpc.v1.cri.containerd]" {
        skip_legacy = 1
        next
      }
      /^[[:space:]]*\[/ {
        skip_legacy = 0
      }
      !skip_legacy {
        print
      }
    ' "${CONTAINERD_CONFIG}" >"${tmp_config}"
    mv "${tmp_config}" "${CONTAINERD_CONFIG}"
  }

  sed -i 's/snapshotter = "overlayfs"/snapshotter = "soci"/g' "${CONTAINERD_CONFIG}"
  sed -i "s/snapshotter = 'overlayfs'/snapshotter = 'soci'/g" "${CONTAINERD_CONFIG}"

  if grep -q 'io\.containerd\.cri\.v1\.images' "${CONTAINERD_CONFIG}" ||
    grep -q '^version = 3' "${CONTAINERD_CONFIG}"; then
    table_path="io.containerd.cri.v1.images"
    remove_legacy_cri_snapshotter_table
  else
    table_path="io.containerd.grpc.v1.cri.containerd"
  fi

  if ! set_snapshotter_in_table "${table_path}"; then
    cat >>"${CONTAINERD_CONFIG}" <<EOF_CRI

[plugins."${table_path}"]
  snapshotter = "soci"
EOF_CRI
  fi

  set_disable_snapshot_annotations_in_table "${table_path}" || true

  if grep -q '^\[plugins\."io\.containerd\.cri\.v1\.images"\]' "${CONTAINERD_CONFIG}" &&
    ! grep -q '^\[\[plugins\."io\.containerd\.transfer\.v1\.local"\.unpack_config\]\]' "${CONTAINERD_CONFIG}"; then
    cat >>"${CONTAINERD_CONFIG}" <<EOF_TRANSFER

[[plugins."io.containerd.transfer.v1.local".unpack_config]]
  platform = "linux"
  snapshotter = "soci"
EOF_TRANSFER
  fi
}

enable_kubelet_image_service_proxy() {
  if [[ ! -f "${KUBELET_ENV_FILE}" ]]; then
    return 0
  fi
  if grep -q -- '--image-service-endpoint=' "${KUBELET_ENV_FILE}"; then
    return 0
  fi
  cp "${KUBELET_ENV_FILE}" "${KUBELET_ENV_FILE}.pre-hermes-$(date +%Y%m%d%H%M%S)"
  sed -i 's#^NODEADM_KUBELET_ARGS=\(.*\)$#NODEADM_KUBELET_ARGS=\1 --image-service-endpoint=unix://'"${HERMES_DAEMON_ADDRESS}"'#' "${KUBELET_ENV_FILE}"
}

backup_containerd_config
ensure_proxy_plugin
enable_cri_snapshotter
enable_kubelet_image_service_proxy

systemctl daemon-reload
systemctl enable --now hermes-daemon.service
systemctl restart hermes-daemon.service

if [[ "${RESTART_CONTAINERD}" == "true" ]]; then
  systemctl restart containerd
fi
if [[ -f "${KUBELET_ENV_FILE}" ]]; then
  systemctl restart kubelet
fi

systemctl is-active hermes-daemon.service
if command -v ctr >/dev/null 2>&1; then
  ctr plugin ls | grep -E '(^TYPE|soci)' || true
fi

echo "Hermes daemon installed. endpoint=${HERMES_ENDPOINT} platform=${HERMES_PLATFORM}"
