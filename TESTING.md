# Testing Barq

This document shows how to test Barq properly, from a quick local smoke test
to a full end-to-end run on a real Kubernetes cluster.

## Prerequisites

- Go 1.22+ (for local runs)
- Docker + Docker Compose (for the compose test)
- kind + kubectl (for the real-Kubernetes test)

---

## Option 1: Docker Compose (fastest, no cluster)

This starts the Barq controller with a real S3-compatible backend (MinIO).
The controller runs in **simulator mode**, so worker results are synthetic,
but the API, UI, storage, caching and reconciliation loop are all exercised
end to end against real object storage.

### 1. Start the stack

```sh
cd /Users/arief/rea-work/ci/barq
docker compose up -d
```

Wait for both services to be healthy:

```sh
docker compose ps
```

You should see `minio` and `barq-controller` both `Up (healthy)`.

### 2. Submit a pipeline

Create a test pipeline:

```yaml
name: smoke-test
steps:
  - id: build
    image: barq/plugin-runtime:1.0.0
    run:
      - echo "building the project..."
      - go version || true
      - echo "build complete"

  - id: test-shards
    image: barq/plugin-runtime:1.0.0
    depends_on: [build]
    parallelism: 3
    run:
      - echo "running shard $BARQ_NODE_INDEX of $BARQ_NODE_TOTAL"
      - echo "shard $BARQ_NODE_INDEX passed"

  - id: release
    image: alpine:3.20
    depends_on: [test-shards]
    when:
      branch: [main]
    run:
      - echo "release step ran"
```

Submit it:

```sh
curl -sS -X POST http://localhost:8080/api/v1/runs \
  -H 'Content-Type: application/yaml' \
  --data-binary @pipeline.yaml | jq .
```

You'll get back a run with an `id`.

### 3. Watch the run

```sh
RUN_ID=<the-id-from-above>

# Poll the status
curl -sS http://localhost:8080/api/v1/runs/$RUN_ID | jq '.status'

# Stream the logs
curl -sS http://localhost:8080/api/v1/runs/$RUN_ID/logs
```

Open the UI: <http://localhost:8080>

### 4. Verify storage hit MinIO

Open the MinIO console at <http://localhost:9001> (login: `barqadmin` /
`barqadmin-secret`), browse to the `barq` bucket, and confirm objects
appear under `barq/runs/<run-id>/`.

---

## Option 2: Local simulator (no Docker)

Run the controller directly on your machine:

```sh
cd /Users/arief/rea-work/ci/barq

# Use a writable temp directory so there are no permission errors
export BARQ_HOT_ROOT=/tmp/barq-hot
export BARQ_LOCAL_OBJECT_ROOT=/tmp/barq-objects
export BARQ_MODE=simulator
export BARQ_ALLOW_SIGNUP=true

go run ./cmd/barq
```

Then submit a pipeline exactly as in Option 1, but point at
`http://127.0.0.1:8080`.

---

## Option 3: Real Kubernetes (end-to-end with real worker Pods)

This is the production path. The controller spawns real Pods in a kind
cluster and workers execute real containers.

### 3.1 Create the cluster

```sh
kind create cluster --name barq
```

### 3.2 Build and load images

```sh
cd /Users/arief/rea-work/ci/barq

docker build -t barq:latest .
docker build -t barq/plugin-runtime:1.0.0 plugins/runtime

kind load docker-image barq:latest
kind load docker-image barq/plugin-runtime:1.0.0
```

### 3.3 Deploy the controller

```sh
kubectl apply -f deploy/k8s-manifests.yaml
kubectl -n barq rollout status deploy/barq-controller
kubectl -n barq port-forward svc/barq 8080:8080
```

### 3.4 Configure S3 storage

The controller needs a durable object store. Set up MinIO in the cluster
or use an existing S3 bucket, then restart the controller with the right
env vars:

```sh
kubectl -n barq set env deploy/barq-controller \
  BARQ_MODE=local \
  BARQ_OBJECT_BACKEND=s3 \
  BARQ_S3_ENDPOINT=http://minio:9000 \
  BARQ_S3_BUCKET=barq \
  BARQ_S3_ACCESS_KEY_ID=barqadmin \
  BARQ_S3_SECRET_ACCESS_KEY=barqadmin-secret \
  BARQ_S3_PATH_STYLE=true
```

### 3.5 Run a pipeline

```sh
curl -sS -X POST http://127.0.0.1:8080/api/v1/runs \
  -H 'Content-Type: application/yaml' \
  --data-binary @pipeline.yaml
```

Watch real Pods being created:

```sh
kubectl -n barq get pods -w
```

When the run completes:

```sh
RUN_ID=...
curl -sS http://127.0.0.1:8080/api/v1/runs/$RUN_ID/logs
```

---

## What to verify in each test

| Feature | How to check |
|---------|-------------|
| Pipeline submission | POST returns a run `id` and `200 OK` |
| DAG / parallel steps | Steps with no `depends_on` run in the same wave; `depends_on` steps wait |
| Matrix / sharding | `parallelism: N` creates N shards, each with `$BARQ_NODE_INDEX` |
| Conditions | A step with `when.branch: [main]` skips on other branches |
| Failure gating | `failure: ignore` lets the pipeline continue after a step exits non-zero |
| Caching | Run the same pipeline twice; the second run shows `CACHED` and creates no Pods |
| Secrets | A step listing `secrets: [TOKEN]` gets the value injected; logs are redacted |
| Logs | `GET /api/v1/runs/<id>/logs` returns step output in order |
| UI | Open `http://localhost:8080` and see the SVG DAG + live SSE logs |
| Storage | Objects appear in the S3 bucket under `barq/runs/<run-id>/` |
| gRPC agent | Port `9080` accepts connections from external `barq-agent` processes |

---

## Common issues

### "create local object root: permission denied"

Set `BARQ_HOT_ROOT` and `BARQ_LOCAL_OBJECT_ROOT` to a directory you own
(for example `/tmp/barq-hot` and `/tmp/barq-objects`), or run Docker
Compose which mounts named volumes.

### "production mode requires BARQ_OBJECT_BACKEND=s3"

Simulator and local mode work with the local backend. Production mode
requires S3. Use `BARQ_MODE=simulator` for local testing.

### "no master key configured"

In simulator/local mode this is just a warning — the encrypted secret
vault is disabled. Set `BARQ_MASTER_KEY` to 32 random bytes to enable it
(required for production).

### gRPC agent port

The controller also listens on port `9080` for the gRPC agent protocol.
External `barq-agent` processes connect there. You don't need it for
simulator mode tests.
