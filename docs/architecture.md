# Barq Architecture

Barq is a Kubernetes-native CI and DAG execution engine written in Go
with an embedded interactive SVG web UI. It is a single static binary
with **zero external Go dependencies**.

## 1. Goals and non-goals

### Goals

| Goal | How it is met |
|---|---|
| No CRDs | Barq talks to the Kubernetes REST API directly and stores coordination state in a native `Lease` |
| No external database | Run state, archives, cache records and workspaces live in an S3 compatible object store |
| Fast failure recovery | A preempted worker checkpoints its workspace and exits `75`; the controller reschedules it |
| No wasted compute | A Merkle content-addressed cache skips Pod creation entirely on a hit |
| Small footprint | One binary, an embedded UI, a distroless base image |
| Real identity | Local accounts, OIDC single sign-on, scoped API tokens, role based access control |
| Local development that actually works | A `local` mode with an embedded object store and a `simulator` mode that needs no cluster |
| Configuration as code | A declarative YAML document applied with `barqctl apply` |

### Non-goals

Barq is **not** a general workflow engine. It does not implement
Argo-style dynamic fan-out at runtime, a distributed queue, a
multi-cluster execution model, or an artifact registry. Those are
deliberate scope decisions, not omissions. See `docs/roadmap.md`.

## 2. Component map

```text
                    ┌──────────────────────────────┐
   Browser ────────▶│  Control plane (HTTP + SSE)  │
                    │  internal/controller          │
                    └───────────────┬──────────────┘
                                    │
                        ┌───────────▼───────────┐
                        │  Leader election      │
                        │  coordination.k8s.io  │
                        │  Lease + CAS          │
                        └───────────┬───────────┘
                                    │  only the leader schedules
              ┌─────────────────────▼─────────────────────┐
              │  Engine                                   │
              │  · YAML parse   (internal/yaml)           │
              │  · DAG compile (internal/dag)             │
              │  · Merkle cache (internal/cache)          │
              │  · Alerts         (internal/alert)        │
              │  · Health         (internal/health)       │
              │  · Statistics     (internal/stats)        │
              │  · Metrics        (internal/metrics)      │
              └───────┬───────────────────────┬───────────┘
                      │                       │
        ┌─────────────▼──────────┐  ┌─────────▼──────────┐
        │ Admission control      │  │ Execution plane    │
        │ internal/queue         │  │ internal/runner    │
        │ · pending              │  │ internal/k8s       │
        │ · waiting on deps      │  │ · REST client      │
        │ · running              │  │ · worker script    │
        │ · concurrency groups   │  │ · simulator        │
        └────────────────────────┘  └─────────┬──────────┘
                                             │
                    ┌────────────────────────▼───────────────┐
                    │  Object plane (internal/storage)       │
                    │  · S3 (SigV4) · local · tiered · vault  │
                    └────────────────────────┬───────────────┘
                                             │
                    ┌────────────────────────▼───────────────┐
                    │  Kubernetes                           │
                    │  Worker Pods: barq.io/run-id, step-id  │
                    └────────────────────────────────────────┘

                    ┌──────────────────────────────┐
   GitHub / GitLab▶│  Webhooks (internal/forge)    │
   Cron scheduler▶ │  Approvals  (internal/cron)   │
                    └──────────────────────────────┘
```

## 3. Execution model

### 3.0 The subsystems

Barq is built from subsystems that each own one concern. The
boundaries matter more than the code inside them.

| Package | Responsibility | Concurrency |
|---|---|---|
| `internal/queue` | Admission control: which runs may execute | One mutex, one background process loop, one channel per worker |
| `internal/runner` | The lifecycle of one step | One goroutine per step, a `sync.WaitGroup` for the log drain |
| `internal/dag` | The compiled plan | Pure functions, no shared state |
| `internal/k8s` | The execution backend | The HTTP client is safe for concurrent use |
| `internal/storage` | The object plane and the hot tier | One mutex per run for the log stream |
| `internal/controller` | The reconciler and the API | A mutex per run, a lock-free read path for the snapshots |
| `internal/errgroup` | Concurrent execution with error collection | A `sync.WaitGroup` and a cancellation cause |

The rule is that a subsystem never reaches into another one's state.
The queue decides *whether* a run may run, the runner decides *how* a
step runs, and the reconciler decides *what* runs next.

### 3.1 Pipeline definition

