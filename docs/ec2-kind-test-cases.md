# EC2 + kind Test Cases

This document defines the EC2 + kind test suite for Hermes. It is intended for
repeatable validation in the AWS lab environment before moving the same flows to
EKS.

The current benchmark driver is:

```bash
./hack/kind/benchmark-kind-controller-pod.sh
```

The script creates kind clusters, deploys `hermes-controller`, creates a
`HermesPolicy` plus an intentionally unschedulable trigger Pod, installs
`hermes-daemon` as a host systemd service in the kind worker node, and writes
evidence under `out/benchmarks/`.

The current AWS EC2 + kind acceptance path uses `nodeport-localhost`: the daemon
talks to `http://127.0.0.1:30080` from inside the kind worker container. The
Kubernetes Service is only the NodePort backing object; the current report path
does not use Service DNS or direct ClusterIP access.

All EC2 + kind build tests are policy-triggered. The controller watches Pods,
matches their images against `HermesPolicy`, and builds artifacts from the
observed Pod image and `imagePullSecrets`.

## Environment

Use an Amazon Linux 2023 EC2 instance in `us-east-2` with Docker, kind, kubectl,
AWS CLI, Go, zlib headers, and enough disk for large image pulls. The previous
vLLM validation used this image:

```text
763104351884.dkr.ecr.us-east-1.amazonaws.com/vllm:0.9-gpu-py312-ec2
```

For private ECR images, the EC2 identity must be able to call
`ecr:GetAuthorizationToken` for the registry account and region.

If using the disposable EC2 lab helper from the parent research workspace, start
the host before copying or building Hermes:

```bash
AWS_REGION=us-east-2 \
INSTANCE_TYPE=m6i.2xlarge \
VOLUME_SIZE_GB=200 \
../launch-kind-ec2.sh
```

That helper creates an SSM-managed AL2023 EC2 instance and a starter kind
cluster. The Hermes benchmark script creates its own per-test kind clusters on
the same host.

Required local build inputs on the EC2 host:

```bash
sudo dnf install -y docker git jq tar gzip unzip conntrack-tools iptables socat ethtool fuse3 pigz gcc make zlib-devel

CGO_ENABLED=1 go build -o bin/hermes-controller ./cmd/controller
CGO_ENABLED=1 go build -o bin/hermes-daemon ./cmd/daemon
```

Build the controller image with `ko`, or provide a prebuilt local image through
`HERMES_CONTROLLER_IMAGE`. The benchmark script loads that image into the kind
cluster.

## Common Evidence

Each test should preserve:

- `out/benchmarks/kind-controller-pod-*.txt`
- `out/benchmarks/resolve-<cluster>.json`
- `kubectl -n hermes-system logs deploy/hermes-controller`
- `journalctl -u hermes-daemon.service` from the kind worker container
- `kubectl get pods -A -o wide`
- `kubectl get svc -n hermes-system hermes-controller -o wide` to verify the
  NodePort backing object
- `kubectl get hermespolicy -o yaml` for build cases
- `ctr -n k8s.io images ls -q` on the controller node for source image cleanup
  checks

Common pass markers:

```text
HERMES_READY_REF=<image@digest>
SOCI_INDEX_DIGEST=sha256:...
SOCI_ZTOC_COUNT=<non-zero for large images>
HERMES_ARTIFACT_READY_MS=<milliseconds>
CONTROLLER_SOURCE_IMAGE_CLEAN=true
hermes_POD_READY_MS=<milliseconds>
fetching index from Hermes artifact store
remote snapshot successfully prepared.
fetching artifact from remote
PASS
```

## Test Matrix

| ID | Area | Trigger | Image | Expected result |
| --- | --- | --- | --- | --- |
| TC-00 | EC2 host readiness | N/A | N/A | EC2 host can run Docker, kind, kubectl, and build Hermes binaries. |
| TC-01 | Public-image smoke | HermesPolicy | Public ECR Golang | Controller observes a matching trigger Pod, builds an index, and daemon lazy-mounts the image. |
| TC-02 | Large-image benchmark | HermesPolicy | vLLM ECR image | Overlay baseline and Hermes path both run; Hermes Pod Ready is materially lower after artifact readiness. |
| TC-03 | NodePort localhost endpoint | HermesPolicy | Any supported image | Worker daemon reaches controller through `127.0.0.1:<nodePort>`. |
| TC-04 | Private ECR auth from Pod | HermesPolicy | Private ECR image | Controller reads the Pod `imagePullSecrets` and pulls the image without controller-level registry env vars. |
| TC-05 | Bad or missing pull secret | HermesPolicy | Private ECR image | Build fails with an auth error and does not create a Ready artifact. |
| TC-06 | Policy selector negative | HermesPolicy | Unmatched public image | Workload falls back to the registry and no Hermes artifact is created. |
| TC-07 | Cache reuse | HermesPolicy | Same image digest | Second run skips or quickly reuses the cached Ready artifact. |
| TC-08 | RBAC guard | HermesPolicy | Private ECR image | Controller can read pull secrets for Pod auth and does not require global registry credentials. |
| TC-09 | Controller image cleanup | HermesPolicy | Public or private image | Source image pulled by the controller is removed from controller-node containerd after artifact build. |

