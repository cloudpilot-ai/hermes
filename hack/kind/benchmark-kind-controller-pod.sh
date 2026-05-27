#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

IMAGE="${HERMES_BENCH_IMAGE:-public.ecr.aws/docker/library/golang:1.25-bookworm}"
PLATFORM="${HERMES_PLATFORM:-linux/amd64}"
OVERLAY_CLUSTER="${HERMES_OVERLAY_CLUSTER:-hermes-bench-overlay}"
HERMES_CLUSTER="${HERMES_CLUSTER:-hermes-bench-hermes}"
CONTROLLER_IMAGE="${HERMES_CONTROLLER_IMAGE:-}"
CONTROLLER_KO_REPO="${HERMES_CONTROLLER_KO_REPO:-ko.local}"
POD_COMMAND="${HERMES_BENCH_COMMAND:-sleep 300}"
POD_TIMEOUT="${HERMES_BENCH_TIMEOUT:-900s}"
RECREATE_CLUSTERS="${HERMES_RECREATE_CLUSTERS:-true}"
KEEP_OVERLAY_CLUSTER="${HERMES_KEEP_OVERLAY_CLUSTER:-false}"
KEEP_HERMES_CLUSTER="${HERMES_KEEP_HERMES_CLUSTER:-true}"
SKIP_OVERLAY_BASELINE="${HERMES_SKIP_OVERLAY_BASELINE:-false}"
RESULT_DIR="${HERMES_BENCH_RESULT_DIR:-${ROOT}/out/benchmarks}"
KIND_WAIT="${HERMES_KIND_WAIT:-5m}"
TUNE_KIND_HOST="${HERMES_TUNE_KIND_HOST:-true}"
REGISTRY_AUTH_HOST="${HERMES_REGISTRY_AUTH_HOST:-}"
REGISTRY_AUTH_USERNAME="${HERMES_REGISTRY_USERNAME:-}"
REGISTRY_AUTH_PASSWORD="${HERMES_REGISTRY_PASSWORD:-}"
IMAGE_PULL_SECRET="hermes-bench-registry"
CONTROLLER_NAMESPACE="${HERMES_CONTROLLER_NAMESPACE:-hermes-system}"
CONTROLLER_SERVICE_NAME="${HERMES_CONTROLLER_SERVICE_NAME:-hermes-controller}"
CONTROLLER_SERVICE_PORT="${HERMES_CONTROLLER_SERVICE_PORT:-39091}"
CONTROLLER_NODE_PORT="${HERMES_CONTROLLER_NODE_PORT:-30080}"
ENDPOINT_MODE="${HERMES_ENDPOINT_MODE:-nodeport-localhost}"
POLICY_NAME="${HERMES_POLICY_NAME:-hermes-bench-policy}"
POLICY_IMAGE_REGEX="${HERMES_POLICY_IMAGE_REGEX:-.*${IMAGE##*/}.*}"
VERIFY_CONTROLLER_IMAGE_CLEANUP="${HERMES_VERIFY_CONTROLLER_IMAGE_CLEANUP:-true}"
RUN_UNMATCHED_TEST="${HERMES_RUN_UNMATCHED_TEST:-false}"
UNMATCHED_IMAGE="${HERMES_UNMATCHED_IMAGE:-public.ecr.aws/docker/library/busybox:1.36}"
UNMATCHED_COMMAND="${HERMES_UNMATCHED_COMMAND:-sleep 60}"

mkdir -p "${RESULT_DIR}"
RESULT_FILE="${RESULT_DIR}/kind-controller-pod-$(date +%Y%m%d%H%M%S).txt"

log() {
  printf '%s\n' "$*" | tee -a "${RESULT_FILE}"
}

now_ms() {
  python3 - <<'PY'
import time
print(int(time.time() * 1000))
PY
}

need_binary() {
  if [[ ! -x "${ROOT}/bin/$1" ]]; then
    echo "missing ${ROOT}/bin/$1; copy the CI-built linux/amd64 artifact into bin/ before running the kind benchmark" >&2
    exit 1
  fi
}

registry_auth_enabled() {
  [[ -n "${REGISTRY_AUTH_HOST}" && -n "${REGISTRY_AUTH_USERNAME}" && -n "${REGISTRY_AUTH_PASSWORD}" ]]
}