A pipeline is a `.barq.yaml` document. The parser supports a **strict
subset** of YAML: block mappings, block sequences, flow collections,
quoted and plain scalars, block scalars (`|` and `>`), and comments.
Anchors, aliases, tags, merge keys and multi-document streams are
**rejected** rather than misinterpreted. JSON is accepted because JSON
is a subset of YAML.

```yaml
name: release
git_commit_tree: "9f8e7d6c"
env:
  GOFLAGS: -mod=readonly

when:
  branch: ["main", "release/*"]

steps:
  - id: build
    image: golang:1.22-alpine
    matrix:
      GO_VERSION: ["1.22", "1.23"]
      ARCH: [amd64, arm64]
    resources:
      requests: {cpu: "500m", memory: "512Mi"}
      limits:   {cpu: "2",    memory: "2Gi"}
    run:
      - go build ./...

  - id: test
    image: golang:1.22-alpine
    depends_on: [build]
    parallelism: 4
    max_parallel: 2
    run: go test ./...

  - id: publish
    image: golang:1.22-alpine
    depends_on: [test]
    when:
      status: ["success"]
    secrets: [NPM_TOKEN]
    run: npm publish

  - id: notify
    plugin: barq://slack
    depends_on: [test]
    with:
      channel: "#ci-builds"
      webhook_url: "https://hooks.example.com/..."
```

Unknown fields are rejected. A typo in a configuration key is a
configuration error, not a silent default.

### 3.1.1 Step options

| Key | Meaning |
|---|---|
| `image` | The container image of the worker |
| `run` / `commands` | The shell commands the step executes |
| `settings` | Plugin inputs, injected as `PLUGIN_<UPPER_SNAKE_CASE>` |
| `environment` | Plain environment variables of the worker |
| `env` | An alias for `environment` |
| `with` | Plugin inputs, an alias for `settings` |
| `depends_on` | The steps that must complete first |
| `depends_on_optional` | Dependencies that may be missing |
| `matrix` | The dimensions to expand the step into |
| `parallelism` | The number of shards to split the step into |
| `max_parallel` | The number of shards that may run at the same time |
| `resources` | The CPU and memory requests and limits |
| `service_account` | The service account the worker runs as |
| `node_selector` | The nodes the worker may run on |
| `tolerations` | The taints the worker tolerates |
| `timeout_seconds` | The maximum runtime of the step |
| `secrets` | The secrets to inject into the worker |
| `cache` | Whether the step may be served from the cache |
| `no_workspace` | Whether the workspace is captured |
| `when` | The conditions under which the step runs |
| `failure` | `fail`, `ignore` or `cancel` |
| `on_success` | Run only when the upstream steps succeeded |
| `on_failure` | Run only when an upstream step failed |
| `detached` | Run in the background without blocking the pipeline |
| `services` | Sidecar containers that run alongside the worker |

### 3.1.2 Pipeline options

| Key | Meaning |
|---|---|
| `name` | The name of the pipeline |
| `git_commit_tree` / `git_commit` | The source revision, used in the cache fingerprint |
| `env` | Environment variables for every step |
| `secrets` | Secrets injected into every step |
| `when` | The conditions under which the pipeline runs at all |
| `group` | The concurrency group of the pipeline |
| `max_concurrent` | The number of runs of the pipeline that may execute at once |
| `services` | Sidecar containers that run alongside every step |
| `clone` | The step that fetches the source |
| `skip_clone` | Disables the automatic clone step |

### 3.2 Compilation

`internal/dag` turns a `PipelineSpec` into a `Compiled` graph:

1. **Condition filtering** — a step whose `when` block does not match
   is removed from the plan entirely. The plan is therefore exactly
   the work that needs to happen, rather than a plan with branches
   that are decided at runtime.
2. **Matrix expansion** — every step with a `matrix` becomes the
   Cartesian product of its dimensions. Matrix values are injected as
   environment variables and folded into the generated step ID.
3. **Sharding** — a step with `parallelism: N` becomes `N` independent
   nodes. `BARQ_NODE_INDEX` and `BARQ_NODE_TOTAL` are injected so the
   step can split its work. `max_parallel` caps how many shards run at
   once.
4. **Plugin translation** — `barq://name` resolves to a command in the
   plugin runtime image. A custom OCI plugin resolves to
   `/usr/local/bin/barq-plugin`. `with:` keys become
   `PLUGIN_<UPPER_SNAKE_CASE>` environment variables.
