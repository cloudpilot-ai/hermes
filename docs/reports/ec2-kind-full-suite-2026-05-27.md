# Hermes EC2 + kind Full Test Report

Test date: 2026-05-27

## Summary

This report records a full run of the EC2 + kind test suite defined in
`docs/ec2-kind-test-cases.md`. The suite was executed on the AWS lab EC2 host
and covered TC-00 through TC-09.

Overall result: PASS.

Key findings:

1. The EC2 host can build Hermes and run the kind-based controller/daemon tests.
2. All product behavior tests passed, including public image build, private ECR
   image auth through Pod `imagePullSecrets`, NodePort localhost access,
   policy-negative fallback, cache reuse, RBAC, and controller source image
   cleanup.
3. The current endpoint path is `http://127.0.0.1:30080`; the Kubernetes
   Service is only the NodePort backing object.
4. The vLLM lazy-loading path reached Pod Ready in `15032 ms` after the artifact
   was Ready, compared with `333589 ms` for the normal overlayfs baseline.
   That is about `22.2x` faster for Pod Ready in this lab run.
5. The bad-secret case failed closed as expected. AWS ECR returned
   `400 Bad Request` for the invalid token path, and Hermes did not create a
   Ready artifact.
6. The first cache-reuse second run exposed a benchmark harness bug, not a
   product bug. `create_cluster()` returned status `1` when reusing an existing
   kind cluster under `set -e`. The harness was fixed by returning `0`, then the
   cache-reuse test was rerun and passed.

## Environment

| Item | Value |
| --- | --- |
| EC2 instance | `i-0ddba1077bbb797b8` |
| EC2 name | `soci-kind-test` |
| AWS Region | `us-east-2` |
| Instance type | `m6i.large` |
| OS | Amazon Linux 2023 |
| Kubernetes environment | kind |
| Docker | `25.0.14` |
| containerd | `2.2.3+unknown` |
| kind | `v0.31.0` |
| kubectl client | `v1.36.1` |
| Go | `go1.25.9 linux/amd64` |
| GCC | `11.5.0` |
| AWS caller | `arn:aws:sts::306107317780:assumed-role/soci-kind-test-ssm-role/i-0ddba1077bbb797b8` |
| Controller image | `hermes-controller:e2e` |
| Daemon endpoint mode | `nodeport-localhost` |
| Controller NodePort | `30080` |
| Daemon endpoint | `http://127.0.0.1:30080` |
| Local evidence copy | `out/remote-full-suite/full-suite-20260527T084304Z` |
| Remote evidence path | `/root/hermes-current/out/full-suite-20260527T084304Z` |

Host readiness and build commands completed successfully:

```text
docker version
kind version
kubectl version --client=true
aws sts get-caller-identity
go version
gcc --version
CGO_ENABLED=1 go build -o bin/hermes-controller ./cmd/controller
CGO_ENABLED=1 go build -o bin/hermes-daemon ./cmd/daemon
```

The Go validation suite also passed:

```text
go test ./pkg/apis/... ./pkg/controller/... ./cmd/controller
```

## Test Matrix Result

| ID | Area | Result | Evidence |
| --- | --- | --- | --- |
| TC-00 | EC2 host readiness | PASS | `tc00-host-readiness.rc=0` |
| TC-01 | Public-image smoke | PASS | Golang image built and lazy-mounted |
| TC-02 | Large-image benchmark | PASS | vLLM overlay and Hermes metrics recorded |
| TC-03 | NodePort localhost endpoint | PASS | `HERMES_ENDPOINT=http://127.0.0.1:30080` |
| TC-04 | Private ECR auth from Pod | PASS | vLLM pull succeeded using Pod auth path |
| TC-05 | Bad or missing pull secret | PASS | Expected failure; no Ready artifact |
| TC-06 | Policy selector negative | PASS | Unmatched busybox created no artifact |
| TC-07 | Cache reuse | PASS | Reran after harness fix; second build reused cache |
| TC-08 | RBAC guard | PASS | Controller service account can read secrets; no global registry env vars |
| TC-09 | Controller image cleanup | PASS | `CONTROLLER_SOURCE_IMAGE_CLEAN=true` |

