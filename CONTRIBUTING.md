# Contributing

Thanks for taking an interest in Hermes.

Before opening a larger change, please start with an issue or design note so the direction can be discussed first. For code changes, keep the pull request focused, run `gofmt`, and include enough test or manual verification detail for reviewers to understand the behavior.

Useful local checks:

```shell
go mod tidy
GOOS=linux GOARCH=amd64 go list ./...
bash -n hack/kind/*.sh
```

The snapshotter code depends on Linux/containerd behavior, so full build and
test validation should be done in CI or on a Linux worker. Controller image
builds use `ko`; the repository keeps `.ko.yaml` for that path instead of a
Dockerfile. Kind e2e helpers live under `hack/kind/`.
