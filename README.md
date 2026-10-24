# Barq CI

A Kubernetes-native CI and DAG execution engine written in Go, with an
embedded interactive SVG web UI, real identity management, alerting
and configuration as code.

**One static binary. No CRDs. No database. No third-party Go
dependencies.**

```
┌──────────────────────────────────────────────────────────────────────┐
│  Barq                                                                │
│                                                                      │
│  .barq.yaml ──▶ parse ──▶ compile ──▶ wave dispatch ──▶ Pods       │
│                    │         │            │                         │
│                    │         │            ├─▶ Merkle cache (hit?)  │
│                    │         │            ├─▶ Spot checkpoint (75)  │
│                    │         │            └─▶ secret masking        │
│                    │         │                                      │
│                    │         └─▶ SVG graph UI + SSE logs            │
│                    │                                                │
│                    └─▶ config as code ──▶ barqctl apply             │
│                                                                      │
│  Identity: local accounts · OIDC single sign-on · scoped API tokens  │
│  Storage:  S3 / MinIO / R3 cold tier · local hot tier · AES-GCM vault│
│  Alerts:   generic webhooks · GitHub commit status · HMAC signed     │
└──────────────────────────────────────────────────────────────────────┘
```

## Quick start

### Run it without a cluster

```sh
go run ./cmd/barq
```

Open <http://127.0.0.1:8080>. The controller runs in **simulator**
mode: worker results are synthetic. They exercise the API, the UI, the
lease and the reconciliation loop. They never prove that a build
succeeded, and the UI says so.

### Run it on kind

```sh
docker build -t barq:latest .
docker build -t barq/plugin-runtime:1.0.0 plugins/runtime
kind load docker-image barq:latest
kind load docker-image barq/plugin-runtime:1.0.0
kind create cluster --name barq

kubectl apply -f deploy/k8s-manifests.yaml
kubectl -n barq rollout status deploy/barq-controller
kubectl -n barq port-forward svc/barq 8080:8080
```

The first account you create becomes an administrator. Sign up at
<http://127.0.0.1:8080>.

### Run a pipeline

```sh
cat > pipeline.yaml <<'YAML'
name: demo
steps:
  - id: build
    image: barq/plugin-runtime:1.0.0
    matrix:
      GO: ["1.22", "1.23"]
    run:
      - echo "building $GO"

  - id: test
    image: barq/plugin-runtime:1.0.0
    depends_on: [build]
    parallelism: 2
    run:
      - echo "shard $BARQ_NODE_INDEX of $BARQ_NODE_TOTAL"
YAML

curl -sS -X POST http://127.0.0.1:8080/api/v1/runs \
  -H 'Content-Type: application/yaml' \
  --data-binary @pipeline.yaml
```

Or with the CLI:

```sh
barqctl spawn pipeline.yaml
barqctl runs
barqctl logs <run-id>
```

## Features

| | |
|---|---|
| **DAG execution** | `depends_on`, matrix expansion, test sharding, Kahn topological waves, cycle detection |
| **Conditions** | `when` blocks with event, branch, ref, repo, status, matrix and path constraints |
| **Step gating** | `on_success` and `on_failure` gates, `failure: fail\|ignore\|cancel`, detached steps |
| **Sidecars** | Per-step and per-pipeline `services` that run alongside the worker |
| **Caching** | SHA-256 Merkle fingerprints; a hit skips Pod creation *and* restores the original workspace |
| **Spot resilience** | The worker traps `SIGTERM`, checkpoints and exits `75`; the controller reschedules it |
| **Admission control** | A real task queue with concurrency groups, dependency gating and lease expiry |
| **Identity** | Local accounts, OIDC single sign-on, scoped API tokens, three roles, CSRF protection |
| **Secrets** | AES-256-GCM encrypted vault, metadata-only API, boundary-safe log redaction |
| **Configuration as code** | A declarative YAML document applied with `barqctl apply` |
| **Observability** | Dependency health probes, Prometheus metrics, usage analytics, cache hit rate |
| **Alerting** | Generic signed webhooks, GitHub commit status, per-event filters |
| **Scheduling** | Cron entries, manual approvals, per-repository concurrency budgets |
| **UI** | Embedded SVG DAG with wave columns, live SSE logs, dark and light themes, no framework |

