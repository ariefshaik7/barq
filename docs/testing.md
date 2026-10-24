# Testing Barq

## 1. The gate

Every change must pass:

```sh
go build ./...
go vet ./...
gofmt -l .
go test ./...
go test -race ./...
```

`gofmt -l .` must print nothing.

## 2. What is covered

| Package | What is tested |
|---|---|
| `internal/yaml` | Full document parsing, unknown field rejection, tab rejection, anchor rejection, duplicate IDs, unknown dependencies, plugin/run conflict, JSON input, block scalars, comments, and a fuzz target |
| `internal/yaml` | The `when` conditions: event, branch glob, status, matrix, multiple blocks as OR, within a block as AND, and the rejection of unknown values |
| `internal/dag` | Cycle detection, wave grouping, matrix expansion, shard cloning, fan-in across matrix variants **and across shards**, builtin plugin translation, custom plugin contract, unknown builtin rejection, identifier length bounding, the condition filtering, the plugin settings, the sidecar services and the failure modes |
| `internal/storage` | Secret redaction including the chunk boundary case, short secret rejection, partial secret retention, atomic state writes, log append and archive, secret vault round trip, ciphertext tamper detection, metadata-only listing, relay authentication (missing, wrong, expired, valid), method scoping, path traversal rejection |
| `internal/cache` | Fingerprint determinism, environment ordering independence, sensitivity to every input, round trip, miss when the workspace is missing |
| `internal/auth` | The first account is an administrator, the password policy, the salt makes the hashes unique, the sign-in timing, the session lifecycle, the disabled account, the role hierarchy, the password change revokes the sessions and the accounts survive a restart |
| `internal/queue` | The push and the poll, the release, the dependency gating, the failure propagation, the concurrency limit, the worker filter, the reclaim of an expired task, the pause and the resume, the statistics and the duplicate rejection |
| `internal/runner` | The successful step, the drain before the wait, the failing step, the out of memory, the cleanup, the cleanup when the log stream fails, the cancellation, the state reporting, the line splitting and the panicking log handler |
| `internal/cron` | The valid and the invalid expressions, the matching, the step syntax, the next occurrence, the store lifecycle and the once-per-minute guarantee |
| `internal/metrics` | The counters, the gauges, the histogram buckets, the label escaping and the determinism of the exposition output |
| `internal/configfile` | The document parsing, the expansion, the unknown role, the duplicate user, the future version and the secret resolution |
| `internal/alert` | The delivery, the event filter, the severity filter, the disabled destination, the signed payload and the retry |
| `internal/controller` | Run creation and persistence, invalid pipeline rejection, successful execution, failure propagation, Spot preemption and reschedule, cancellation, the three rebuild modes, webhook signature verification, API authentication, UI serving, leader election with optimistic concurrency and the takeover of an expired lease |

## 3. Running the tests

```sh
# Everything.
go test ./...

# With the race detector. Always run this before pushing.
go test -race ./...

# Verbose, one package.
go test -v ./internal/controller/

# A single test.
go test -run TestSpotPreemptionReschedulesTheStep -v ./internal/controller/

# Coverage.
go test -coverprofile=coverage.out ./...
go tool cover -func=coverage.out | tail -1
```

## 4. The fuzz target

The YAML parser has a fuzz target because the parser handles untrusted
input:

```sh
go test -fuzz=FuzzParseNeverPanics -fuzztime=60s ./internal/yaml/
```

A crash is a bug. The parser must return an error for anything it does
not understand, never panic.

## 5. End-to-end verification on kind

The unit tests run against the in-process simulator. The following
exercises the real Kubernetes path.

### 5.1 Bring up the cluster

```sh
docker build -t barq:latest .
docker build -t barq/plugin-runtime:1.0.0 plugins/runtime
kind load docker-image barq:latest
kind load docker-image barq/plugin-runtime:1.0.0
kind create cluster --name barq
kubectl apply -f deploy/k8s-manifests.yaml
kubectl -n barq rollout status deploy/barq-controller
```

### 5.2 Run a pipeline

```sh
kubectl -n barq port-forward svc/barq 8080:8080 &

curl -sS -X POST http://127.0.0.1:8080/api/v1/runs \
  -H 'Content-Type: application/yaml' \
  --data-binary @- <<'YAML'
name: e2e
steps:
  - id: build
    image: barq/plugin-runtime:1.0.0
    run:
      - echo "build ok"
  - id: test
    image: barq/plugin-runtime:1.0.0
    depends_on: [build]
    parallelism: 2
    run:
      - echo "shard $BARQ_NODE_INDEX of $BARQ_NODE_TOTAL"
YAML
```

