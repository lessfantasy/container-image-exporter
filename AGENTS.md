# AGENTS.md

Guidance for people and coding agents working on this repository.

## What this is

A Prometheus exporter in Go. It discovers running container images in
Docker or Kubernetes, checks their registries for newer versions and
exposes two metrics. Read `README.md` for behaviour and
`docs/metrics-design.md` for the metric contract.

## Commands

```bash
make lint    # gofmt check and go vet
make test    # go test ./... and go test -race ./...
make build   # static binary in bin/
```

All three must pass before a change is done. Tests must not need network
access beyond localhost, a Docker daemon, a cluster or credentials.

## Layout

```text
cmd/exporter                   main: wiring only
internal/config                flags and environment
internal/discovery             Instance model, Discoverer interface
internal/discovery/docker      Docker Engine API
internal/discovery/kubernetes  Kubernetes API, owner resolution
internal/image                 reference normalization
internal/registry              registry client and credential sources
internal/version               strategy detection
internal/version/semver        tag family and comparison
internal/version/floating      digest comparison
internal/cache                 result store (TTL, backoff)
internal/scanner               scan loop, worker pool, snapshot
internal/metrics               aggregation and collector
internal/server                HTTP endpoints
tests                          end-to-end tests with local fakes
deploy                         Docker and Kubernetes examples
packaging/deb                  Debian package: unit, maintainer scripts
```

Dependency direction: `scanner` is the only package that imports
discovery, registry, version and metrics together. Keep those four
independent of each other.

## Invariants

Do not break these without an explicit decision by the maintainers:

1. Exactly two metric families, with the label sets in
   `internal/metrics/metrics.go`. No further metrics, no default
   collectors, no global Prometheus registry.
2. Every series carries every label of its family on every platform.
3. `/metrics` performs no I/O. It reads the snapshot.
4. An unknown update status is never published as `0`. The update series
   is omitted and `container_image_info` is `0`.
5. A failed check replaces an earlier success. Failures are retried with
   backoff and never cached for the full registry interval.
6. A discovery error is never turned into an empty inventory.
7. Read-only towards Docker (GET), Kubernetes (list; get on Secrets only
   behind its flag) and registries (GET, HEAD).
8. No configuration files. Flags and environment variables only, with the
   precedence flags > environment > defaults.
9. Credentials, tokens and secret contents are never logged.
10. Unrecognized tags are reported as unsupported. No version is guessed.

## Conventions

- Standard library first: `flag`, `log/slog`, `net/http`.
- Table-driven tests. Mock at the narrow interface a package defines
  (`docker.API`, `scanner.Registry`, `discovery.Discoverer`).
- Errors are wrapped with context: `fmt.Errorf("list pods: %w", err)`.
- New tag rules need rows in `internal/version/version_test.go` and
  `internal/version/semver/semver_test.go`, and a line in the README.

## Extending

- **New runtime**: implement `discovery.Discoverer`, add a mode in
  `internal/config` and a case in `cmd/exporter/main.go`.
- **New Kubernetes intermediate controller** (a kind between Pod and the
  top-level workload): add it to `intermediateOwners` in
  `internal/discovery/kubernetes/kubernetes.go` and grant `list` in
  `deploy/kubernetes/rbac.yaml`.
- **New versioning strategy**: add a `version.Strategy`, detect it in
  `version.Detect` with a documented precedence, handle it in
  `internal/scanner/checker.go`.
- **New registry credential source**: add a host-gated keychain in
  `internal/registry/keychain.go`.

## Out of scope

Updating or restarting containers, a web UI, dashboards, vulnerability
scanning, GitHub release comparison, plugins, databases, persistent
caches, calendar versioning, commit ancestry, configuration files.
