# Metric contract and label collisions

This note records the analysis of the requested metric schema and the
decisions that follow from it.

## The contract

```text
container_image_info{platform, container, namespace, workload_kind, registry, repository, current_tag, latest_tag}  0 | 1
container_image_update_available{platform, container, namespace, workload_kind, registry, repository}             0 | 1
```

Prometheus identifies a series by its metric name and label values. Two
samples with the same identity in one scrape are an error, so the exporter
must produce at most one sample per label set.

## Where one label set is not one image

The identity of a running image is larger than either label set:

| Dimension | In `info` labels | In `update` labels |
|---|---|---|
| Workload (platform, namespace, kind, name) | yes | yes |
| Repository | yes | yes |
| Tag | yes | **no** |
| Container inside the pod | **no** | **no** |
| Running digest | **no** | **no** |

Cases:

1. **Rolling update.** A Deployment runs `api:1.4.2` and `api:1.5.0` at
   the same time. `info` separates them through `current_tag`. `update`
   has one label set for both.
2. **Two containers, one repository, different tags.** For example an
   init container on `app:1.0.0` and a main container on `app:1.1.0`.
   Same as case 1.
3. **Two containers, same repository and tag.** Both metrics have one
   label set for two images. The registry result is identical for both.
4. **Replicas on one floating tag with different digests.** `app:latest`
   pulled at different times. Both metrics have one label set; the update
   result differs per replica.
5. **Different references that normalize equally.** `nginx:1.27` and
   `docker.io/library/nginx:1.27`. They are the same image and are treated
   as one.

Docker is not affected by cases 1 to 4: a container name is unique and a
container runs one image.

## Decision: conservative aggregation

`metrics.Build` groups observations by label set.

`container_image_info`: value is `1` only when every observation in the
group succeeded. With mixed success `latest_tag` differs (`""` on
failure), so in practice groups are homogeneous.

`container_image_update_available`:

- omitted when any observation in the group is unknown;
- otherwise `1` when at least one observation has an update;
- otherwise `0`.

Reasons:

- `1` for "at least one" answers the operational question "is there
  something to update in this workload". During a rolling update it turns
  to `0` when the last old replica is gone.
- Omitting the series when a member is unknown follows the rule that an
  unknown status is never reported as "no update". A `0` computed from the
  known members alone would be exactly that.
- The result does not depend on discovery order.

## What cannot be expressed

- Which tag has the update, on the `update` metric alone. The join with
  `container_image_info` shows it: the series whose `current_tag` differs
  from `latest_tag`.
- Which container of a pod is affected.

## Smallest backward-compatible extension (not implemented)

Add `current_tag` to `container_image_update_available`. It resolves
cases 1 and 2, keeps existing queries valid when they aggregate with
`max without (current_tag)`, and adds no cardinality beyond what
`container_image_info` already has. It does not resolve cases 3 and 4,
which would need a `container_name` or digest label; the aggregation above
is sufficient for them because the merged images share one registry tag.

The extension is left out because the specification fixes the label set
and asks for explicit approval before the contract changes.

## Unknown inventory

No third metric signals "inventory unknown". Instead `/metrics` answers
`503` before the first complete scan and after discovery has failed for
more than three scan intervals. Prometheus then records `up == 0`, which
cannot be mistaken for an empty inventory.
