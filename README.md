# container-image-exporter

A small Prometheus exporter that tells you whether the container images
running in Docker or Kubernetes have a newer version in their registry.

It discovers the images that are running right now, asks the registry of
each image for newer tags (SemVer tags) or for the current digest (moving
tags such as `latest`), and exposes exactly two metrics with the same
labels on every platform. It never changes, restarts or updates anything.

```text
container_image_info{platform, container, namespace, workload_kind, registry, repository, current_tag, latest_tag}
container_image_update_available{platform, container, namespace, workload_kind, registry, repository}
```

## Contents

- [Quick start](#quick-start)
- [Architecture](#architecture)
- [Supported environments](#supported-environments)
- [Supported registries and authentication](#supported-registries-and-authentication)
- [Configuration](#configuration)
- [Docker deployment](#docker-deployment)
- [Kubernetes deployment](#kubernetes-deployment)
- [Prometheus configuration](#prometheus-configuration)
- [Metrics reference](#metrics-reference)
- [Version detection](#version-detection)
- [Kubernetes deduplication](#kubernetes-deduplication)
- [Failure behaviour](#failure-behaviour)
- [Known limitations](#known-limitations)
- [Troubleshooting](#troubleshooting)
- [Security considerations](#security-considerations)
- [Development](#development)

## Quick start

```bash
make build
```

```bash
./bin/container-image-exporter --mode=docker
```

```bash
curl -s http://localhost:9877/metrics
```

Requirements: Go 1.27 or newer to build. The binary is statically linked
(`CGO_ENABLED=0`) and runs on linux/amd64 and linux/arm64.

## Architecture

```text
                 scan loop (one goroutine, every --scan-interval)
 ┌───────────┐   ┌──────────────┐   ┌───────────────┐   ┌─────────────┐
 │ discovery │ → │ plan lookups │ → │ worker pool   │ → │ snapshot    │
 │ docker or │   │ normalize +  │   │ (bounded) →   │   │ (immutable, │
 │ kubernetes│   │ deduplicate  │   │ registry      │   │  atomic)    │
 └───────────┘   └──────────────┘   └───────┬───────┘   └──────┬──────┘
                                            │                  │
                                     in-memory cache     /metrics handler
                                     (TTL + backoff)     (reads memory only)
```

| Package | Responsibility |
|---|---|
| `cmd/exporter` | Wiring, signal handling, exit codes |
| `internal/config` | Flags and environment variables |
| `internal/discovery` | `Instance` model and `Discoverer` interface |
| `internal/discovery/docker` | Docker Engine API (Docker and Compose) |
| `internal/discovery/kubernetes` | Kubernetes API, owner resolution, deduplication |
| `internal/image` | Image reference normalization |
| `internal/registry` | Registry client, credential sources, error classification |
| `internal/version` | Strategy detection (SemVer, floating, unsupported) |
| `internal/version/semver` | Version extraction, tag family, newest compatible tag |
| `internal/version/floating` | Digest comparison |
| `internal/cache` | Result store with TTL and failure backoff |
| `internal/scanner` | Scan loop, worker pool, rate-limit pauses, snapshot |
| `internal/metrics` | Series aggregation and Prometheus collector |
| `internal/server` | HTTP endpoints, graceful shutdown |

Discovery, registry access, version logic and metric generation do not
import each other; only `scanner` connects them. A new runtime implements
`discovery.Discoverer`; a new versioning strategy adds a `version.Strategy`
and a case in `scanner/checker.go`.

Design rules:

- `/metrics` never performs I/O. It serves the last snapshot from memory.
- Identical registry lookups are made once per cycle, however many
  containers use the image. A tag listing is shared by all tags of a
  repository.
- Two independent intervals: `--scan-interval` for runtime discovery,
  `--registry-interval` for how long a successful registry result is
  reused.
- At most `--registry-concurrency` registry checks run in parallel. No
  goroutine is created per image.

## Supported environments

| Environment | Discovery | Notes |
|---|---|---|
| Docker (`docker run`) | Docker Engine API | Running containers only |
| Docker Compose | Same implementation | No labels or changes needed |
| Kubernetes | Kubernetes API with a ServiceAccount | All namespaces, containers and init containers |

Kubernetes workload kinds: Deployment, StatefulSet, DaemonSet, ReplicaSet,
ReplicationController, Job, CronJob and standalone Pod. Other controller
kinds (for example an Argo `Rollout`) are reported under their own kind.

## Supported registries and authentication

All registries are accessed through the OCI / Docker Registry v2 API using
[go-containerregistry](https://github.com/google/go-containerregistry).
Public images need no credentials.

| Registry | Hosts | Credentials for private images |
|---|---|---|
| Docker Hub | `docker.io` | Docker config |
| GitHub Container Registry | `ghcr.io` | Docker config |
| Quay | `quay.io` | Docker config |
| Amazon ECR | `*.dkr.ecr.*.amazonaws.com`, `public.ecr.aws` | AWS default credential chain: environment, IRSA, EKS Pod Identity, instance profile |
| Google Artifact Registry / GCR | `*.pkg.dev`, `gcr.io`, `*.gcr.io` | Application Default Credentials, GKE Workload Identity |
| Azure Container Registry | `*.azurecr.io` | `AZURE_*` service principal variables, managed identity |
| Any other OCI registry | any | Docker config |

Credential sources, in order. The first source with credentials for the
registry wins; otherwise the request is anonymous:

1. **Kubernetes imagePullSecrets** of the pod that runs the image. Only
   with `--kubernetes-image-pull-secrets=true` and the optional RBAC in
   `deploy/kubernetes/rbac-image-pull-secrets.yaml`.
2. **Docker config**: `$DOCKER_CONFIG/config.json`, or
   `~/.docker/config.json`. `auths`, `credHelpers` and `credsStore` are
   honoured, so Docker credential helpers work when their binaries are on
   `PATH`. The published container image contains no helper binaries; the
   three cloud providers below are built in instead.
3. **Google** credentials, for Google hosts only.
4. **AWS** credentials, for ECR hosts only.
5. **Azure** credentials, for ACR hosts only.

A broken credential source is skipped and logged at debug level. Access
tokens, passwords and secret contents are never logged.

Reference normalization follows Docker's rules: `nginx` is
`docker.io/library/nginx:latest`, `grafana/grafana` is
`docker.io/grafana/grafana:latest`, and `registry.example.com:5000/app`
keeps its registry. Docker Hub is always labelled `docker.io`.

## Configuration

Only CLI flags and environment variables. There is no configuration file.

Precedence: **CLI flags > environment variables > defaults**.

| Flag | Environment variable | Default | Purpose |
|---|---|---|---|
| `--mode` | `EXPORTER_MODE` | `docker` | `docker` or `kubernetes` |
| `--listen-address` | `EXPORTER_LISTEN_ADDRESS` | `:9877` | HTTP listen address |
| `--docker-host` | `EXPORTER_DOCKER_HOST` | empty | Docker endpoint. Empty uses `DOCKER_HOST` or the default socket |
| `--kubeconfig` | `EXPORTER_KUBECONFIG` | empty | Kubeconfig for local development. Empty uses the in-cluster ServiceAccount |
| `--scan-interval` | `EXPORTER_SCAN_INTERVAL` | `1m` | Interval between discovery scans |
| `--registry-interval` | `EXPORTER_REGISTRY_INTERVAL` | `6h` | How long a successful registry check is reused |
| `--registry-timeout` | `EXPORTER_REGISTRY_TIMEOUT` | `30s` | Deadline for all registry requests of one image check |
| `--registry-concurrency` | `EXPORTER_REGISTRY_CONCURRENCY` | `4` | Maximum parallel registry checks (1 to 64) |
| `--kubernetes-image-pull-secrets` | `EXPORTER_KUBERNETES_IMAGE_PULL_SECRETS` | `false` | Use imagePullSecrets of discovered pods |
| `--log-level` | `EXPORTER_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `--version` | | | Print the version and exit |
| `--help` | | | Print usage and exit |

Authentication uses the standard variables of each provider:
`DOCKER_CONFIG`, `AWS_REGION`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`,
`AWS_ROLE_ARN`, `AWS_WEB_IDENTITY_TOKEN_FILE`,
`GOOGLE_APPLICATION_CREDENTIALS`, `AZURE_TENANT_ID`, `AZURE_CLIENT_ID`,
`AZURE_CLIENT_SECRET`.

Invalid configuration prints every problem and exits with code 2. A
runtime that cannot be initialised (for example Kubernetes mode outside a
cluster without `--kubeconfig`) exits with code 1.

HTTP endpoints:

| Path | Purpose |
|---|---|
| `/metrics` | The two metric families. `503` while no reliable scan result exists |
| `/healthz` | Liveness. Always `200` |
| `/readyz` | `200` once a scan result exists, otherwise `503` |

## Docker deployment

Build the image (multi-stage, distroless, non-root, about 35 MB):

```bash
docker build -t container-image-exporter:local .
```

### With a socket proxy (recommended)

```bash
docker compose -f deploy/docker/docker-compose.yml up -d --build
```

The exporter talks to
[docker-socket-proxy](https://github.com/Tecnativa/docker-socket-proxy),
which only allows `GET` on the container and image endpoints. The exporter
itself has no access to `docker.sock`.

### With the socket directly

```bash
DOCKER_GID=$(stat -c '%g' /var/run/docker.sock) docker compose -f deploy/docker/docker-compose.socket.yml up -d --build
```

> **Mounting `docker.sock` read-only does not make the Docker API
> read-only.** `:ro` only protects the socket file. Any process that can
> open the socket can start privileged containers, which is root on the
> host. The exporter only sends `GET` requests, but a compromised exporter
> would not be limited to them. Use the socket proxy in production.

### CLI example

```bash
docker run -d --name container-image-exporter -p 127.0.0.1:9877:9877 --read-only --cap-drop ALL --group-add "$(stat -c '%g' /var/run/docker.sock)" -v /var/run/docker.sock:/var/run/docker.sock:ro container-image-exporter:local --mode=docker --scan-interval=1m --registry-interval=6h
```

### Environment variable example

```bash
docker run -d --name container-image-exporter -p 127.0.0.1:9877:9877 -e EXPORTER_MODE=docker -e EXPORTER_DOCKER_HOST=tcp://docker-socket-proxy:2375 -e EXPORTER_REGISTRY_INTERVAL=6h -e EXPORTER_LOG_LEVEL=info --network socket-proxy container-image-exporter:local
```

`socket-proxy` stands for a Docker network that the socket proxy container
is attached to. See `deploy/docker/exporter.env.example` for all variables.

Docker API endpoints used: `GET /_ping`, `GET /containers/json`,
`GET /containers/{id}/json`, `GET /images/{id}/json`.

### Debian package

`make deb VERSION=0.1.0` writes packages for amd64 and arm64 to `dist/`.

```bash
sudo apt install ./dist/container-image-exporter_0.1.0_amd64.deb
```

The package installs the binary to `/usr/bin`, a hardened systemd unit
`container-image-exporter.service` and the environment file
`/etc/default/container-image-exporter` (mode 0640, kept on upgrade). It
creates the system user `container-image-exporter`, then enables and
starts the service.

The package does not grant access to the Docker socket, because
membership of the `docker` group is equivalent to root. Until access is
granted, `/metrics` answers `503`. Either point `EXPORTER_DOCKER_HOST` at
a socket proxy, or grant access explicitly:

```bash
sudo adduser container-image-exporter docker
```

```bash
sudo systemctl restart container-image-exporter
```

## Kubernetes deployment

The manifests use the namespace `monitoring`. Replace the image in
`deployment.yaml` with one you built and pushed.

```bash
kubectl create namespace monitoring
```

```bash
kubectl apply -f deploy/kubernetes/rbac.yaml -f deploy/kubernetes/deployment.yaml -f deploy/kubernetes/service.yaml
```

```bash
kubectl apply -f deploy/kubernetes/servicemonitor.yaml
```

| File | Content |
|---|---|
| `rbac.yaml` | ServiceAccount, ClusterRole, ClusterRoleBinding |
| `deployment.yaml` | Deployment: non-root, read-only root filesystem, no capabilities |
| `service.yaml` | Service on port 9877 |
| `servicemonitor.yaml` | Optional, for the Prometheus Operator |
| `rbac-image-pull-secrets.yaml` | Optional, read access to Secrets for imagePullSecrets |

RBAC is read-only and minimal: `list` on `pods`, `replicasets` and `jobs`.
No `watch`, no write verbs. The exporter needs no privileged container, no
host network and no container runtime socket.

For private registries prefer workload identity: annotate the
ServiceAccount for IRSA, GKE Workload Identity or Azure Workload Identity.
Alternatively mount a `kubernetes.io/dockerconfigjson` Secret and set
`DOCKER_CONFIG` (commented example in `deployment.yaml`).

Local development against a cluster:

```bash
./bin/container-image-exporter --mode=kubernetes --kubeconfig="$HOME/.kube/config"
```

## Prometheus configuration

```yaml
scrape_configs:
  - job_name: container-image-exporter
    scrape_interval: 1m
    # Keep the exporter's "namespace" label (the monitored workload's
    # namespace) when Prometheus adds a target label of the same name.
    honor_labels: true
    static_configs:
      - targets: ["container-image-exporter:9877"]
```

Example alerts:

```yaml
groups:
  - name: container-images
    rules:
      - alert: ContainerImageUpdateAvailable
        expr: container_image_update_available == 1
        for: 24h
        annotations:
          summary: "{{ $labels.container }} runs an outdated {{ $labels.repository }} image"
      - alert: ContainerImageCheckFailing
        expr: container_image_info == 0
        for: 12h
        annotations:
          summary: "Version check fails for {{ $labels.repository }}:{{ $labels.current_tag }}"
      - alert: ContainerImageExporterDown
        expr: up{job="container-image-exporter"} == 0
        for: 15m
```

Join to see the target version of every outdated image:

```promql
container_image_info * on (platform, container, namespace, workload_kind, registry, repository) group_left() (container_image_update_available == 1)
```

## Metrics reference

Exactly two metric families are exposed. There are no `go_*`,
`process_*`, `promhttp_*` or `prometheus_*` metrics: the exporter uses a
dedicated registry with a single custom collector.

### `container_image_info`

Gauge. `1`: discovery and registry check succeeded. `0`: the check failed
or its result cannot be determined reliably.

| Label | Docker | Kubernetes |
|---|---|---|
| `platform` | `docker` | `kubernetes` |
| `container` | Container name | Name of the top-level workload |
| `namespace` | empty | Namespace |
| `workload_kind` | empty | `Deployment`, `StatefulSet`, ... |
| `registry` | Registry host | Registry host |
| `repository` | Repository path | Repository path |
| `current_tag` | Running tag | Running tag |
| `latest_tag` | Newest compatible tag, empty when unknown | Same |

### `container_image_update_available`

Gauge. `1`: a newer image is available. `0`: up to date. **The series is
absent when the update status is unknown.**

Labels: `platform`, `container`, `namespace`, `workload_kind`, `registry`,
`repository`.

Every series of a family carries every label of that family on every
platform. Docker series have `namespace=""` and `workload_kind=""`.

### How `latest_tag` is determined

- **SemVer tag**: the highest version among the registry tags that belong
  to the same tag family as the running tag (see below). When nothing
  newer exists it equals `current_tag`.
- **Floating tag**: always the same as `current_tag`. The tag did not
  change, its content did.
- **Failed check**: empty.

### Example: Docker

```text
# HELP container_image_info Running container image. 1 = version check succeeded, 0 = check failed or result unknown.
# TYPE container_image_info gauge
container_image_info{container="grafana",current_tag="11.2.0",latest_tag="11.3.1",namespace="",platform="docker",registry="docker.io",repository="grafana/grafana",workload_kind=""} 1
container_image_info{container="shop-db-1",current_tag="16.4-alpine",latest_tag="17.2-alpine",namespace="",platform="docker",registry="docker.io",repository="library/postgres",workload_kind=""} 1
container_image_info{container="shop-web-1",current_tag="latest",latest_tag="latest",namespace="",platform="docker",registry="docker.io",repository="library/nginx",workload_kind=""} 1
container_image_info{container="tools",current_tag="abc123",latest_tag="",namespace="",platform="docker",registry="ghcr.io",repository="acme/tools",workload_kind=""} 0
# HELP container_image_update_available 1 = a newer image is available, 0 = up to date. Absent when the update status is unknown.
# TYPE container_image_update_available gauge
container_image_update_available{container="grafana",namespace="",platform="docker",registry="docker.io",repository="grafana/grafana",workload_kind=""} 1
container_image_update_available{container="shop-db-1",namespace="",platform="docker",registry="docker.io",repository="library/postgres",workload_kind=""} 1
container_image_update_available{container="shop-web-1",namespace="",platform="docker",registry="docker.io",repository="library/nginx",workload_kind=""} 0
```

`tools` has no `container_image_update_available` series: `abc123` is not
a supported tag format, so the status is unknown.

### Example: Kubernetes

```text
container_image_info{container="backend-api",current_tag="v1.4.2",latest_tag="v1.5.0",namespace="prod",platform="kubernetes",registry="ghcr.io",repository="acme/backend-api",workload_kind="Deployment"} 1
container_image_info{container="backend-api",current_tag="v1.5.0",latest_tag="v1.5.0",namespace="prod",platform="kubernetes",registry="ghcr.io",repository="acme/backend-api",workload_kind="Deployment"} 1
container_image_info{container="coredns",current_tag="v1.14.6-eksbuild.4",latest_tag="v1.14.6-eksbuild.9",namespace="kube-system",platform="kubernetes",registry="602401143452.dkr.ecr.eu-west-1.amazonaws.com",repository="eks/coredns",workload_kind="Deployment"} 1
container_image_info{container="fluent-bit",current_tag="stable",latest_tag="stable",namespace="logging",platform="kubernetes",registry="docker.io",repository="fluent/fluent-bit",workload_kind="DaemonSet"} 1
container_image_info{container="nightly-report",current_tag="main-mongod8.3",latest_tag="main-mongod8.4",namespace="batch",platform="kubernetes",registry="docker.io",repository="percona/percona-server-mongodb-operator",workload_kind="CronJob"} 1
container_image_update_available{container="backend-api",namespace="prod",platform="kubernetes",registry="ghcr.io",repository="acme/backend-api",workload_kind="Deployment"} 1
container_image_update_available{container="coredns",namespace="kube-system",platform="kubernetes",registry="602401143452.dkr.ecr.eu-west-1.amazonaws.com",repository="eks/coredns",workload_kind="Deployment"} 1
container_image_update_available{container="fluent-bit",namespace="logging",platform="kubernetes",registry="docker.io",repository="fluent/fluent-bit",workload_kind="DaemonSet"} 0
container_image_update_available{container="nightly-report",namespace="batch",platform="kubernetes",registry="docker.io",repository="percona/percona-server-mongodb-operator",workload_kind="CronJob"} 1
```

`backend-api` is in the middle of a rolling update: ten pods produce two
info series (one per running tag) and one update series.

### Label collisions

`container_image_update_available` has no tag label, and neither metric
has a label for the individual container inside a pod. Several running
images can therefore map to one label set. The exporter never emits
duplicate series and never lets one result overwrite another. It merges
them conservatively:

| Situation | `container_image_info` | `container_image_update_available` |
|---|---|---|
| Rolling update, two tags of one repository | One series per tag | One series: `1` if any running tag has an update |
| Two containers of a workload, same repository and tag | One series: `1` only if all checks succeeded | One series |
| Replicas on one floating tag with different digests | One series | `1` if any replica is outdated |
| One of the merged images cannot be checked | Its own series is `0` | **Absent**: a partial answer would be misleading |

What the schema cannot express: which container of a workload is outdated
when two containers use the same repository with different tags (the info
series still shows both tags), and a per-tag update flag during a rolling
update. The smallest backward-compatible extension would be an additional
`current_tag` label on `container_image_update_available`. It is not
implemented, because it changes the requested label set. Details:
[docs/metrics-design.md](docs/metrics-design.md).

## Version detection

Each tag is classified by the first matching rule:

| Order | Rule | Strategy | Examples |
|---|---|---|---|
| 1 | Empty or not a valid tag | unsupported | `""`, `has space` |
| 2 | Known moving alias (whole tag, case-insensitive) | floating | `latest`, `stable`, `edge`, `nightly`, `main` |
| 3 | Digits and hyphens only | floating | `2026-11-10`, `20261110`, `8` |
| 4 | Contains a version with two or three dot-separated numbers | SemVer | `1.26.2`, `v1.14.6`, `main-mongod8.3` |
| 5 | Anything else | unsupported | `abc123`, `v1`, `jdk17` |

The full alias list is in `internal/version/version.go`. Unsupported tags
are never guessed: `container_image_info` is `0`.

### SemVer rules

A tag is split into `<prefix><version><suffix>`:

| Tag | Prefix | Version | Suffix |
|---|---|---|---|
| `1.26.2` | | `1.26.2` | |
| `main-mongod8.3` | `main-mongod` | `8.3` | |
| `v1.14.6-eksbuild.4` | `v` | `1.14.6` | `-eksbuild.4` |
| `1.27.0-rc.1-alpine3.20` | | `1.27.0` | `-rc.1-alpine3.20` |

- The **version** is the leftmost run of two or three dot-separated
  numbers. Four or more components (`1.2.3.4`) are unsupported.
- The **suffix** is split into words. A leading pre-release word (`alpha`,
  `beta`, `rc`, `pre`, `preview`, `dev`, `snapshot`, `nightly`, `canary`,
  with an optional number) is the pre-release. A word that is only digits,
  or `r`, `rev`, `build`, `b` followed by digits, is a **build counter**.
  Every other word (`alpine`, `alpine3`, `arm64v8`, `jdk17`, `eksbuild`)
  is a **variant identifier**. Numbers glued to letters are part of the
  identifier, not a counter.
- A suffix word that looks like a commit hash (7 to 40 hex characters with
  digits and letters) makes the tag unsupported: such tags cannot be
  ordered.

A registry tag is a candidate only if it belongs to the **same tag
family**:

1. Identical prefix, case-sensitive. `main-mongod8.3` never matches
   `main-operator9.0`; `v1.2.3` never matches `1.3.0`.
2. Same number of version components. `8.3` never becomes `8.4.1`.
3. Identical variant identifiers once build counters are ignored.
   `-alpine` never matches `-bookworm` or no suffix; `-eksbuild.4` matches
   `-eksbuild.9` but not `-fargate.1`; `-arm64` never matches `-amd64`;
   `-jdk17` never matches `-jdk21`.
4. Pre-releases: a stable tag only accepts stable candidates. A
   pre-release tag accepts candidates of its own channel and stable
   candidates (`1.3.0-rc.1` accepts `1.3.0-rc.2` and `1.3.0`, not
   `1.4.0-beta.1`).

Candidates are ordered numerically, never as text: by version
(`1.9.0 < 1.10.0`, `2.9 < 2.10`), then pre-release (a stable version is
newer than its pre-releases), then build counters
(`-eksbuild.4 < -eksbuild.10`). The highest candidate is `latest_tag`. An
update is available when it is higher than the running tag.

### Floating tag rules

For `latest`, `stable` and the other floating tags the exporter compares
digests. It never looks for another tag.

- The running digest comes from the runtime: Docker `RepoDigests` for the
  image's repository, Kubernetes `status.containerStatuses[].imageID`.
- The registry digest is fetched with a `HEAD` request for the tag. Equal
  digests mean no update.
- Multi-platform images: depending on runtime and pull method the runtime
  reports either the **image index** digest or the digest of the
  **platform-specific manifest**. If the tag digest differs and the tag
  points to an image index, the exporter fetches the index and also
  accepts the running digest when it equals one of the platform manifests
  in the index. Different kinds of digests are never compared as if they
  were the same kind, and a match is always a true match.
- No running digest (locally built image, runtime reports only an image
  ID): the comparison cannot be made, so the check is unsuccessful.

## Kubernetes deduplication

1. Only pods in phase `Pending` or `Running` are considered. `Succeeded`
   and `Failed` pods are not running.
2. Inside a pod, a container counts when the kubelet reports an image ID
   for it, i.e. the image was pulled and the container was started. A
   container that exists only in the spec (for example in
   `ImagePullBackOff`) is ignored. Init containers of a live pod are
   included, also after they completed.
3. The image reference is taken from the pod spec, the digest from the
   container status.
4. The owner chain is followed through controller `ownerReferences` to
   the top: `Pod → ReplicaSet → Deployment`, `Pod → Job → CronJob`. A pod
   without controller, and a static pod owned by a `Node`, is a standalone
   `Pod`.
5. Running images are grouped by **(namespace, workload kind, workload
   name, image reference, running digest)**. Ten replicas with one image
   are one group. During a rolling update the old and the new tag are two
   groups, so both stay visible. ReplicaSets owned by a Deployment and
   Jobs owned by a CronJob never appear on their own, and a Job is never
   counted in addition to its pods.

The result is deterministic: groups are sorted, and merging into series
follows the fixed rules in [Label collisions](#label-collisions).

## Failure behaviour

| Situation | Result |
|---|---|
| Registry check fails (timeout, 5xx, DNS, auth, 404, 429) | `container_image_info` = `0`, `latest_tag=""`, no update series |
| Unsupported tag, digest-only reference, missing running digest | Same |
| A previously successful check fails on refresh | The old result is dropped immediately. Nothing stale is shown |
| Failed check | Retried after 1m, 2m, 4m ... capped at `--registry-interval`. Failures are never cached for the full interval |
| Transient network error or 5xx | Up to 3 attempts with exponential backoff and jitter within one check |
| Registry answers `429` | No retry. All checks for that registry pause for 1m, 2m, 4m ... up to 30m |
| Runtime discovery fails | The last inventory is reused for 3 × `--scan-interval`. After that `/metrics` answers `503` |
| Startup, before the first complete scan | `/metrics` answers `503` |
| Container or workload stops | Its series disappear with the next scan |
| One image fails | Only that image is affected |

Why `503` instead of an empty page: an empty `200` response would be
indistinguishable from "nothing is running". With `503` Prometheus records
`up == 0`, and no additional metric is needed.

## Known limitations

- **Two-component tags are treated as SemVer.** `nginx:1.27` is a moving
  tag on Docker Hub. The exporter reports `1.28` as an update but does not
  notice a new patch release behind `1.27`.
- **Major-only tags** such as `redis:7` are digits only and therefore
  floating (digest comparison). `v1` and `17-jdk` are unsupported.
- **Tag ordering trusts the registry.** A tag that parses as a higher
  version in the same family is reported even if it is not a release the
  maintainers intend for general use.
- **Suffix numbers.** In `-alpine3.20` the trailing `20` is a build
  counter, so `-alpine3.21` is the same family. In `-debian-12-r5` both
  `12` and `5` are counters, so `-debian-13-r1` is the same family too.
- **Floating tags on multi-platform images.** When the runtime reported
  the index digest and the index was republished for another platform
  only, an update is reported although this platform's image is unchanged.
- **Floating tags need a registry digest.** Locally built or `docker load`
  images, and Kubernetes runtimes that report only an image ID, cannot be
  compared.
- **Collisions** as described in [Label collisions](#label-collisions).
- **Mirrors and pull-through caches.** The registry named in the image
  reference is queried, not a configured mirror.
- **Large clusters.** Every scan lists all pods, ReplicaSets and Jobs
  (ReplicaSets and Jobs as metadata only). No watch or informer is used,
  to keep RBAC and memory small.
- **Rate limits.** The `Retry-After` header is not evaluated; a fixed
  growing pause is used. Tag listings do not count against Docker Hub's
  pull limit; the manifest `GET` for multi-platform floating tags does.
- **One replica.** Several replicas work but multiply registry requests.
- **Azure Workload Identity** depends on the credential helper library
  (`docker-credential-acr-env`); service principal and managed identity
  are the tested paths in that library.

## Troubleshooting

| Symptom | Likely cause and fix |
|---|---|
| `/metrics` returns `503` | No complete scan yet, or discovery has been failing for more than 3 scan intervals. Check the log for `runtime discovery failed` |
| `permission denied` on `docker.sock` | The container user is not in the socket's group. Use `--group-add`, or the socket proxy |
| Kubernetes: `pods is forbidden` | `rbac.yaml` not applied, or the ServiceAccount namespace in the ClusterRoleBinding does not match |
| `container_image_info` is `0`, log says `authentication failed` | No credentials for a private registry. See [authentication](#supported-registries-and-authentication) |
| Log says `rate limited` | Authenticate against the registry, or raise `--registry-interval` |
| Log says `unsupported tag` | The tag format is not SemVer or floating. This is intentional |
| Log says `runtime does not expose a registry digest` | Floating tag on a locally built image, or the runtime reports only an image ID |
| `latest_tag` is not what you expect | Run with `--log-level=debug`, then compare the tag with the [family rules](#semver-rules) |
| Update series missing for a workload | One of the images sharing its label set could not be checked |
| The Prometheus `namespace` label is the exporter's namespace | Set `honor_labels: true` (`honorLabels` in a ServiceMonitor) |

## Security considerations

- **Read-only.** Docker: `GET` requests only. Kubernetes: `list` only,
  plus `get` on Secrets when imagePullSecrets are enabled. Registries:
  `GET` and `HEAD` only. Tests assert this for all three.
- **Docker socket.** Access to `docker.sock` is root on the host, also
  when mounted `:ro`. Use the socket proxy.
- **Kubernetes Secrets.** `--kubernetes-image-pull-secrets` requires read
  access to Secrets, which RBAC cannot limit to pull secrets. It is off by
  default and has its own manifest. Prefer workload identity.
- **Credentials** are held in memory only and never logged. Registry
  error messages contain the registry URL but no authorization data. The
  container image sets `AWS_ECR_DISABLE_CACHE=true` so ECR tokens are not
  written to disk.
- **Container hardening.** Non-root user, read-only root filesystem, all
  capabilities dropped, no privilege escalation, seccomp `RuntimeDefault`,
  distroless base image without a shell.
- **Metrics endpoint.** Unauthenticated and unencrypted, as usual for
  exporters. It reveals image names and versions; restrict access with a
  NetworkPolicy or bind it to localhost.
- **Outbound traffic.** Only the Docker or Kubernetes API and the
  registries of the running images are contacted.

## Development

```bash
make lint
```

```bash
make test
```

```bash
make image
```

```bash
make deb VERSION=0.1.0
```

Tests need no network access beyond localhost, no Docker daemon, no
cluster and no credentials. `tests/` contains end-to-end tests that run
the real Docker SDK client and the real registry client against local
fakes.

Contributor notes: [AGENTS.md](AGENTS.md).