5. **Dependency rewriting** — a dependency on a matrix step expands to
   *all* of its variants, and a dependency on a sharded step expands to
   *all* of its shards, so a fan-in waits for every branch.
6. **Wave assignment** — Kahn's topological sort groups independent
   nodes into concurrent execution waves. A cycle is a hard error that
   names the unresolved nodes.

Generated identifiers are bounded to 63 characters, the Kubernetes label
limit, with a deterministic hash suffix to preserve uniqueness.

### 3.3 Conditions

A `when` block is a set of constraints. The constraints within one
block are combined with **AND**. Several blocks are combined with
**OR**.

| Constraint | Matches |
|---|---|
| `event` | The trigger: `push`, `pull_request`, `tag`, `release`, `manual` |
| `branch` | The branch, with a trailing `*` glob |
| `ref` | The git reference, with a trailing `*` glob |
| `repo` | The repository, with a trailing `*` glob |
| `instance` | The forge instance |
| `status` | `success` or `failure` of the upstream steps |
| `matrix` | The value of a matrix axis |
| `path` | A changed path, with a trailing `*` glob |
| `local` | Whether the run is executing locally |

The status condition is deliberately asymmetric. A step with no status
condition runs when the upstream steps succeeded, because success is
the default expectation. A step that must run on failure has to say so
explicitly.

### 3.4 Dispatch

The leader reconciles every active run on a fixed interval:

1. List the Pods labelled `barq.io/run-id=<run>`.
2. **Adopt** any Pod that the current state does not know about. This
   is what makes leader failover transparent: the worker keeps running
   and the new leader picks it up.
3. **Harvest** terminated Pods: capture the exit code, record the
   result, wait for the log stream to reach EOF, then delete the Pod.
4. **Dispatch** every queued step whose dependencies are all
   `SUCCESS` or `CACHED`.
5. **Finalize** the run when nothing is active.

Pod names are deterministic: `barq-<run>-<step>-a<attempt>`. If a stale
leader created a Pod with the same name, the new leader adopts it
rather than creating a duplicate. This is the primary defence against
double execution during a failover.

## 4. Admission control

The queue is the point at which the whole system decides how much work
runs at once.

### 4.1 The three collections

The queue holds every run in exactly one of three places:

- **Pending** — the run could start now.
- **Waiting on dependencies** — the run is blocked because a
  dependency has not finished.
- **Running** — the run was admitted and is executing.

A single mutex guards all three. A background goroutine ticks every
100 milliseconds and rearranges them: it reclaims the expired runs,
promotes the waiting ones whose dependencies are satisfied, and assigns
the pending ones to the workers.

### 4.2 Concurrency groups

A pipeline declares a `group` and a `max_concurrent`. The runs in a
group share a budget, so a single repository cannot consume the whole
cluster. The slots in a group are granted in creation order, so an
earlier pipeline keeps its place even when a later one becomes ready
first. The reservation only applies across pipelines, because
dependencies never cross pipelines and reserving a slot across them
could deadlock.

The global limit, `BARQ_MAX_CONCURRENT_RUNS`, applies to the runs that
do not declare their own group.

### 4.3 Reclaiming

A run that is executing holds a lease. The owner extends the lease
while it works. A run whose lease expires is returned to the front of
the pending list rather than failed, because the loss of a controller
must not fail the build.

### 4.4 Admission

The controller polls the queue as a worker. When the queue hands it a
run, the run is *admitted*: its steps may now start. When the run
reaches a terminal state, the slot is released.

This means the reconciler never starts a step of a run that the queue
has not released, which is what makes the limits effective.

## 5. The step lifecycle

The runner owns the lifecycle of one step. The ordering is deliberate.

```text
  StartStep ──▶ TailStep ──▶ drain the logs ──▶ WaitStep ──▶ DestroyStep
      │             │                              │             │
      │             └── the stream is attached      │             └── the worker
      │                 before the exit code is      │                 is removed
      │                 awaited, because a backend  └── the exit code and
      │                 may close the stream           the timings
      └── the worker is created
```

Two contexts are involved. The **work context** controls the
execution. The **cleanup context** is used to remove the worker, so
that a canceled workflow can still reach the backend. Using the work
context for the cleanup would leave the worker behind every time a
run was canceled.

A step that fails is a *successfully executed* step from the runner's
point of view. The failure is reported through the step state, not as
a runtime error, because the pipeline decides what a failure means:
`failure: ignore` lets the pipeline continue and `failure: cancel`
stops it.