prepare_registry_auth() {
  if [[ -z "${REGISTRY_AUTH_HOST}" && "${IMAGE}" == *".dkr.ecr."*".amazonaws.com/"* ]]; then
    REGISTRY_AUTH_HOST="${IMAGE%%/*}"
  fi
  if [[ -z "${REGISTRY_AUTH_USERNAME}" && -n "${REGISTRY_AUTH_HOST}" ]]; then
    REGISTRY_AUTH_USERNAME="AWS"
  fi
  if [[ -z "${REGISTRY_AUTH_PASSWORD}" && "${REGISTRY_AUTH_HOST}" =~ \.dkr\.ecr\.([a-z0-9-]+)\.amazonaws\.com$ ]]; then
    if ! command -v aws >/dev/null 2>&1; then
      echo "missing aws cli; set HERMES_REGISTRY_PASSWORD or install aws cli for ECR auth" >&2
      exit 1
    fi
    REGISTRY_AUTH_PASSWORD="$(aws ecr get-login-password --region "${BASH_REMATCH[1]}")"
  fi
  if registry_auth_enabled; then
    log "REGISTRY_AUTH_HOST=${REGISTRY_AUTH_HOST}"
  fi
}

ensure_pod_pull_secret() {
  local cluster="$1"
  local namespace="${2:-default}"
  if ! registry_auth_enabled; then
    return
  fi
  kubectl --context "kind-${cluster}" -n "${namespace}" create secret docker-registry "${IMAGE_PULL_SECRET}" \
    --docker-server="${REGISTRY_AUTH_HOST}" \
    --docker-username="${REGISTRY_AUTH_USERNAME}" \
    --docker-password="${REGISTRY_AUTH_PASSWORD}" \
    --dry-run=client -o yaml | kubectl --context "kind-${cluster}" apply -f - >/dev/null
}

tune_kind_host() {
  if [[ "${TUNE_KIND_HOST}" != "true" ]]; then
    return
  fi
  if [[ "$(uname -s)" != "Linux" ]]; then
    return
  fi

  if command -v sysctl >/dev/null 2>&1; then
    sysctl -w fs.inotify.max_user_instances=8192 >/dev/null 2>&1 || true
    sysctl -w fs.inotify.max_user_watches=1048576 >/dev/null 2>&1 || true
    sysctl -w fs.inotify.max_queued_events=32768 >/dev/null 2>&1 || true
  fi
}

create_cluster() {
  local cluster="$1"
  if kind get clusters | grep -qx "${cluster}"; then
    if [[ "${RECREATE_CLUSTERS}" == "true" ]]; then
      kind delete cluster --name "${cluster}"
    else
      return 0
    fi
  fi

  cat <<YAML | kind create cluster --name "${cluster}" --wait "${KIND_WAIT}" --config -
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
YAML

  kubectl --context "kind-${cluster}" wait --for=condition=Ready nodes --all --timeout=180s
}

delete_cluster_if_needed() {
  local cluster="$1"
  local keep="$2"
  if [[ "${keep}" != "true" ]]; then
    kind delete cluster --name "${cluster}" >/dev/null 2>&1 || true
  fi
}

worker_name() {
  local cluster="$1"
  echo "${cluster}-worker"
}

control_plane_name() {
  local cluster="$1"
  echo "${cluster}-control-plane"
}

soft_clear_image_on_node() {
  local node="$1"
  local image="$2"
  docker exec "${node}" crictl rmi "${image}" >/dev/null 2>&1 || true
  docker exec "${node}" ctr -n k8s.io images rm "${image}" >/dev/null 2>&1 || true
  docker exec "${node}" ctr -n k8s.io content prune >/dev/null 2>&1 || true
}

run_pod_once_with_image() {
  local cluster="$1"
  local node="$2"
  local pod="$3"
  local mode="$4"
  local image="$5"
  local command="$6"
  local start_ms end_ms elapsed_ms image_pull_secrets_yaml

  image_pull_secrets_yaml=""
  if registry_auth_enabled; then
    image_pull_secrets_yaml="  imagePullSecrets:
    - name: ${IMAGE_PULL_SECRET}"
  fi

  kubectl --context "kind-${cluster}" delete pod "${pod}" --ignore-not-found --wait=true >/dev/null 2>&1 || true
  soft_clear_image_on_node "${node}" "${image}"

  start_ms="$(now_ms)"
  cat <<YAML | kubectl --context "kind-${cluster}" apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: ${pod}
  labels:
    app: hermes-benchmark
    mode: ${mode}
spec:
  restartPolicy: Never
  nodeName: ${node}
${image_pull_secrets_yaml}
  containers:
    - name: app
      image: ${image}
      command:
        - sh
        - -lc
        - ${command@Q}
YAML
  kubectl --context "kind-${cluster}" wait --for=condition=Ready "pod/${pod}" --timeout="${POD_TIMEOUT}" | tee -a "${RESULT_FILE}"
  end_ms="$(now_ms)"
  elapsed_ms=$((end_ms - start_ms))

  kubectl --context "kind-${cluster}" get pod "${pod}" -o wide | tee -a "${RESULT_FILE}"
  log "${mode}_POD_READY_MS=${elapsed_ms}"
}

