# Kind Benchmark

This benchmark is a local smoke test for the Hermes controller and daemon path.
It creates a temporary kind cluster, creates a `HermesPolicy` plus a matching
trigger Pod so the controller builds a SOCI index, installs the daemon on the
worker node, and starts a Pod through the `soci` containerd snapshotter.

The benchmark is intentionally not an installation guide. It is useful for
checking that a development build can:

- build a SOCI v1 index in the controller,
- expose the index and zTOCs through a NodePort-backed controller endpoint,
- have the node daemon fetch those artifacts through `127.0.0.1:<nodePort>`,
- lazy mount image layers while reading layer bytes from the original registry.

## Run

Build Linux `amd64` binaries first. The build uses CGO and zlib, so run this on
a Linux machine with the zlib development package installed:

```bash
sudo apt-get update
sudo apt-get install -y build-essential zlib1g-dev

CGO_ENABLED=1 go build -o bin/hermes-controller ./cmd/controller
CGO_ENABLED=1 go build -o bin/hermes-daemon ./cmd/daemon
```

Build or provide a controller image, then run:

```bash
HERMES_CONTROLLER_IMAGE=hermes-controller:local \
HERMES_ENDPOINT_MODE=nodeport-localhost \
HERMES_CONTROLLER_NODE_PORT=30080 \
HERMES_BENCH_IMAGE=public.ecr.aws/docker/library/golang:1.25-bookworm \
HERMES_POLICY_IMAGE_REGEX='.*golang.*' \
HERMES_SKIP_OVERLAY_BASELINE=true \
HERMES_KEEP_HERMES_CLUSTER=true \
./hack/kind/benchmark-kind-controller-pod.sh
```

Results are written under `out/benchmarks/`.

For the broader EC2 + kind validation matrix, see
[EC2 + kind Test Cases](../ec2-kind-test-cases.md).

## Endpoint Mode

The current benchmark path uses the node-local NodePort endpoint:

```bash
HERMES_ENDPOINT_MODE=nodeport-localhost
```

`nodeport-localhost` is the current EC2 + kind validation path. It changes the
controller backing Service to `NodePort` and configures the node daemon with
`http://127.0.0.1:<nodePort>`. The default node port is `30080`, configurable
with `HERMES_CONTROLLER_NODE_PORT`.

## NodePort Note

The daemon runs as a host systemd service inside the kind worker node, not as a
Pod. Using NodePort keeps the controller endpoint node-local and avoids relying
on Kubernetes Service DNS from the host process. This matches the intended EKS
deployment model.

## Expected Evidence

A successful run should include lines like:

```text
POLICY_NAME=hermes-bench-policy
POLICY_IMAGE_REGEX=...
CONTROLLER_NODE_PORT=30080
HERMES_ENDPOINT=http://127.0.0.1:30080
SOCI_INDEX_DIGEST=sha256:...
SOCI_ZTOC_COUNT=...
CONTROLLER_SOURCE_IMAGE_CLEAN=true
hermes_POD_READY_MS=...
PASS
```

The daemon logs should include:

```text
fetching index from Hermes artifact store
remote snapshot successfully prepared.
fetching artifact from remote
```