## 6. Identity and access

Barq has two distinct credential types, and the distinction matters.

### 6.1 Session cookies

A session is an interactive credential. It is stored in an `HttpOnly`
cookie with `SameSite=Lax`, and it is issued by a successful sign-in
or sign-up.

A state changing request that uses a session cookie must present a
CSRF token in the `X-Barq-CSRF` header. The token is issued alongside
the session cookie in a separate, readable cookie. A cross site form
post therefore cannot reuse the session.

### 6.2 API tokens

An API token is a programmatic credential. It is presented as a bearer
token, it carries explicit scopes (`read`, `write`, `admin`), and it
does **not** need a CSRF token, because a browser never attaches a
bearer token automatically.

The plaintext token is shown to the user exactly once. Only a keyed
digest of it is stored, so a leak of the object store does not leak
usable credentials. A token can be revoked individually, and every
token of a disabled account is revoked with it.

### 6.3 Roles

| Role | Can |
|---|---|
| `viewer` | Read runs, logs and statistics |
| `builder` | Create, stop and rebuild runs |
| `admin` | Everything, including users, secrets, alerts and configuration |

The first account created on a fresh installation becomes an
administrator, so a new deployment can be bootstrapped without an
out-of-band secret.

### 6.4 Single sign-on

OIDC is supported through the authorization code flow with PKCE. The
ID token signature is verified against the issuer's published key set,
and the algorithm is **pinned** rather than taken from the token, which
is the classic JWT confusion vulnerability.

An account is matched on the issuer and subject, never on the email
address, because the email address is only a claim and the subject is
the stable identifier. A domain allow list restricts which accounts
may sign in.

## 7. High availability

### 7.1 Leader election

Replicas coordinate through a native `coordination.k8s.io/v1` Lease.

- The `holderIdentity` is a **stable** identity: the Pod UID. It is
  never a URL or an IP address, because the holder identity must not
  change when a Pod is rescheduled.
- The current leader's endpoint is stored in the Lease **annotation**
  `barq.io/leader-url`, populated from the downward API.
- Every update carries the `metadata.resourceVersion` that was read
  from the API server. The API server rejects a stale update with
  `409 Conflict`, which is the compare-and-swap that prevents split
  brain. There is no separate "read then write" window.
- A lease whose `renewTime` is older than the lease duration may be
  taken over, incrementing `leaseTransitions`.

### 7.2 Follower behaviour

Only the leader schedules. Followers **proxy** every authenticated
control-plane request to the leader's recorded URL. This means a
Service load balancer can round-robin across replicas without a run
ever appearing to vanish, and without splitting state.

The health endpoint is answered locally by each replica so the UI can
show whether it is talking to the leader or a follower.

### 7.3 Bounded stale-leader window

Lease election provides *best-effort* single-active scheduling, not
linearizable consensus. Between a leader losing network connectivity
and its renewal deadline expiring, that leader may still believe it
holds the lease. This window is bounded by `BARQ_LEASE_DURATION`.

The consequences are handled explicitly:

- Deterministic Pod names mean a stale leader and a new leader
  cannot both create a worker for the same step and attempt.
- The `barq.io/attempt` label means a superseded worker is recognised
  and deleted rather than harvested.
- Durable state lives in the object store, so a new leader recovers
  the full run graph rather than inferring it.

### 7.4 Recovery

When a replica becomes leader it loads every persisted run from the
object store. A run whose state is `RUNNING` is reconciled against the
live Pods: running workers are adopted, finished workers are
harvested, and queued steps resume.

## 8. Storage

### 8.1 Two tiers

| Tier | Contents | Location | Lifetime |
|---|---|---|---|
| Hot | Active run state, streaming log buffer | Local volume | Controller Pod lifetime |
| Cold | Run state, gzipped logs, Merkle records, workspaces | Object store | Durable |

Hot state is written with a temporary file and an atomic rename, so a
crash never leaves a half-written state document. The durable copy is
written on every state transition, which is what makes failover
possible without a shared file system.

### 8.2 Object store backends

`internal/storage` defines an `ObjectStore` interface with two
implementations:

- **`S3`** — AWS S3, MinIO, Cloudflare R2 and other S3 compatible
  services. SigV4 is implemented from scratch with the standard
  library. Path style and virtual host addressing are both supported.
- **`Local`** — a filesystem store for local development. It is
  **leader local, not durable across controller restarts, and not S3
  compatible**. It exists so that a developer does not have to run
  MinIO before they can try Barq. The controller logs a warning when
  it is selected.