## Architecture at a glance

```text
   Browser ──▶ Control plane (HTTP + SSE) ──▶ Leader election (Lease + CAS)
                                                      │
    ┌─────────────────────────────────────────────────▼────────────────────┐
    │  Engine: parse ─▶ compile ─▶ queue ─▶ runner ─▶ worker Pods         │
    │            │         │         │        │                          │
    │            │         │         │        └─▶ logs, exit code, cache  │
    │            │         │         └─▶ admission, concurrency groups   │
    │            │         └─▶ matrix, shards, waves, conditions          │
    │            └─▶ strict YAML subset, no unknown fields                 │
    └──────────────────────────────────────────────────────────────────────┘
                    │                              │
                    ▼                              ▼
            Object store (S3)              Kubernetes API
            state, logs, cache,
            workspaces, secrets
```

The subsystems are independent. The queue decides *whether* a run may
run, the runner decides *how* a step runs, and the reconciler decides
*what* runs next. See [docs/architecture.md](docs/architecture.md).

## Documentation

| Document | Contents |
|---|---|
| [docs/architecture.md](docs/architecture.md) | Components, execution model, identity, HA, storage, caching, failure matrix |
| [docs/plugins.md](docs/plugins.md) | The plugin runtime contract, built-in plugins, writing a plugin |
| [docs/security.md](docs/security.md) | Threat model, authentication, secrets, worker isolation, supply chain |
| [docs/operations.md](docs/operations.md) | Configuration reference, deployment, runbooks, troubleshooting |
| [docs/testing.md](docs/testing.md) | The test gate, what is covered, end-to-end verification |
| [docs/comparisons.md](docs/comparisons.md) | How Barq compares with Drone, Woodpecker, Jenkins and Tekton |
| [docs/roadmap.md](docs/roadmap.md) | What is not implemented, and what is next |

## Configuration

Everything is environment variables. The full reference is in
[docs/operations.md](docs/operations.md).

```sh
export BARQ_MODE=production
export BARQ_OBJECT_BACKEND=s3
export BARQ_S3_BUCKET=my-bucket
export BARQ_S3_ACCESS_KEY_ID=...
export BARQ_S3_SECRET_ACCESS_KEY=...
export BARQ_MASTER_KEY="$(openssl rand -hex 32 | xxd -r -p | base64)"
export BARQ_API_TOKEN="$(openssl rand -hex 32)"
```

Production mode refuses to start without a durable object store, a
master key and an API token.

## Building

```sh
go build ./...
go test ./...
go test -race ./...
go vet ./...
gofmt -l .

docker build -t barq:latest .
docker buildx build --platform linux/amd64,linux/arm64 -t barq:latest .
```

## Layout

```
barq/
├── cmd/
│   ├── barq/            the controller
│   └── barqctl/         the administration CLI
├── internal/
│   ├── alert/           webhook delivery and GitHub commit status
│   ├── apierr/          typed API errors with stable codes
│   ├── auth/            accounts, sessions, API tokens, OIDC
│   ├── cache/           the Merkle content-addressable cache
│   ├── config/          environment configuration
│   ├── configfile/      configuration as code
│   ├── controller/      engine, HTTP API, webhooks, embedded UI
│   ├── cron/            the scheduled pipeline feature
│   ├── dag/             the compiler
│   ├── errgroup/        concurrent execution with error collection
│   ├── forge/           the source control provider abstraction
│   ├── health/          dependency health probes
│   ├── k8s/             the Kubernetes client, worker script, simulator
│   ├── metrics/         the Prometheus exposition registry
│   ├── queue/           the admission control task queue
│   ├── runner/          the step lifecycle
│   ├── secrets/         secret value tracking
│   ├── stats/           usage analytics
│   ├── storage/         object store, tiering, secret vault
│   └── yaml/            the pipeline parser and conditions
├── deploy/              Kubernetes manifests
├── docs/                the documentation
├── examples/            a configuration as code example
└── plugins/runtime/     the plugin runtime image
```

## Licence

Apache 2.0.