run_pod_once() {
  run_pod_once_with_image "$1" "$2" "$3" "$4" "${IMAGE}" "${POD_COMMAND}"
}

build_controller_image() {
  if [[ -n "${CONTROLLER_IMAGE}" ]]; then
    return
  fi
  if ! command -v ko >/dev/null 2>&1; then
    echo "missing ko; install ko or set HERMES_CONTROLLER_IMAGE to a prebuilt controller image" >&2
    exit 1
  fi

  CONTROLLER_IMAGE="$(
    cd "${ROOT}"
    KO_DOCKER_REPO="${CONTROLLER_KO_REPO}" ko build --platform="${PLATFORM}" --tag-only ./cmd/controller
  )"
}

deploy_controller() {
  local cluster="$1"
  local control_node service_type_yaml service_node_port_yaml
  control_node="$(control_plane_name "${cluster}")"
  service_type_yaml=""
  service_node_port_yaml=""
  if [[ "${ENDPOINT_MODE}" == "nodeport" || "${ENDPOINT_MODE}" == "nodeport-localhost" ]]; then
    service_type_yaml="  type: NodePort"
    service_node_port_yaml="      nodePort: ${CONTROLLER_NODE_PORT}"
  fi

  kind load docker-image --name "${cluster}" "${CONTROLLER_IMAGE}" >/dev/null

  kubectl --context "kind-${cluster}" apply -f "${ROOT}/deploy/hermespolicy-crd.yaml" >/dev/null
  kubectl --context "kind-${cluster}" wait --for=condition=Established crd/hermespolicies.hermes.cloudpilot.ai --timeout=120s | tee -a "${RESULT_FILE}"

  cat <<YAML | kubectl --context "kind-${cluster}" apply -f - >/dev/null
apiVersion: v1
kind: Namespace
metadata:
  name: ${CONTROLLER_NAMESPACE}
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: hermes-controller
  namespace: ${CONTROLLER_NAMESPACE}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: hermes-controller
rules:
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list", "watch"]
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["get"]
  - apiGroups: ["hermes.cloudpilot.ai"]
    resources: ["hermespolicies"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["hermes.cloudpilot.ai"]
    resources: ["hermespolicies/status"]
    verbs: ["get", "update", "patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: hermes-controller
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: hermes-controller
subjects:
  - kind: ServiceAccount
    name: hermes-controller
    namespace: ${CONTROLLER_NAMESPACE}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: hermes-controller
  namespace: ${CONTROLLER_NAMESPACE}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: hermes-controller
  template:
    metadata:
      labels:
        app: hermes-controller
    spec:
      nodeSelector:
        kubernetes.io/hostname: ${control_node}
      serviceAccountName: hermes-controller
      tolerations:
        - key: node-role.kubernetes.io/control-plane
          operator: Exists
          effect: NoSchedule
      containers:
        - name: controller
          image: ${CONTROLLER_IMAGE}
          imagePullPolicy: IfNotPresent
          securityContext:
            runAsUser: 0
          args:
            - --watch-kubernetes=true
            - --listen=:39091
            - --db=/data/hermes/hermes-cache.db
            - --platform=${PLATFORM}
            - --max-concurrency=1
            - --containerd-address=/run/containerd/containerd.sock
            - --containerd-namespace=k8s.io
            - --hermes-root=/var/lib/hermes
          ports:
            - name: http
              containerPort: 39091
          volumeMounts:
            - name: data
              mountPath: /data/hermes
            - name: hermes-root
              mountPath: /var/lib/hermes
            - name: containerd
              mountPath: /run/containerd/containerd.sock
      volumes:
        - name: data
          hostPath:
            path: /var/lib/hermes-controller/data
            type: DirectoryOrCreate
        - name: hermes-root
          hostPath:
            path: /var/lib/hermes-controller/hermes
            type: DirectoryOrCreate
        - name: containerd
          hostPath:
            path: /run/containerd/containerd.sock
            type: Socket
---
apiVersion: v1
kind: Service
metadata:
  name: ${CONTROLLER_SERVICE_NAME}
  namespace: ${CONTROLLER_NAMESPACE}
spec:
${service_type_yaml}
  selector:
    app: hermes-controller
  ports:
    - name: http
      port: ${CONTROLLER_SERVICE_PORT}
      targetPort: 39091
${service_node_port_yaml}
YAML

  kubectl --context "kind-${cluster}" -n "${CONTROLLER_NAMESPACE}" rollout status deploy/hermes-controller --timeout=180s | tee -a "${RESULT_FILE}"
}

controller_endpoint_from_worker() {
  local cluster="$1"
  local svc_ip node_port
  case "${ENDPOINT_MODE}" in
    service | service-dns)
      echo "http://${CONTROLLER_SERVICE_NAME}.${CONTROLLER_NAMESPACE}.svc.cluster.local:${CONTROLLER_SERVICE_PORT}"
      ;;
    cluster-ip)
      svc_ip="$(kubectl --context "kind-${cluster}" -n "${CONTROLLER_NAMESPACE}" get svc "${CONTROLLER_SERVICE_NAME}" -o jsonpath='{.spec.clusterIP}')"
      echo "http://${svc_ip}:${CONTROLLER_SERVICE_PORT}"
      ;;
    nodeport | nodeport-localhost)
      node_port="$(kubectl --context "kind-${cluster}" -n "${CONTROLLER_NAMESPACE}" get svc "${CONTROLLER_SERVICE_NAME}" -o jsonpath='{.spec.ports[?(@.name=="http")].nodePort}')"
      printf '%s\n' "CONTROLLER_NODE_PORT=${node_port}" | tee -a "${RESULT_FILE}" >&2
      echo "http://127.0.0.1:${node_port}"
      ;;
    *)
      echo "unsupported HERMES_ENDPOINT_MODE=${ENDPOINT_MODE}; use service, cluster-ip, or nodeport-localhost" >&2
      exit 1
      ;;
  esac
}

