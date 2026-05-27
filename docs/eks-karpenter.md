# EKS + Karpenter Test Build and Install

This document is the fast path for testing Hermes on EKS nodes created by
Karpenter.

The controller runs as a normal Kubernetes Deployment. The node-side
`hermes-daemon` runs directly on each EC2 worker node as the containerd
snapshotter gRPC service.

## 1. Build Artifacts in GitHub Actions

Run the `Hermes Build` workflow manually, or push to `main`.

Outputs:

```text
public.ecr.aws/cloudpilotai/hermes-controller:latest
hermes-daemon-linux-amd64-tar-gz GitHub Actions artifact
hermes-daemon-linux-amd64-tar-gz-sha256 GitHub Actions artifact
```

The image build follows the same pattern as `cloudpilot-agent`: GitHub Actions
sets `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` from `secrets.AWS_AK` and
`secrets.AWS_SK`, logs in to ECR Public, then `ko build --bare` pushes the
controller image to `KO_DOCKER_REPO=public.ecr.aws/cloudpilotai/hermes-controller`.

The ECR Public repository must already exist before the workflow runs. GitHub
Actions downloads artifacts as zip files, so extract `hermes-daemon-linux-amd64.tar.gz`
from `hermes-daemon-linux-amd64-tar-gz` and publish the tarball itself to a URL
that new EC2 nodes can read, such as an S3 presigned URL. Pass that tarball URL
as `HERMES_DAEMON_URL` when installing the node daemon. If you want checksum
verification, extract `hermes-daemon-linux-amd64.tar.gz.sha256` from
`hermes-daemon-linux-amd64-tar-gz-sha256` and pass its contents as
`HERMES_DAEMON_SHA256`; it must be the raw 64-character digest.

## 2. Deploy the Controller

```bash
export HERMES_CONTROLLER_IMAGE=public.ecr.aws/cloudpilotai/hermes-controller:latest

kubectl apply -f deploy/hermespolicy-crd.yaml

sed "s|ghcr.io/example/hermes-controller:latest|${HERMES_CONTROLLER_IMAGE}|" \
  deploy/hermes-controller.yaml | kubectl apply -f -

kubectl -n hermes-system rollout status deploy/hermes-controller
kubectl -n hermes-system get svc hermes-controller -o wide
```

The default Service is a NodePort backing object:

```text
hermes-controller hermes-system 39091:30080/TCP
```

The node daemon defaults to `HERMES_ENDPOINT=auto-nodeport`, which reads the EC2
node primary IP from IMDS and uses `http://<node-ip>:30080`.

## 3. Install the Node gRPC Snapshotter Through Karpenter

Add the install command to the Karpenter `EC2NodeClass` user data. Keep both
URLs in quoted variables; S3 presigned URLs contain `&`, which the shell will
otherwise split into background commands.

```yaml
apiVersion: karpenter.k8s.aws/v1
kind: EC2NodeClass
metadata:
  name: hermes-al2023
spec:
  amiFamily: AL2023
  userData: |
    #!/bin/bash
    set -euxo pipefail

    export HERMES_INSTALLER_URL="https://raw.githubusercontent.com/cloudpilot-ai/hermes/main/hack/eks/install-hermes-daemon.sh"
    export HERMES_DAEMON_URL="<https-url-to-hermes-daemon-linux-amd64.tar.gz>"
    export HERMES_DAEMON_SHA256="<raw-64-character-sha256>"

    curl -fsSL "${HERMES_INSTALLER_URL}" | \
      HERMES_DAEMON_URL="${HERMES_DAEMON_URL}" \
      HERMES_DAEMON_SHA256="${HERMES_DAEMON_SHA256}" \
      bash -s --
```

The installer writes:

```text
/usr/local/bin/hermes-daemon
/etc/hermes-daemon/config.toml
/etc/systemd/system/hermes-daemon.service
```

It also configures containerd with the `soci` proxy snapshotter, keeps snapshot
annotations enabled, enables the daemon CRI keychain, and routes kubelet image
service calls through the daemon so private registry credentials are available
to lazy layer reads.

## 4. Validate on a Karpenter Node

After a new node joins:

```bash
kubectl get nodes -l karpenter.sh/nodepool=<nodepool-name> -o wide
kubectl -n hermes-system logs deploy/hermes-controller
```

On the EC2 node:

```bash
sudo systemctl status hermes-daemon.service --no-pager
sudo ctr plugin ls | grep -E '(^TYPE|soci)'
sudo grep -n 'proxy_plugins.soci\|snapshotter = "soci"' /etc/containerd/config.toml
sudo journalctl -u hermes-daemon.service -n 100 --no-pager
```

Create a policy and a test Pod:

```bash
kubectl apply -f examples/kubernetes/hermespolicy.yaml
kubectl run hermes-vllm-trigger \
  --image=763104351884.dkr.ecr.us-east-1.amazonaws.com/vllm:0.9-gpu-py312-ec2 \
  --restart=Never \
  --command -- sleep 300
```

Expected controller-side signs:

```text
enqueued image=...
pulling image=... through containerd API
built index in-process ...
ready image=... index=sha256:...
cleaned source image ...
```

Expected node-side signs:

```text
fetching index from Hermes artifact store
remote snapshot successfully prepared.
fetching artifact from remote
```