## TC-00 EC2 Host Readiness

Purpose: verify the EC2 lab host is suitable for kind, containerd socket mounts,
CGO builds, and large image pulls.

Commands:

```bash
docker version
kind version
kubectl version --client
aws sts get-caller-identity
go version
gcc --version
ldconfig -p | grep zlib || true
```

Pass criteria:

- Docker is running.
- kind and kubectl are installed.
- AWS caller identity is the expected lab account.
- `go build` for `bin/hermes-controller` and `bin/hermes-daemon` succeeds.

## TC-01 Public-Image Smoke

Purpose: validate the basic controller build path and daemon lazy mount path
without registry credentials.

Command:

```bash
HERMES_CLUSTER=hermes-smoke-public \
HERMES_OVERLAY_CLUSTER=hermes-smoke-overlay \
HERMES_BENCH_IMAGE=public.ecr.aws/docker/library/golang:1.25-bookworm \
HERMES_POLICY_IMAGE_REGEX='.*golang.*' \
HERMES_ENDPOINT_MODE=nodeport-localhost \
HERMES_CONTROLLER_NODE_PORT=30080 \
HERMES_SKIP_OVERLAY_BASELINE=true \
HERMES_KEEP_HERMES_CLUSTER=true \
./hack/kind/benchmark-kind-controller-pod.sh
```

Pass criteria:

- The result file ends with `PASS`.
- `/v1/artifacts` contains one Ready artifact for the Golang image.
- The daemon journal contains `fetching index from Hermes artifact store`.

## TC-02 Large-Image Benchmark

Purpose: measure the difference between ordinary overlayfs cold start and Hermes
lazy loading for the vLLM image used in the AWS lab.

Command:

```bash
HERMES_CLUSTER=hermes-vllm-nodeport \
HERMES_OVERLAY_CLUSTER=hermes-vllm-overlay \
HERMES_BENCH_IMAGE=763104351884.dkr.ecr.us-east-1.amazonaws.com/vllm:0.9-gpu-py312-ec2 \
HERMES_POLICY_IMAGE_REGEX='.*vllm.*' \
HERMES_ENDPOINT_MODE=nodeport-localhost \
HERMES_CONTROLLER_NODE_PORT=30080 \
HERMES_SKIP_OVERLAY_BASELINE=false \
HERMES_KEEP_HERMES_CLUSTER=true \
HERMES_KEEP_OVERLAY_CLUSTER=false \
HERMES_BENCH_TIMEOUT=1200s \
./hack/kind/benchmark-kind-controller-pod.sh
```

Pass criteria:

- `overlay_POD_READY_MS` and `hermes_POD_READY_MS` are both recorded.
- `HERMES_ARTIFACT_READY_MS` is recorded separately from Pod Ready time.
- `SOCI_ZTOC_COUNT` is greater than zero.
- `hermes_POD_READY_MS` is materially lower than `overlay_POD_READY_MS` after
  the artifact is Ready.

## TC-03 NodePort Localhost Endpoint

Purpose: validate the current node-local endpoint mode for kind and EKS-like
node daemons. This is the primary AWS EC2 + kind path.

Command:

```bash
HERMES_CLUSTER=hermes-nodeport \
HERMES_BENCH_IMAGE=public.ecr.aws/docker/library/golang:1.25-bookworm \
HERMES_POLICY_IMAGE_REGEX='.*golang.*' \
HERMES_ENDPOINT_MODE=nodeport-localhost \
HERMES_CONTROLLER_NODE_PORT=30080 \
HERMES_SKIP_OVERLAY_BASELINE=true \
./hack/kind/benchmark-kind-controller-pod.sh
```

Pass criteria:

- The result file contains `CONTROLLER_NODE_PORT=30080`.
- The result file contains `HERMES_ENDPOINT=http://127.0.0.1:30080`.
- The daemon reaches `/healthz` through localhost NodePort and lazy-mounts the
  test Pod.