configure_worker_service_dns() {
  local cluster="$1"
  local worker="$2"
  local dns_ip service_fqdn

  if [[ "${ENDPOINT_MODE}" != "service" && "${ENDPOINT_MODE}" != "service-dns" ]]; then
    return
  fi

  dns_ip="$(kubectl --context "kind-${cluster}" -n kube-system get svc kube-dns -o jsonpath='{.spec.clusterIP}')"
  service_fqdn="${CONTROLLER_SERVICE_NAME}.${CONTROLLER_NAMESPACE}.svc.cluster.local"

  docker exec \
    -e KUBE_DNS_IP="${dns_ip}" \
    "${worker}" bash -lc '
set -euo pipefail
cp /etc/resolv.conf "/etc/resolv.conf.pre-hermes-$(date +%Y%m%d%H%M%S)" || true
cat >/etc/resolv.conf <<DNS_CONFIG
nameserver ${KUBE_DNS_IP}
search svc.cluster.local cluster.local
options ndots:5
DNS_CONFIG
'

  if ! docker exec "${worker}" getent hosts "${service_fqdn}" >/dev/null; then
    log "service DNS did not resolve on worker: ${service_fqdn}"
    docker exec "${worker}" cat /etc/resolv.conf | tee -a "${RESULT_FILE}" || true
    exit 1
  fi
  log "SERVICE_DNS=${service_fqdn}"
}

wait_for_controller_from_worker() {
  local worker="$1"
  local endpoint="$2"
  for _ in $(seq 1 60); do
    if docker exec "${worker}" curl -fsS "${endpoint}/healthz" >/dev/null 2>&1; then
      return
    fi
    sleep 2
  done
  docker exec "${worker}" curl -v "${endpoint}/healthz" || true
  return 1
}

apply_hermes_policy() {
  local cluster="$1"
  cat <<YAML | kubectl --context "kind-${cluster}" apply -f - | tee -a "${RESULT_FILE}"
apiVersion: hermes.cloudpilot.ai/v1alpha1
kind: HermesPolicy
metadata:
  name: ${POLICY_NAME}
spec:
  paused: false
  imageSelectors:
    - imageRegex: ${POLICY_IMAGE_REGEX@Q}
  platforms:
    - ${PLATFORM}
YAML
}