The suite consolidated overlapping tests:

| Run | Covered IDs | Result file |
| --- | --- | --- |
| Public Golang NodePort cleanup run | TC-01, TC-03, TC-09 | `benchmarks/kind-controller-pod-20260527084305.txt` |
| vLLM ECR auth run | TC-02, TC-04 | `benchmarks/kind-controller-pod-20260527084438.txt` |
| Bad secret run | TC-05 | `tc05-bad-secret.stdout.log` |
| Policy negative run | TC-06 | `benchmarks/kind-controller-pod-20260527090421.txt` |
| Cache reuse fixed run | TC-07 | `benchmarks-cache-fixed/kind-controller-pod-20260527091303.txt`, `benchmarks-cache-fixed/kind-controller-pod-20260527091425.txt` |
| RBAC guard run | TC-08 | `tc08-rbac-guard.stdout.log`, `tc08-rbac-guard.stderr.log` |

## Key Metrics

| Scenario | Metric | Value |
| --- | --- | ---: |
| Public Golang | artifact Ready | `13348 ms` |
| Public Golang | Hermes Pod Ready | `5129 ms` |
| Public Golang | zTOC count | `5` |
| Public Golang | total zTOC size | `12780600 bytes` |
| Public Golang | source image cleanup | `true` |
| vLLM overlay baseline | Pod Ready | `333589 ms` |
| vLLM Hermes | artifact Ready | `537763 ms` |
| vLLM Hermes | Pod Ready | `15032 ms` |
| vLLM Hermes | Pod Ready speedup | `22.2x` |
| vLLM Hermes | zTOC count | `12` |
| vLLM Hermes | total zTOC size | `182954760 bytes` |
| vLLM Hermes | source image cleanup | `true` |
| Policy negative | unmatched busybox Pod Ready | `1704 ms` |
| Policy negative | unmatched artifact count | `0` |
| Cache reuse first fixed run | artifact Ready | `15998 ms` |
| Cache reuse first fixed run | Hermes Pod Ready | `4486 ms` |
| Cache reuse second fixed run | artifact Ready | `903 ms` |
| Cache reuse second fixed run | Hermes Pod Ready | `2587 ms` |

## Evidence Highlights

Public Golang NodePort run:

```text
IMAGE=public.ecr.aws/docker/library/golang:1.25-bookworm
HERMES_ENDPOINT=http://127.0.0.1:30080
HERMES_READY_REF=public.ecr.aws/docker/library/golang@sha256:c99705d76da262268a7d29ff9638b2ad51d141512fea8489f5bad3e4a6e95d07
SOCI_INDEX_DIGEST=sha256:2f43a5f5a2be3c7c123040412011bc86f6ff31aa63abc8fe5de94c94108e9651
SOCI_ZTOC_COUNT=5
HERMES_ARTIFACT_READY_MS=13348
CONTROLLER_SOURCE_IMAGE_CLEAN=true
hermes_POD_READY_MS=5129
PASS
```

vLLM ECR auth and benchmark run:

```text
IMAGE=763104351884.dkr.ecr.us-east-1.amazonaws.com/vllm:0.9-gpu-py312-ec2
overlay_POD_READY_MS=333589
HERMES_ENDPOINT=http://127.0.0.1:30080
HERMES_READY_REF=763104351884.dkr.ecr.us-east-1.amazonaws.com/vllm@sha256:7ca69228a9066855929a9260bed4f8f076f3433f57fc0c05cc1ae425fd19d2b9
SOCI_INDEX_DIGEST=sha256:222d53c03c52b9a157d90505e1343628e4bf7d51292c8b14ff9c650ccded7d18
SOCI_ZTOC_COUNT=12
HERMES_ARTIFACT_READY_MS=537763
CONTROLLER_SOURCE_IMAGE_CLEAN=true
hermes_POD_READY_MS=15032
PASS
```

