# Barq Roadmap and Known Limitations

This document is the honest list. It says what Barq does not do, what
it does imperfectly, and what the next increments are likely to be.

## 1. Not implemented

### 1.1 Pipeline features

| Feature | Why it is missing | What it would take |
|---|---|---|
| Cron with a pipeline from the repository | The scheduler fires an entry, but the definition is only fetched when the entry names a repository | A repository lookup in the forge layer |
| Matrix `exclude` and `include` | Only the Cartesian product is supported | Extra keys on `MatrixSpec` and a filter in the compiler |
| Step retry on non-preemption failures | Only preemption is retried | A `retry:` key with an attempt count and a backoff |
| Clone step | The source is expected to be in the workspace already | A `clone` step that runs `git` in the plugin runtime |
| Netrc credential injection | The clone step does not exist | A credential provider that writes a netrc file |
| Per-step `services` networking | The sidecars share the Pod network, but there is no service discovery | A headless service per workflow and a DNS search domain |
| Artifact publishing | The workspace is preserved but not published | A `barq://artifact` step that uploads to S3 directly |

### 1.2 Platform features

| Feature | Status |
|---|---|
| Prometheus metrics | **Implemented.** `/metrics` serves the exposition format. |
| Dependency health probes | **Implemented.** The monitor runs the probes in the background. |
| Usage analytics | **Implemented.** The statistics cover the success rate, the cache hit rate, the compute and the failure hotspots. |
| Web UI for secrets | The API exists; the UI does not manage secrets yet. |
| Build provenance and signing | Not implemented. Tekton Chains and Sigstore are the reference. |
| A registry for artifacts | Not implemented. |
| Garbage collection of old runs | Not implemented. The object store grows without bound. A `BARQ_RETENTION` setting with a periodic sweep is the obvious fix. |
| Quotas and concurrency limits | **Partially implemented.** The queue enforces the global and the per-group limits. A namespace quota is not implemented. |
| Network policies for workers | The controller's policy ships with the manifests; a worker policy does not. |
| Multi-cluster execution | Not implemented. |
| A queue separate from the run state | The queue is in memory and rebuilt on recovery. |
| Distributed tracing | Not implemented. The metrics are there; the traces are not. |

## 2. Implemented imperfectly

### 2.1 The stale leader window

Lease election gives best-effort single-active scheduling. Between a
leader losing connectivity and its renewal deadline expiring, it may
still act. The window is bounded by `BARQ_LEASE_DURATION` and the
consequences are mitigated by deterministic Pod names and the
`barq.io/attempt` label, but it is not eliminated.

Eliminating it properly requires a fencing token that the object store
rejects from a stale writer. S3 does not offer conditional writes on
arbitrary keys in a way that a stdlib client can use portably.

### 2.2 The local object store is not durable

The `local` backend keeps objects on the controller's own volume. If
the controller Pod is replaced, the objects are gone. This is fine for
development and it is why the controller logs a warning when the
backend is selected. Production must use `s3`.

### 2.3 The simulator is synthetic

The simulator runs commands as local processes. It is useful for
exercising the API, the UI and the reconciliation loop. It is not a
Kubernetes emulator and it does not reproduce admission control,
scheduling, image pulling or eviction.

### 2.4 The Merkle cache includes the image reference

The fingerprint uses the image reference as written in the pipeline. If
you use a mutable tag such as `golang:1.22`, a cache hit may be
served for different image content. Use a digest if that matters.

### 2.5 Secret redaction is literal

Redaction matches exact byte sequences. A secret that a step encodes or
transforms before printing is not redacted. See `docs/security.md`.

### 2.6 The YAML subset is a subset

Anchors, aliases, tags, merge keys, multi-document streams and complex
type inference are rejected rather than partially supported. A
pipeline that uses them fails to parse with a clear error instead of
behaving unexpectedly.

### 2.7 Fan-in conflicts are last-wins

When several upstream steps export the same variable, the last one
wins, in sorted dependency order. This is deterministic but silent.
The plugin SDK recommends namespaced keys.

### 2.8 Log redaction retains a tail

The boundary-safe masker holds back the last few bytes of a log chunk
so that a secret spanning a chunk boundary is caught. The tail is
released on the next chunk or when the run is archived. Live log
viewers can therefore lag by a few bytes. This is the price of not
leaking a credential.

### 2.9 The queue is in memory

The queue is rebuilt on recovery. A run that was queued when the
leader died is re-queued, so nothing is lost, but the ordering within
a concurrency group is not preserved across a failover.

### 2.10 The `on_failure` gate is coarse

A step with `on_failure: true` runs when any upstream step failed. It
does not distinguish between a failed step and a canceled one, and it
does not expose which upstream step failed.

### 2.11 The approval and cron registries are in memory

The approvals and the scheduled entries are process local. A run that
is waiting for an approval is lost if the leader restarts. Both
belong in the object store next to the run state.

## 3. The next increments

In rough priority order.

### 3.1 Garbage collection

A `BARQ_RETENTION` setting, a periodic sweep that deletes run state,
logs, cache records and workspaces older than the retention period, and
a `BARQ_CACHE_RETENTION` for the Merkle cache. A background goroutine
on the leader is sufficient.

### 3.2 A clone step

A `clone` step that fetches the source into the workspace, with
credential injection through `GIT_ASKPASS` so that a token never
appears in a URL or in the process arguments. The plugin runtime image
already has git.

### 3.3 Per-worker network policies

A NetworkPolicy for the worker namespace that allows egress to the
object store and to the package registries, and denies everything
else. The controller's own policy ships with the manifests; the
worker's does not, because the allowed destinations depend on the
registries that the pipelines use.

### 3.4 A queue that survives a restart

The queue is in memory. A run that is queued when the leader dies is
re-queued on recovery, which is correct, but the ordering within a
group is lost. Persisting the queue head would preserve it.

### 3.5 Provenance

Record the image digest, the source commit and the step fingerprints
into an in-toto style statement and sign it with a key Barq manages.
This makes a cached build verifiable.

### 3.6 Distributed tracing

The metrics are there; the traces are not. An OpenTelemetry exporter
would need a dependency, or a very small OTLP exporter over the
standard library's HTTP client.

## 4. Deliberate non-goals

These are not on the roadmap and are unlikely to be.

- **A general workflow engine.** Barq runs CI pipelines. It is not
  Argo and does not try to be.
- **A plugin marketplace.** Plugins are containers. The ecosystem
  problem is solved by the container registry, not by a Barq-specific
  catalogue.
- **A hosted service.** Barq is a self-hosted control plane.
- **Supporting every CI trigger in every forge.** GitHub and GitLab
  are supported. The webhook interface is generic enough that others
  can be added.
- **Replacing the Kubernetes API.** Barq uses it. It does not compete
  with it.

## 5. Stability

Barq is pre-1.0. The following are expected to change:

- The YAML schema may gain keys. Existing keys will not change meaning.
- The REST API paths and payload shapes may change.
- The object layout under the prefix may change. The prefix itself is
  configurable, which is the migration lever.
- The `barq://` builtin plugin set may grow. The `PLUGIN_*` convention
  and the output file format are stable.

The following are expected to remain stable:

- The `75` exit code contract for preemption.
- The `.barq.env` output format and its parsing rules.
- The `PLUGIN_*` environment variable convention.
- The `/usr/local/bin/barq-plugin` entrypoint convention.
