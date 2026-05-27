#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
NODE="${KIND_NODE:-hermes-control-plane}"
HERMES_ENDPOINT="${HERMES_ENDPOINT:-http://127.0.0.1:30080}"
ENABLE_SOCI_CRI="${ENABLE_SOCI_CRI:-true}"
RESTART_CONTAINERD="${RESTART_CONTAINERD:-true}"
REGISTRY_AUTH_HOST="${HERMES_REGISTRY_AUTH_HOST:-}"
REGISTRY_AUTH_USERNAME="${HERMES_REGISTRY_USERNAME:-}"
REGISTRY_AUTH_PASSWORD="${HERMES_REGISTRY_PASSWORD:-}"

docker cp "${ROOT}/bin/hermes-daemon" "${NODE}:/usr/local/bin/hermes-daemon"

docker exec \
  -e HERMES_ENDPOINT="${HERMES_ENDPOINT}" \
  -e ENABLE_SOCI_CRI="${ENABLE_SOCI_CRI}" \
  -e RESTART_CONTAINERD="${RESTART_CONTAINERD}" \
  -e REGISTRY_AUTH_HOST="${REGISTRY_AUTH_HOST}" \
  -e REGISTRY_AUTH_USERNAME="${REGISTRY_AUTH_USERNAME}" \
  -e REGISTRY_AUTH_PASSWORD="${REGISTRY_AUTH_PASSWORD}" \
  "${NODE}" bash -lc '
set -euo pipefail

mkdir -p /etc/hermes-daemon /run/hermes-daemon /var/lib/hermes-daemon

if [[ -n "${REGISTRY_AUTH_HOST}" && -n "${REGISTRY_AUTH_USERNAME}" && -n "${REGISTRY_AUTH_PASSWORD}" ]]; then
  mkdir -p /root/.docker
  auth="$(printf "%s:%s" "${REGISTRY_AUTH_USERNAME}" "${REGISTRY_AUTH_PASSWORD}" | base64 | tr -d "\n")"
  cat >/root/.docker/config.json <<DOCKER_AUTH_CONFIG
{"auths":{"${REGISTRY_AUTH_HOST}":{"username":"${REGISTRY_AUTH_USERNAME}","password":"${REGISTRY_AUTH_PASSWORD}","auth":"${auth}"}}}
DOCKER_AUTH_CONFIG
  chmod 0600 /root/.docker/config.json
fi

cat >/etc/hermes-daemon/config.toml <<SOCI_CONFIG
[pull_modes]
  [pull_modes.soci_v1]
    enable = true

[external_artifact_store]
  enable = true
  endpoint = "${HERMES_ENDPOINT}"
  timeout_sec = 5
  platform = "linux/amd64"
  fallback_to_registry = true
SOCI_CONFIG

cat >/etc/systemd/system/hermes-daemon.service <<SERVICE_UNIT
[Unit]
Description=Hermes daemon containerd snapshotter plugin
After=network.target
Before=containerd.service

[Service]
Type=notify
Environment=HOME=/root
Environment=DOCKER_CONFIG=/root/.docker
ExecStart=/usr/local/bin/hermes-daemon
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
SERVICE_UNIT

if ! grep -q "^\[proxy_plugins\.soci\]" /etc/containerd/config.toml; then
  cp /etc/containerd/config.toml "/etc/containerd/config.toml.pre-hermes-$(date +%Y%m%d%H%M%S)"
  awk "
    BEGIN { inserted = 0 }
    /^\\[proxy_plugins\\]/ {
      print
      if (!inserted) {
        print \"[proxy_plugins.soci]\"
        print \"  type = \\\"snapshot\\\"\"
        print \"  address = \\\"/run/hermes-daemon/hermes-daemon.sock\\\"\"
        print \"  [proxy_plugins.soci.exports]\"
        print \"    root = \\\"/var/lib/hermes-daemon\\\"\"
        print \"    address = \\\"/run/hermes-daemon/hermes-daemon.sock\\\"\"
        print \"    enable_remote_snapshot_annotations = \\\"true\\\"\"
        print \"\"
        inserted = 1
        next
      }
    }
    { print }
  " /etc/containerd/config.toml > /etc/containerd/config.toml.hermes
  mv /etc/containerd/config.toml.hermes /etc/containerd/config.toml
fi

if ! grep -q "enable_remote_snapshot_annotations" /etc/containerd/config.toml; then
  cp /etc/containerd/config.toml "/etc/containerd/config.toml.pre-hermes-exports-$(date +%Y%m%d%H%M%S)"
  awk "
    BEGIN { in_soci = 0; inserted = 0 }
    /^\\[proxy_plugins\\.soci\\]/ {
      in_soci = 1
      print
      next
    }
    in_soci && /^\\[/ {
      print \"  [proxy_plugins.soci.exports]\"
      print \"    root = \\\"/var/lib/hermes-daemon\\\"\"
      print \"    address = \\\"/run/hermes-daemon/hermes-daemon.sock\\\"\"
      print \"    enable_remote_snapshot_annotations = \\\"true\\\"\"
      print \"\"
      inserted = 1
      in_soci = 0
    }
    { print }
    END {
      if (in_soci && !inserted) {
        print \"  [proxy_plugins.soci.exports]\"
        print \"    root = \\\"/var/lib/hermes-daemon\\\"\"
        print \"    address = \\\"/run/hermes-daemon/hermes-daemon.sock\\\"\"
        print \"    enable_remote_snapshot_annotations = \\\"true\\\"\"
      }
    }
  " /etc/containerd/config.toml > /etc/containerd/config.toml.hermes
  mv /etc/containerd/config.toml.hermes /etc/containerd/config.toml
fi

if [[ "${ENABLE_SOCI_CRI}" == "true" ]]; then
  sed -i "s/snapshotter = \"overlayfs\"/snapshotter = \"soci\"/" /etc/containerd/config.toml
  sed -i "s/^version = 2/version = 3/" /etc/containerd/config.toml
  if ! grep -q "^\[plugins\.\"io\.containerd\.cri\.v1\.images\"\]" /etc/containerd/config.toml; then
    cat >>/etc/containerd/config.toml <<CRI_SOCI_CONFIG

[plugins."io.containerd.cri.v1.images"]
  snapshotter = "soci"

[plugins."io.containerd.transfer.v1.local"]
  [[plugins."io.containerd.transfer.v1.local".unpack_config]]
    platform = "linux"
    snapshotter = "soci"
CRI_SOCI_CONFIG
  fi
fi

systemctl daemon-reload
systemctl enable --now hermes-daemon.service
systemctl restart hermes-daemon.service
if [[ "${RESTART_CONTAINERD}" == "true" ]]; then
  systemctl restart containerd
fi

systemctl is-active hermes-daemon.service
ctr plugin ls | grep -E "(^TYPE|soci)"
'