The two are not interchangeable. Production requires `S3`.

### 8.3 Workspace relay

Workers never receive object store credentials. The controller issues
short-lived, method-scoped, object-scoped pre-signed URLs:

- A `GET` URL for each upstream workspace the step depends on.
- A `PUT` URL for the workspace the step produces.

With the `S3` backend these are native SigV4 pre-signed URLs. With the
`Local` backend they are HMAC capabilities served by the controller's
own `/_objects/{key...}` endpoint, scoped to the method, the key and
an expiry.

The local relay **rejects** unsigned requests, requests with a wrong
signature, expired requests, requests using the wrong method, and keys
that attempt path traversal.

## 9. Caching

Before a Pod is created, Barq computes a SHA-256 Merkle fingerprint
over:

- the image reference,
- the commands,
- the sorted environment,
- the source tree identifier,
- the sorted fingerprints of the upstream steps,
- the versions of the secrets in use.

Every field is length-prefixed before hashing so that no two different
inputs can produce the same byte stream.

A cache hit skips Pod creation entirely — no compute, no container
pull. The cache record also stores the **workspace object key** from
the original run, so a downstream step still receives the artifacts and
the `.barq.env` outputs of the cached step. A record whose workspace
has been garbage collected is treated as a miss rather than a dangling
reference.

Caching is enabled by default for ordinary steps and disabled for
plugins, because a notification is not idempotent. Use `cache: false`
on a side-effecting step.

## 10. Spot preemption

The worker entrypoint is a POSIX shell script that:

1. Downloads the upstream workspaces and extracts them.
2. Parses `/workspace/.barq.env` **as data** and exports the values.
3. Runs each command as a supervised child process.
4. Traps `SIGTERM` and `SIGINT`.

On `SIGTERM` the trap kills the child, uploads the workspace to the
pre-signed `PUT` URL, and exits with **75** (`EX_TEMPFAIL`). The
controller interprets `75` as *reschedule me*, not *fail me*: the step
is re-queued with an incremented attempt and the checkpoint is used as
the new download source.

A step that exceeds `BARQ_MAX_ATTEMPTS` preemptions fails the run.

The `SIGTERM` handler is best-effort. Kubernetes provides a termination
grace period, and the workspace upload may not complete if the node is
lost entirely. In that case the step restarts from its last completed
upstream state.

## 11. Secret handling

### 11.1 The encrypted secret store

Secret values are stored in the object store encrypted with AES-256-GCM.
The master key is supplied from outside the secret database, through
`BARQ_MASTER_KEY` or `BARQ_MASTER_KEY_FILE`, and is required in
production mode.

- The whole database is encrypted as one authenticated blob.
- Tampering with the ciphertext fails authentication and the read is
  rejected.
- The `GET /api/v1/secrets` endpoint returns **metadata only**: name,
  version, timestamps and a truncated fingerprint. It never returns a
  value.
- A step requests a secret by name through `secrets: [NAME]`. The
  controller injects the value into the worker environment.

### 11.2 Log redaction

Secret values are scrubbed **before** they reach disk, the object store
or an SSE subscriber. The masker is boundary safe: it retains the
longest possible partial secret so that a secret split across two log
chunks is still redacted. This is the difference between a masker that
works in production and one that leaks a credential on exactly the
wrong chunk boundary.

Redaction is literal. It matches exact byte sequences. It does not
detect base64, hex or otherwise transformed secret material, and it
does not prevent a step from deliberately exfiltrating a secret. Run
untrusted code in an untrusted pipeline only.

### 11.3 Authentication

The control-plane API requires a bearer token in production mode. The
token is compared in constant time. The UI reads it from
`localStorage` and sends it as `X-Barq-Token`.

Webhook authentication is a **separate** concern. A GitHub HMAC
signature proves the payload came from GitHub; it says nothing about
the human who is allowed to operate the CI API.

Put the controller behind an authenticated ingress or API gateway
before exposing it outside the cluster.

## 12. Webhooks

`POST /api/v1/webhooks/github` and `POST /api/v1/webhooks/gitlab`:

- GitHub is validated with `X-Hub-Signature-256` (`sha256=` plus a
  hex HMAC over the **raw body**), compared in constant time.