Bad secret expected-failure run:

```text
artifact failed: pull image 763104351884.dkr.ecr.us-east-1.amazonaws.com/vllm:0.9-gpu-py312-ec2 platform=linux/amd64: failed to resolve reference "763104351884.dkr.ecr.us-east-1.amazonaws.com/vllm:0.9-gpu-py312-ec2": unexpected status from HEAD request to https://763104351884.dkr.ecr.us-east-1.amazonaws.com/v2/vllm/manifests/0.9-gpu-py312-ec2: 400 Bad Request
```

Policy negative run:

```text
HERMES_ENDPOINT=http://127.0.0.1:30080
HERMES_READY_REF=public.ecr.aws/docker/library/golang@sha256:c99705d76da262268a7d29ff9638b2ad51d141512fea8489f5bad3e4a6e95d07
CONTROLLER_SOURCE_IMAGE_CLEAN=true
hermes_unmatched_POD_READY_MS=1704
UNMATCHED_ARTIFACT_COUNT=0
PASS
```

Cache reuse fixed rerun:

```text
first:  HERMES_ARTIFACT_READY_MS=15998, hermes_POD_READY_MS=4486
second: HERMES_ARTIFACT_READY_MS=903, hermes_POD_READY_MS=2587
same index: sha256:2f43a5f5a2be3c7c123040412011bc86f6ff31aa63abc8fe5de94c94108e9651
CONTROLLER_SOURCE_IMAGE_CLEAN=true
PASS
```

Controller cache and cleanup logs included:

```text
skip cached index image=public.ecr.aws/docker/library/golang@sha256:c99705d76da262268a7d29ff9638b2ad51d141512fea8489f5bad3e4a6e95d07
cleaned source image image=public.ecr.aws/docker/library/golang:1.25-bookworm ... reason=cached
```

RBAC guard:

```text
kubectl --context kind-hermes-vllm-nodeport auth can-i get secrets --as system:serviceaccount:hermes-system:hermes-controller
yes
```

The controller deployment did not contain the removed global registry auth
configuration:

```text
registry-auth-host: not present
registry-username: not present
registry-password: not present
HERMES_CONTROLLER_REGISTRY_AUTH_HOST: not present
HERMES_CONTROLLER_REGISTRY_USERNAME: not present
HERMES_CONTROLLER_REGISTRY_PASSWORD: not present
```

## Harness Fix

The first TC-07 second run failed before producing product evidence. The cause
was in the benchmark harness:

```bash
create_cluster() {
  if kind get clusters | grep -qx "${cluster}"; then
    if [[ "${RECREATE_CLUSTERS}" == "true" ]]; then
      kind delete cluster --name "${cluster}"
    else
      return 0
    fi
  fi
}
```

Before the fix, the `return` inherited the failed status of
`[[ "${RECREATE_CLUSTERS}" == "true" ]]`, which made the script exit under
`set -e` whenever a test intentionally reused an existing kind cluster. After
changing it to `return 0`, the cache-reuse test passed in a clean fixed rerun.

## Post-run Cleanup

The EC2 lab was cleaned after the suite:

```text
No kind clusters found.
Filesystem      Size  Used Avail Use% Mounted on
/dev/nvme0n1p1   80G   12G   69G  15% /
```

## Limitations

1. The run used a single `m6i.large` EC2 kind lab, not a multi-node EKS cluster.
2. The vLLM workload used the benchmark Pod path and measured image pull,
   artifact build, and Pod Ready timing. It did not validate a real model load.
3. The TC-05 bad-secret error surfaced as an AWS ECR `400 Bad Request`, not a
   literal `401` or `403`, but the behavior still failed closed and produced no
   Ready artifact.