Poll until the run reaches a terminal state:

```sh
RUN_ID=$(curl -sS http://127.0.0.1:8080/api/v1/runs | jq -r '.runs[0].id')
watch -n1 "curl -sS http://127.0.0.1:8080/api/v1/runs/$RUN_ID | jq '.status, (.steps|map({(.id):.status})|add)'"
```

### 5.3 Verify the pieces

```sh
# Pods were created and then removed.
kubectl -n barq get pods -l barq.io/run-id=$RUN_ID

# Logs were captured.
curl -sS http://127.0.0.1:8080/api/v1/runs/$RUN_ID/logs | head -40

# The lease exists and has a holder.
kubectl -n barq get lease barq-leader -o jsonpath='{.spec.holderIdentity}{"\n"}'

# Run state is durable.
kubectl -n barq exec deploy/barq-controller -- ls /var/lib/barq/ebs/runs
```

### 5.4 Verify preemption

```sh
curl -sS -X POST http://127.0.0.1:8080/api/v1/runs \
  -H 'Content-Type: application/yaml' \
  --data-binary @- <<'YAML'
name: preempt
steps:
  - id: flaky
    image: barq/plugin-runtime:1.0.0
    run:
      - SIMULATE_SPOT_PREEMPT
YAML
```

The step should show `SPOT_MIGRATING` at least once and then succeed
on a later attempt.

### 5.5 Verify leader failover

```sh
# Start a long running step.
curl -sS -X POST http://127.0.0.1:8080/api/v1/runs \
  -H 'Content-Type: application/yaml' \
  --data-binary @- <<'YAML'
name: failover
steps:
  - id: slow
    image: barq/plugin-runtime:1.0.0
    run:
      - sleep 300
YAML

# Kill the leader.
LEADER=$(kubectl -n barq get lease barq-leader -o jsonpath='{.spec.holderIdentity}')
kubectl -n barq delete pod -l app.kubernetes.io/name=barq --field-selector=status.podIP="$LEADER_IP"

# A new leader takes over within the lease duration and the worker
# keeps running.
kubectl -n barq get pods -l barq.io/run-id=$RUN_ID
```

### 5.6 Verify the cache

Run the same pipeline twice with an identical definition and an
identical `git_commit_tree`. The second run should report the step as
`CACHED` and should not create a Pod.

```sh
curl -sS http://127.0.0.1:8080/api/v1/runs/$RUN_ID | jq '.steps'
```

## 6. Acceptance criteria for a change

A change is complete when:

1. `go build ./...`, `go vet ./...` and `gofmt -l .` are clean.
2. `go test ./...` and `go test -race ./...` pass.
3. A new behaviour has a test that would fail without the change.
4. A bug fix has a regression test that reproduces the original
   failure.
5. Anything user-visible is documented in `docs/`.

## 7. Testing the Kubernetes client

The client is tested through the `Backend` interface, so the same
controller tests run against the simulator and the real client. To
exercise the real client, run the end-to-end checks above.

The client is deliberately thin. It sends exactly the requests that the
Kubernetes API documents. If a request starts failing, compare the
payload against the API reference before adding a workaround.

## 8. Testing the S3 backend

The S3 client has no unit tests against a live endpoint. To verify it:

```sh
# Run MinIO locally.
docker run -d --name minio -p 9000:9000 -p 9001:9001 \
  -e MINIO_ROOT_USER=barqadmin -e MINIO_ROOT_PASSWORD=barqadmin \
  minio/minio server /data --console-address ":9001"

# Point the controller at it.
export BARQ_MODE=local
export BARQ_OBJECT_BACKEND=s3
export BARQ_S3_ENDPOINT=http://127.0.0.1:9000
export BARQ_S3_BUCKET=barq
export BARQ_S3_ACCESS_KEY_ID=barqadmin
export BARQ_S3_SECRET_ACCESS_KEY=barqadmin
export BARQ_S3_PATH_STYLE=true
export BARQ_HOT_ROOT=$(mktemp -d)
export BARQ_LOCAL_OBJECT_ROOT=$(mktemp -d)

go run ./cmd/barq
```

Create a bucket, then run a pipeline and confirm that objects appear
under `barq/runs/`. This exercises the SigV4 implementation end to
end, including pre-signed URL generation and the `PUT` and `GET`
round trips.
