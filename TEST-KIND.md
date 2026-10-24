# Barq End-to-End Test on kind

This document contains the exact commands to run Barq on a real Kubernetes
cluster using kind. Copy and run them in your terminal.

## Prerequisites (already verified available)

- kind v0.33.0
- kubectl v1.35.3
- Docker

---

## Step 1: Create the kind cluster

```sh
kind create cluster --name barq --wait 60s
```

Expected output: cluster ready, kubectl context set to `kind-barq`.

---

## Step 2: Build the Barq images

From `/Users/arief/rea-work/ci/barq`:

```sh
docker build -t barq:latest -f Dockerfile .
docker build -t barq/plugin-runtime:1.0.0 -f plugins/runtime/Dockerfile .
```

---

## Step 3: Load images into kind

```sh
kind load docker-image barq:latest --name barq
kind load docker-image barq/plugin-runtime:1.0.0 --name barq
```

---

## Step 4: Create the credentials secret

Generate a master key and apply the secret:

```sh
MASTER_KEY=$(openssl rand -hex 32 | xxd -r -p | base64)
kubectl -n barq create secret generic barq-credentials \
  --from-literal=master-key="$MASTER_KEY" \
  --from-literal=api-token="dev-token-12345" \
  --from-literal=relay-secret="dev-relay-secret-12345" \
  --from-literal=endpoint="" \
  --from-literal=bucket="" \
  --from-literal=region="us-east-1" \
  --from-literal=access-key-id="" \
  --from-literal=secret-access-key="" \
  --from-literal=github-webhook-secret="" \
  --from-literal=github-token="" \
  --from-literal=gitlab-webhook-secret="" \
  --from-literal=gitlab-token=""
```

---

## Step 5: Deploy Barq to kind

```sh
kubectl apply -f deploy/k8s-manifests.yaml
kubectl -n barq rollout status deploy/barq-controller --timeout=120s
```

---

## Step 6: Port-forward the controller

In one terminal:

```sh
kubectl -n barq port-forward svc/barq 8080:8080
```

---

## Step 7: Submit a test pipeline

In another terminal:

```sh
cat > /tmp/pipeline.yaml <<'YAML'
name: smoke-test
steps:
  - id: build
    image: barq/plugin-runtime:1.0.0
    run:
      - echo "building..."
      - echo "build done"

  - id: test
    image: barq/plugin-runtime:1.0.0
    depends_on: [build]
    parallelism: 2
    run:
      - echo "shard $BARQ_NODE_INDEX of $BARQ_NODE_TOTAL"
      - echo "test shard complete"

  - id: release
    image: alpine:3.20
    depends_on: [test]
    when:
      branch: [main]
    run:
      - echo "release step ran"
YAML

curl -sS -X POST http://localhost:8080/api/v1/runs \
  -H 'Authorization: Bearer dev-token-12345' \
  -H 'Content-Type: application/yaml' \
  --data-binary @/tmp/pipeline.yaml | jq .
```

Save the returned `id` as `RUN_ID`.

---

## Step 8: Watch the run

Poll the status:

```sh
RUN_ID=<the-id-from-above>

while true; do
  STATUS=$(curl -sS http://localhost:8080/api/v1/runs/$RUN_ID | jq -r '.status')
  echo "Status: $STATUS"
  if [ "$STATUS" != "RUNNING" ] && [ "$STATUS" != "PENDING" ]; then
    break
  fi
  sleep 3
done
```

View logs:

```sh
curl -sS http://localhost:8080/api/v1/runs/$RUN_ID/logs
```

Watch real worker Pods:

```sh
kubectl -n barq get pods -w -l app.kubernetes.io/name=barq
```

---

## Step 9: Open the UI

```sh
open http://localhost:8080
```

You'll see the embedded SVG DAG UI with wave columns and live SSE logs.

---

## Step 10: Verify S3 storage (optional)

To test with S3 instead of the local backend, create a MinIO deployment or
point BARQ_S3_ENDPOINT at an existing S3-compatible store, then restart the
controller with:

```sh
kubectl -n barq set env deploy/barq-controller \
  BARQ_MODE=local \
  BARQ_OBJECT_BACKEND=s3 \
  BARQ_S3_ENDPOINT=http://your-s3:9000 \
  BARQ_S3_BUCKET=barq \
  BARQ_S3_ACCESS_KEY_ID=... \
  BARQ_S3_SECRET_ACCESS_KEY=... \
  BARQ_S3_PATH_STYLE=true
```

Objects appear under `barq/runs/<run-id>/` in your bucket.

---

## Cleanup

```sh
kind delete cluster --name barq
```

---

## What you'll see working

| Feature | Evidence |
|---------|----------|
| Real worker Pods | `kubectl get pods -n barq` shows `barq-*` Pods per step |
| DAG execution | `build` in wave 0, `test-shard-0/1` in wave 1 (parallel) |
| Matrix/sharding | `$BARQ_NODE_INDEX` and `$BARQ_NODE_TOTAL` set per shard |
| Conditions | `release` step only runs when `when.branch: [main]` matches |
| Logs | `GET /api/v1/runs/<id>/logs` returns ordered step output |
| UI | SVG DAG at `http://localhost:8080` |
| Storage | Run state and logs in the `emptyDir` hot tier (local mode) |