create_policy_trigger_pod() {
  local cluster="$1"
  local pod="${2:-hermes-policy-trigger}"
  local image_pull_secrets_yaml

  image_pull_secrets_yaml=""
  if registry_auth_enabled; then
    image_pull_secrets_yaml="  imagePullSecrets:
    - name: ${IMAGE_PULL_SECRET}"
  fi

  kubectl --context "kind-${cluster}" delete pod "${pod}" --ignore-not-found --wait=true >/dev/null 2>&1 || true
  cat <<YAML | kubectl --context "kind-${cluster}" apply -f - | tee -a "${RESULT_FILE}"
apiVersion: v1
kind: Pod
metadata:
  name: ${pod}
  labels:
    app: hermes-policy-trigger
spec:
  restartPolicy: Never
  nodeSelector:
    hermes.cloudpilot.ai/policy-trigger-only: "true"
${image_pull_secrets_yaml}
  containers:
    - name: app
      image: ${IMAGE}
      command:
        - sh
        - -lc
        - "sleep 3600"
YAML
}

trigger_policy_build() {
  local cluster="$1"
  apply_hermes_policy "${cluster}"
  create_policy_trigger_pod "${cluster}" hermes-policy-trigger
}

verify_controller_source_image_cleaned() {
  local cluster="$1"
  local ready_ref="$2"
  local control_node image_names leftovers

  if [[ "${VERIFY_CONTROLLER_IMAGE_CLEANUP}" != "true" ]]; then
    log "CONTROLLER_SOURCE_IMAGE_CLEAN=skipped"
    return
  fi

  control_node="$(control_plane_name "${cluster}")"
  image_names="$(docker exec "${control_node}" ctr -n k8s.io images ls -q)"
  leftovers="$(
    IMAGE_NAMES="${image_names}" IMAGE="${IMAGE}" READY_REF="${ready_ref}" python3 - <<'PY'
import os

names = set(filter(None, os.environ.get("IMAGE_NAMES", "").splitlines()))
checks = [os.environ["IMAGE"], os.environ["READY_REF"]]
print("\n".join(name for name in checks if name in names))
PY
  )"

  if [[ -n "${leftovers}" ]]; then
    log "CONTROLLER_SOURCE_IMAGE_CLEAN=false"
    log "controller source image records still exist after build:"
    printf '%s\n' "${leftovers}" | tee -a "${RESULT_FILE}"
    kubectl --context "kind-${cluster}" -n "${CONTROLLER_NAMESPACE}" logs deploy/hermes-controller --tail=200 | tee -a "${RESULT_FILE}" || true
    exit 1
  fi

  log "CONTROLLER_SOURCE_IMAGE_CLEAN=true"
}

trigger_policy_build_and_wait() {
  local cluster="$1"
  local worker="$2"
  local endpoint="$3"
  local build_start build_end build_ms ready_ref index_digest ztoc_count ztoc_size

  build_start="$(now_ms)"
  trigger_policy_build "${cluster}"

  ready_ref=""
  for _ in $(seq 1 600); do
    artifacts="$(docker exec "${worker}" curl -fsS "${endpoint}/v1/artifacts" || true)"
    ready_ref="$(ARTIFACTS="${artifacts}" IMAGE="${IMAGE}" PLATFORM="${PLATFORM}" python3 - <<'PY'
import json, os
items = json.loads(os.environ.get("ARTIFACTS") or "[]")
for item in items:
    if item.get("sourceImageRef") == os.environ["IMAGE"] and item.get("platform") == os.environ["PLATFORM"] and item.get("status") == "Ready":
        print(item.get("imageDigestRef", ""))
        break
PY
)"
    if [[ -n "${ready_ref}" ]]; then
      break
    fi
    failed_msg="$(ARTIFACTS="${artifacts}" IMAGE="${IMAGE}" PLATFORM="${PLATFORM}" python3 - <<'PY'
import json, os
items = json.loads(os.environ.get("ARTIFACTS") or "[]")
for item in items:
    if item.get("sourceImageRef") == os.environ["IMAGE"] and item.get("platform") == os.environ["PLATFORM"] and item.get("status") == "Failed":
        print(item.get("error", "artifact failed"))
        break