- GitLab is validated with the `X-Gitlab-Token` header.
- The body is read with a size limit.
- The delivery identifier is used to reject replays.
- Branch filters are applied through `BARQ_ALLOWED_BRANCHES`.
- The pipeline definition is fetched from the pushed commit through
  the provider API, so the executed definition is the one in the
  commit, not one supplied by the requester.

## 13. Operating modes

| Mode | Execution backend | Object backend | Durability |
|---|---|---|---|
| `production` | Kubernetes | S3 (required) | Durable |
| `local` | Kubernetes | Local or S3 | Local store is not durable |
| `simulator` | In-process | Local | Synthetic; never proves a build succeeded |

The simulator is **explicit**. There is no automatic fallback, because
a controller that silently simulates work is a controller that reports
success for builds that never ran. The simulator banner in the UI and
the `simulator` flag in the health response make the mode obvious.

## 14. Observability

### 14.1 Metrics

The `/metrics` endpoint serves the Prometheus text exposition format.
The registry is implemented in `internal/metrics`, so there is no
client library dependency.

| Metric | Type | Meaning |
|---|---|---|
| `barq_runs_created_total` | Counter | The runs that were created |
| `barq_runs_finished_total` | Counter | The runs that finished, by status |
| `barq_run_duration_seconds` | Histogram | The wall clock duration of a run |
| `barq_steps_started_total` | Counter | The steps that started |
| `barq_steps_finished_total` | Counter | The steps that completed, by status |
| `barq_steps_failed_total` | Counter | The steps that failed |
| `barq_steps_cached_total` | Counter | The steps served from the cache |
| `barq_steps_preempted_total` | Counter | The steps rescheduled after a preemption |
| `barq_step_duration_seconds` | Histogram | The duration of a step |
| `barq_admission_total` | Counter | The runs admitted to execution |
| `barq_queue_depth` | Gauge | The runs waiting to be admitted |
| `barq_queue_running` | Gauge | The runs that are executing |
| `barq_active_runs` | Gauge | The runs that are executing |

The endpoint is deliberately unauthenticated, because a scraper cannot
present a session. It exposes counts and timings, never identifiers.

### 14.2 Health

The health monitor runs a set of probes in the background and reports
the state of every dependency. A probe that fails three times in a
row is unhealthy; before that it is degraded. The overall state is the
worst of the critical probes.

### 14.3 Analytics

The statistics are computed from the persisted run history and cached
for fifteen seconds. They cover the success rate, the cache hit rate,
the compute consumed and saved, the preemption count, the daily run
time series, the slowest steps and the failure hotspots.

## 15. Additional features

### 15.1 Scheduled pipelines

A cron entry names a pipeline, a five field cron expression and a
branch. The scheduler fires the entry when the expression is due and
the branch has a new commit. The expression supports numbers, ranges,
lists, stars and the `*/N` step syntax.

### 15.2 Approvals

A run can be put into the blocked state and wait for a human. An
approved run is released back into the queue, where it waits for a
concurrency slot like any other run. A declined run is canceled.

### 15.3 Alerts

Alerts are delivered to generic webhooks, which is enough to integrate
with Slack, Discord, Teams, PagerDuty and Opsgenie. The payload is
signed with an HMAC when a secret is configured. A destination may
filter by event and by severity. The GitHub integration reports the
run as a commit status, so a pull request shows the result directly.

## 16. Failure matrix

| Failure | Behaviour |
|---|---|
| Controller Pod dies mid-run | Worker Pods keep running. The new leader adopts them from the Pod list, harvests their results and continues. |
| Leader loses the lease | The replica stops scheduling immediately and demotes itself. A standby takes over after the lease expires. |
| Two replicas race to create a worker | Deterministic Pod names mean the second create fails with `AlreadyExists` and the Pod is adopted. |
| Worker Pod is preempted | The worker checkpoints and exits `75`. The controller reschedules from the checkpoint. |
| Node is lost entirely | The Pod disappears. The step is rescheduled on the next reconcile with the same attempt budget. |
| Object store is unreachable | State changes are logged as failures. Scheduling continues; the run state is re-persisted on the next transition. |
| Cache record is corrupt | Treated as a miss. The step re-executes. |
| Cache workspace was deleted | Treated as a miss. The step re-executes. |
| Secret ciphertext is tampered with | The read fails authentication and the secret is unavailable. The step fails with a clear message. |
| Webhook is replayed | The delivery identifier is recognised and the request is acknowledged without starting a run. |
| A step fails | Downstream steps are cancelled. Independent branches are allowed to finish. |