## TC-04 Private ECR Auth From Pod imagePullSecrets

Purpose: validate the controller authentication model. Hermes should not require
controller-level registry username/password flags or environment variables. For
Pod-observed builds, it should read the Pod's `imagePullSecrets` and reuse those
credentials for the controller-side image pull.

This case must go through the Pod watcher because the controller gets registry
credentials from the observed Pod's `imagePullSecrets`.

Command:

```bash
export ECR_REGION=us-east-1
export HERMES_REGISTRY_AUTH_HOST=763104351884.dkr.ecr.us-east-1.amazonaws.com
export HERMES_REGISTRY_USERNAME=AWS
export HERMES_REGISTRY_PASSWORD="$(aws ecr get-login-password --region "${ECR_REGION}")"

HERMES_CLUSTER=hermes-private-ecr-policy \
HERMES_BENCH_IMAGE=763104351884.dkr.ecr.us-east-1.amazonaws.com/vllm:0.9-gpu-py312-ec2 \
HERMES_POLICY_NAME=vllm-policy \
HERMES_POLICY_IMAGE_REGEX='.*vllm.*' \
HERMES_ENDPOINT_MODE=nodeport-localhost \
HERMES_SKIP_OVERLAY_BASELINE=true \
HERMES_KEEP_HERMES_CLUSTER=true \
HERMES_BENCH_TIMEOUT=1200s \
./hack/kind/benchmark-kind-controller-pod.sh
```

Expected behavior:

- The script creates a Docker registry Secret in the workload namespace.
- The policy trigger Pod references that Secret with `imagePullSecrets`.
- The controller watches the Pod, reads the referenced Secret, and pulls the
  private image using those credentials.
- No `HERMES_CONTROLLER_REGISTRY_*` environment variables are present on the
  controller Deployment.

Pass criteria:

- `kubectl get hermespolicy vllm-policy -o yaml` shows `phase: Ready`.
- The controller log shows `pulling image=... through containerd API` followed by
  `pulled image=...`.
- `/v1/artifacts` has a Ready artifact for the private ECR image.
- The daemon successfully starts the `hermes-lazy` Pod.

## TC-05 Bad Or Missing Pull Secret

Purpose: verify that private registry access fails closed when the observed Pod
does not provide valid credentials.

Suggested command with an intentionally bad password:

```bash
export HERMES_REGISTRY_AUTH_HOST=763104351884.dkr.ecr.us-east-1.amazonaws.com
export HERMES_REGISTRY_USERNAME=AWS
export HERMES_REGISTRY_PASSWORD=invalid-token

HERMES_CLUSTER=hermes-private-ecr-bad-secret \
HERMES_BENCH_IMAGE=763104351884.dkr.ecr.us-east-1.amazonaws.com/vllm:0.9-gpu-py312-ec2 \
HERMES_POLICY_IMAGE_REGEX='.*vllm.*' \
HERMES_ENDPOINT_MODE=nodeport-localhost \
HERMES_SKIP_OVERLAY_BASELINE=true \
HERMES_KEEP_HERMES_CLUSTER=true \
./hack/kind/benchmark-kind-controller-pod.sh
```

Expected behavior:

- The benchmark exits non-zero after the artifact enters `Failed`.
- The controller logs include an authentication or authorization error from the
  registry.
- No Ready artifact exists for the image.

Pass criteria:

- Failure is explicit and attributable to registry auth.
- The controller does not use fallback global credentials.

## TC-06 Policy Selector Negative

Purpose: verify that HermesPolicy only builds matching images, and unmatched
Pods continue through normal registry fallback.

Command:

```bash
HERMES_CLUSTER=hermes-policy-negative \
HERMES_BENCH_IMAGE=public.ecr.aws/docker/library/golang:1.25-bookworm \
HERMES_POLICY_IMAGE_REGEX='.*golang.*' \
HERMES_RUN_UNMATCHED_TEST=true \
HERMES_UNMATCHED_IMAGE=public.ecr.aws/docker/library/busybox:1.36 \
HERMES_ENDPOINT_MODE=nodeport-localhost \
HERMES_SKIP_OVERLAY_BASELINE=true \
./hack/kind/benchmark-kind-controller-pod.sh
```

Pass criteria:

- The matched image becomes Ready.
- `UNMATCHED_ARTIFACT_COUNT=0`.
- The unmatched Pod reaches Ready through registry fallback.

## TC-07 Cache Reuse

Purpose: verify that repeated builds for the same image digest reuse the
controller cache.

Commands:

```bash
HERMES_CLUSTER=hermes-cache-reuse \
HERMES_BENCH_IMAGE=public.ecr.aws/docker/library/golang:1.25-bookworm \
HERMES_POLICY_IMAGE_REGEX='.*golang.*' \
HERMES_ENDPOINT_MODE=nodeport-localhost \
HERMES_SKIP_OVERLAY_BASELINE=true \
HERMES_KEEP_HERMES_CLUSTER=true \
./hack/kind/benchmark-kind-controller-pod.sh

HERMES_CLUSTER=hermes-cache-reuse \
HERMES_BENCH_IMAGE=public.ecr.aws/docker/library/golang:1.25-bookworm \
HERMES_POLICY_IMAGE_REGEX='.*golang.*' \
HERMES_ENDPOINT_MODE=nodeport-localhost \
HERMES_SKIP_OVERLAY_BASELINE=true \
HERMES_RECREATE_CLUSTERS=false \
HERMES_KEEP_HERMES_CLUSTER=true \
./hack/kind/benchmark-kind-controller-pod.sh
```

Pass criteria:

- The second run reaches Ready without rebuilding all artifacts.
- Controller logs include a cached artifact path such as `skip cached index`.
- The image digest and index digest are stable across runs.

## TC-08 RBAC Guard

Purpose: verify that the controller has the Kubernetes permissions required for
Pod-driven auth and no longer needs controller-level registry credentials.

The current EC2 + kind MVP grants `get` on Secrets because the controller watches
Pods across namespaces. Production hardening should reduce this blast radius
with namespace scoping or a narrower credential delegation model.

Commands:

```bash
kubectl --context kind-hermes-private-ecr-policy auth can-i get secrets \
  --as system:serviceaccount:hermes-system:hermes-controller

kubectl --context kind-hermes-private-ecr-policy -n hermes-system get deploy hermes-controller -o yaml \
  | grep -E 'HERMES_CONTROLLER_REGISTRY|registry-auth|registry-username|registry-password' || true
```

Pass criteria:

- `kubectl auth can-i get secrets` returns `yes`.
- The grep command returns no controller registry env or flags.
- Private ECR policy-triggered build still succeeds.

## TC-09 Controller Source Image Cleanup

Purpose: verify that the controller does not keep the full source image in the
controller node's containerd image store after SOCI artifacts become Ready. The
artifact cache should keep the generated SOCI index and zTOCs, but the image
pulled only for building should be cleaned.

The benchmark script checks this automatically by default and fails if the
source image tag or the Ready digest reference still exists in
`ctr -n k8s.io images ls -q` on the kind control-plane node.

Command:

```bash
HERMES_CLUSTER=hermes-image-cleanup \
HERMES_BENCH_IMAGE=public.ecr.aws/docker/library/golang:1.25-bookworm \
HERMES_POLICY_IMAGE_REGEX='.*golang.*' \
HERMES_ENDPOINT_MODE=nodeport-localhost \
HERMES_SKIP_OVERLAY_BASELINE=true \
HERMES_KEEP_HERMES_CLUSTER=true \
HERMES_VERIFY_CONTROLLER_IMAGE_CLEANUP=true \
./hack/kind/benchmark-kind-controller-pod.sh
```

Manual inspection, if the cluster is kept:

```bash
RESULT_FILE=out/benchmarks/<result-file>.txt
READY_REF="$(grep '^HERMES_READY_REF=' "${RESULT_FILE}" | tail -1 | cut -d= -f2-)"

docker exec hermes-image-cleanup-control-plane ctr -n k8s.io images ls -q \
  | grep -Fx -e 'public.ecr.aws/docker/library/golang:1.25-bookworm' -e "${READY_REF}" || true

kubectl --context kind-hermes-image-cleanup -n hermes-system logs deploy/hermes-controller \
  | grep 'cleaned source image'
```

Pass criteria:

- The result file contains `CONTROLLER_SOURCE_IMAGE_CLEAN=true`.
- The controller log contains `cleaned source image ... reason=built` for a
  fresh build or `reason=cached` for a cache hit after a pull.
- `ctr -n k8s.io images ls -q` on the control-plane node does not contain the
  test source image tag or `HERMES_READY_REF`.
- The Hermes artifact remains resolvable through `/v1/indexes/resolve`.

## Cleanup

For per-test cleanup:

```bash
kind delete cluster --name <cluster-name>
```

If using the disposable EC2 lab helper from the parent research workspace:

```bash
../destroy-kind-ec2.sh
```

Keep at least the benchmark result file and controller/daemon logs before
destroying the EC2 host.