PY
)"
    if [[ -n "${failed_msg}" ]]; then
      log "artifact failed: ${failed_msg}"
      kubectl --context "kind-${cluster}" -n "${CONTROLLER_NAMESPACE}" logs deploy/hermes-controller --tail=200 | tee -a "${RESULT_FILE}" || true
      exit 1
    fi
    sleep 2
  done

  if [[ -z "${ready_ref}" ]]; then
    log "artifact did not become Ready"
    kubectl --context "kind-${cluster}" -n "${CONTROLLER_NAMESPACE}" logs deploy/hermes-controller --tail=200 | tee -a "${RESULT_FILE}" || true
    exit 1
  fi

  build_end="$(now_ms)"
  build_ms=$((build_end - build_start))
  log "HERMES_READY_REF=${ready_ref}"

  docker exec "${worker}" curl -fsS "${endpoint}/v1/indexes/resolve?image=${ready_ref}&platform=${PLATFORM}" >"${RESULT_DIR}/resolve-${HERMES_CLUSTER}.json"
  read -r index_digest ztoc_count ztoc_size < <(python3 - <<PY
import json
with open("${RESULT_DIR}/resolve-${HERMES_CLUSTER}.json") as f:
    d = json.load(f)
print(d["sociIndex"]["digest"], len(d.get("ztocs") or []), sum(x.get("size", 0) for x in d.get("ztocs") or []))
PY
)
  log "SOCI_INDEX_DIGEST=${index_digest}"
  log "SOCI_ZTOC_COUNT=${ztoc_count}"
  log "SOCI_ZTOC_TOTAL_SIZE=${ztoc_size}"
  log "HERMES_ARTIFACT_READY_MS=${build_ms}"

  log "=== HermesPolicy status ==="
  kubectl --context "kind-${cluster}" get hermespolicy "${POLICY_NAME}" -o yaml | tee -a "${RESULT_FILE}" || true

  verify_controller_source_image_cleaned "${cluster}" "${ready_ref}"
}

install_daemon_on_worker() {
  local worker="$1"
  local endpoint="$2"
  KIND_NODE="${worker}" \
    HERMES_ENDPOINT="${endpoint}" \
    ENABLE_SOCI_CRI=true \
    RESTART_CONTAINERD=true \
    HERMES_REGISTRY_AUTH_HOST="${REGISTRY_AUTH_HOST}" \
    HERMES_REGISTRY_USERNAME="${REGISTRY_AUTH_USERNAME}" \
    HERMES_REGISTRY_PASSWORD="${REGISTRY_AUTH_PASSWORD}" \
    "${ROOT}/hack/kind/install-daemon-in-kind-node.sh" | tee -a "${RESULT_FILE}"
  sleep 5
  kubectl --context "kind-${HERMES_CLUSTER}" wait --for=condition=Ready "node/${worker}" --timeout=180s | tee -a "${RESULT_FILE}"
}

capture_hermes_evidence() {
  local cluster="$1"
  local worker="$2"
  log "=== hermes daemon evidence ==="
  docker exec "${worker}" journalctl -u hermes-daemon.service --no-pager \
    | grep -E 'fetching index from Hermes artifact store|remote snapshot successfully prepared|fetching artifact from remote|fuse operation count' \
    | tail -n 120 | tee -a "${RESULT_FILE}" || true

  log "=== controller logs ==="
  kubectl --context "kind-${cluster}" -n "${CONTROLLER_NAMESPACE}" logs deploy/hermes-controller --tail=120 | tee -a "${RESULT_FILE}" || true
}

run_unmatched_no_build_test() {
  local cluster="$1"
  local worker="$2"
  local endpoint="$3"
  local artifacts matches

  log "=== unmatched policy fallback test ==="
  log "UNMATCHED_IMAGE=${UNMATCHED_IMAGE}"
  run_pod_once_with_image "${cluster}" "${worker}" hermes-unmatched-fallback hermes_unmatched "${UNMATCHED_IMAGE}" "${UNMATCHED_COMMAND}"

  artifacts="$(docker exec "${worker}" curl -fsS "${endpoint}/v1/artifacts" || true)"
  matches="$(ARTIFACTS="${artifacts}" UNMATCHED_IMAGE="${UNMATCHED_IMAGE}" python3 - <<'PY'
import json, os
items = json.loads(os.environ.get("ARTIFACTS") or "[]")
matches = [item for item in items if item.get("sourceImageRef") == os.environ["UNMATCHED_IMAGE"]]
print(len(matches))
PY
)"
  log "UNMATCHED_ARTIFACT_COUNT=${matches}"
  if [[ "${matches}" != "0" ]]; then
    log "unmatched image unexpectedly produced Hermes artifact"
    kubectl --context "kind-${cluster}" -n "${CONTROLLER_NAMESPACE}" logs deploy/hermes-controller --tail=200 | tee -a "${RESULT_FILE}" || true
    exit 1
  fi
}

main() {
  cd "${ROOT}"
  need_binary hermes-daemon

  log "=== Hermes independent controller benchmark ==="
  log "IMAGE=${IMAGE}"
  log "PLATFORM=${PLATFORM}"
  log "OVERLAY_CLUSTER=${OVERLAY_CLUSTER}"
  log "HERMES_CLUSTER=${HERMES_CLUSTER}"
  log "RESULT_FILE=${RESULT_FILE}"
  log "ENDPOINT_MODE=${ENDPOINT_MODE}"
  log "POLICY_NAME=${POLICY_NAME}"
  log "POLICY_IMAGE_REGEX=${POLICY_IMAGE_REGEX}"
  log "VERIFY_CONTROLLER_IMAGE_CLEANUP=${VERIFY_CONTROLLER_IMAGE_CLEANUP}"
  if [[ "${ENDPOINT_MODE}" == "nodeport" || "${ENDPOINT_MODE}" == "nodeport-localhost" ]]; then
    log "CONTROLLER_NODE_PORT=${CONTROLLER_NODE_PORT}"
  fi

  tune_kind_host
  prepare_registry_auth

  log "=== build controller image ==="
  build_controller_image
  log "CONTROLLER_IMAGE=${CONTROLLER_IMAGE}"

  if [[ "${SKIP_OVERLAY_BASELINE}" != "true" ]]; then
    log "=== overlayfs baseline ==="
    create_cluster "${OVERLAY_CLUSTER}"
    ensure_pod_pull_secret "${OVERLAY_CLUSTER}"
    run_pod_once "${OVERLAY_CLUSTER}" "$(worker_name "${OVERLAY_CLUSTER}")" hermes-overlay-baseline overlay
    delete_cluster_if_needed "${OVERLAY_CLUSTER}" "${KEEP_OVERLAY_CLUSTER}"
  else
    log "=== overlayfs baseline skipped ==="
  fi

  log "=== hermes lazy loading ==="
  create_cluster "${HERMES_CLUSTER}"
  ensure_pod_pull_secret "${HERMES_CLUSTER}"
  deploy_controller "${HERMES_CLUSTER}"
  worker="$(worker_name "${HERMES_CLUSTER}")"
  configure_worker_service_dns "${HERMES_CLUSTER}" "${worker}"
  endpoint="$(controller_endpoint_from_worker "${HERMES_CLUSTER}")"
  log "HERMES_ENDPOINT=${endpoint}"
  wait_for_controller_from_worker "${worker}" "${endpoint}"
  trigger_policy_build_and_wait "${HERMES_CLUSTER}" "${worker}" "${endpoint}"
  install_daemon_on_worker "${worker}" "${endpoint}"
  run_pod_once "${HERMES_CLUSTER}" "${worker}" hermes-lazy hermes
  if [[ "${RUN_UNMATCHED_TEST}" == "true" ]]; then
    run_unmatched_no_build_test "${HERMES_CLUSTER}" "${worker}" "${endpoint}"
  fi
  capture_hermes_evidence "${HERMES_CLUSTER}" "${worker}"

  log "=== summary ==="
  summary="$(grep -E '^(overlay|hermes|hermes_unmatched)_POD_READY_MS=|^HERMES_ARTIFACT_READY_MS=|^HERMES_READY_REF=|^SOCI_INDEX_DIGEST=|^SOCI_ZTOC_COUNT=|^CONTROLLER_SOURCE_IMAGE_CLEAN=|^UNMATCHED_ARTIFACT_COUNT=' "${RESULT_FILE}" || true)"
  if [[ -n "${summary}" ]]; then
    printf '%s\n' "${summary}" | tee -a "${RESULT_FILE}"
  fi
  log "PASS"

  if [[ "${KEEP_HERMES_CLUSTER}" != "true" ]]; then
    kind delete cluster --name "${HERMES_CLUSTER}" >/dev/null 2>&1 || true
  fi
}

main "$@"
