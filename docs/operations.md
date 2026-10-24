# Barq Operations

## 1. Configuration

Barq is configured entirely through environment variables.

### 1.1 Core

| Variable | Default | Meaning |
|---|---|---|
| `BARQ_MODE` | `simulator` | `production`, `local` or `simulator` |
| `BARQ_OBJECT_BACKEND` | `local` | `s3` or `local` |
| `BARQ_NAMESPACE` | from the service account | Namespace workers are created in |
| `BARQ_IDENTITY` | the Pod UID | Stable identity used for lease ownership |
| `BARQ_LEASE_NAME` | `barq-leader` | Name of the coordination Lease |
| `BARQ_LEADER_URL` | empty | Base URL other replicas use to reach this one |
| `BARQ_PORT` | `8080` | HTTP listen port |
| `BARQ_HOT_ROOT` | `/var/lib/barq/ebs` | Local hot tier directory |
| `BARQ_LOCAL_OBJECT_ROOT` | `/var/lib/barq/ebs/objects` | Local object store directory |
| `BARQ_OBJECT_PREFIX` | `barq` | Prefix for every object |
| `BARQ_PUBLIC_URL` | `http://127.0.0.1:8080` | Externally reachable base URL, used by the local relay |
| `BARQ_PLUGIN_RUNTIME_IMAGE` | `barq/plugin-runtime:1.0.0` | Image providing the `barq://` builtins |
| `BARQ_RELAY_SECRET` | generated | HMAC secret for the local object relay |

### 1.2 Identity

| Variable | Default | Meaning |
|---|---|---|
| `BARQ_AUTH_REQUIRED` | `false` | Force authentication on outside production mode |
| `BARQ_ALLOW_SIGNUP` | `false` | Permit local account creation |
| `BARQ_SECURE_COOKIES` | `false` | Mark the session cookie as Secure. Set it behind TLS. |
| `BARQ_SESSION_TTL` | `12h` | How long a session remains valid |
| `BARQ_OIDC_ISSUER` | empty | The OIDC issuer URL. Must use https. |
| `BARQ_OIDC_CLIENT_ID` | empty | The OIDC client identifier |
| `BARQ_OIDC_CLIENT_SECRET` | empty | The OIDC client secret |
| `BARQ_OIDC_REDIRECT_URL` | empty | The OIDC callback URL |
| `BARQ_OIDC_ALLOWED_DOMAINS` | empty | Comma separated domain allow list |

### 1.3 Object store

| Variable | Default | Meaning |
|---|---|---|
| `BARQ_S3_ENDPOINT` | `https://s3.amazonaws.com` | S3 compatible endpoint |
| `BARQ_S3_REGION` | `us-east-1` | Signing region |
| `BARQ_S3_BUCKET` | empty | Bucket name |
| `BARQ_S3_ACCESS_KEY_ID` | empty | Access key |
| `BARQ_S3_SECRET_ACCESS_KEY` | empty | Secret key |
| `BARQ_S3_SESSION_TOKEN` | empty | Session token for temporary credentials |
| `BARQ_S3_PATH_STYLE` | `true` | Use path style addressing |

### 1.4 Security

| Variable | Default | Meaning |
|---|---|---|
| `BARQ_API_TOKEN` | empty | Bearer token for the control plane. **Required in production.** |
| `BARQ_MASTER_KEY` | empty | 32 raw bytes or base64 of 32 bytes. **Required in production.** |
| `BARQ_MASTER_KEY_FILE` | empty | Path to a file containing the master key. |

### 1.5 Webhooks
| Variable | Default | Meaning |
|---|---|---|
| `BARQ_GITHUB_WEBHOOK_SECRET` | empty | HMAC secret for GitHub deliveries |
| `BARQ_GITHUB_API_TOKEN` | empty | Token for the GitHub API |
| `BARQ_GITHUB_API_BASE` | `https://api.github.com` | GitHub API base URL |
| `BARQ_GITLAB_WEBHOOK_TOKEN` | empty | Token for GitLab deliveries |
| `BARQ_GITLAB_API_TOKEN` | empty | Token for the GitLab API |
| `BARQ_GITLAB_API_BASE` | `https://gitlab.com` | GitLab base URL |
| `BARQ_ALLOWED_BRANCHES` | empty | Comma separated branch allow list. Empty allows all. |

### 1.6 Tuning

| Variable | Default | Meaning |
|---|---|---|
| `BARQ_MAX_ATTEMPTS` | `3` | Preemption retries per step |
| `BARQ_POLL_INTERVAL` | `2s` | Reconciliation interval |
| `BARQ_LEASE_DURATION` | `10s` | How long a lease is valid without renewal |
| `BARQ_LEASE_RENEW_TIME` | `3s` | How often the lease is renewed |
| `BARQ_SHUTDOWN_GRACE` | `20s` | Graceful shutdown budget |

## 2. Running locally

### 2.1 Simulator, no cluster

```sh
export BARQ_MODE=simulator
export BARQ_HOT_ROOT="$(mktemp -d)"
export BARQ_LOCAL_OBJECT_ROOT="$(mktemp -d)"
go run ./cmd/barq
```

Open <http://127.0.0.1:8080>. The health badge shows **SIMULATOR**.
Worker results are synthetic. They exercise the API, the UI, the lease
and the reconciliation loop. They never prove that a build succeeded.

Create a run:

```sh
curl -sS -X POST http://127.0.0.1:8080/api/v1/runs \
  -H 'Content-Type: application/yaml' \
  --data-binary @- <<'YAML'
name: demo
steps:
  - id: build
    image: alpine:3.20
    run:
      - echo "hello from barq"
YAML
```

### 2.2 kind

```sh
# 1. Build and load the images.
docker build -t barq:latest .
docker build -t barq/plugin-runtime:1.0.0 plugins/runtime
kind load docker-image barq:latest
kind load docker-image barq/plugin-runtime:1.0.0

# 2. Create the cluster if you do not have one.
kind create cluster --name barq

# 3. Apply the manifests.
kubectl apply -f deploy/k8s-manifests.yaml

# 4. Watch it come up.
kubectl -n barq rollout status deploy/barq-controller
kubectl -n barq logs -f deploy/barq-controller
```

Forward the port and open the UI:

```sh
kubectl -n barq port-forward svc/barq 8080:8080
```

### 2.3 Object store

The local profile needs no object store. The controller stores objects
in `BARQ_LOCAL_OBJECT_ROOT` on the hot volume and serves them through
the signed relay.

For a more realistic setup, run MinIO and switch to the S3 backend:

```sh
kubectl apply -f deploy/minio.yaml
kubectl -n barq create secret generic barq-credentials \
  --from-literal=endpoint=http://minio.minio.svc.cluster.local:9000 \
  --from-literal=bucket=barq \
  --from-literal=region=us-east-1 \
  --from-literal=access-key-id=barqadmin \
  --from-literal=secret-access-key=barqadmin \
  --from-literal=api-token="$(openssl rand -hex 32)" \
  --from-literal=master-key="$(openssl rand -hex 32 | xxd -r -p | base64)" \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl -n barq patch configmap barq-config \
  --type merge -p '{"data":{"BARQ_MODE":"local","BARQ_OBJECT_BACKEND":"s3"}}'
kubectl -n barq rollout restart deploy/barq-controller
```

Create the bucket:

```sh
kubectl -n barq run mc --rm -it --restart=Never \
  --image=minio/mc -- \
  /bin/sh -c "mc alias set local http://minio.minio.svc.cluster.local:9000 barqadmin barqadmin && mc mb local/barq"
```

## 3. Running in production

### 3.1 Required changes to the manifests

1. **Use the S3 backend.** Set `BARQ_OBJECT_BACKEND=s3` and supply the
   S3 credentials.
2. **Scale the controller.** Set `replicas: 2` or more. The lease
   guarantees a single active scheduler and the durable object store
   guarantees a single source of truth.
3. **Replace the hot tier.** `emptyDir` is lost when the Pod is
   replaced. Mount a per-replica EBS volume. Use a generic ephemeral
   volume so each replica gets its own volume, or a StatefulSet with a
   `volumeClaimTemplate` if you want the volumes to be stable.
4. **Set the API token and the master key.** Both are required in
   production mode and the controller refuses to start without them.
5. **Pin the plugin runtime image by digest.**

### 3.2 A production hot tier

```yaml
      volumes:
        - name: hot
          ephemeral:
            volumeClaimTemplate:
              spec:
                accessModes: ["ReadWriteOnce"]
                storageClassName: gp3
                resources:
                  requests:
                    storage: 10Gi
```

A generic ephemeral volume gives every replica its own volume, which
avoids the `ReadWriteOnce` multi-attach problem entirely. The hot tier
is a cache in front of the durable tier, so losing it is survivable:
the controller reloads run state from the object store.

### 3.3 Storage layout

```text
s3://<bucket>/barq/
├── runs/<run-id>/state.json              # run state, written on every transition
├── runs/<run-id>/logs.txt.gz             # archived log, written when the run finishes
├── runs/<run-id>/workspaces/<step>-a<n>.tar.gz
├── merkle-cache/<fingerprint>.json       # cache records
└── config/secrets.v1                     # encrypted secret database
```

## 4. Day-to-day operations

### 4.1 Health

```sh
kubectl -n barq get --raw "/api/v1/health" 2>/dev/null || \
  kubectl -n barq port-forward svc/barq 8080:8080 &
curl -sS http://127.0.0.1:8080/api/v1/health | jq
```

```json
{
  "healthy": true,
  "leader": true,
  "identity": "8f1c...",
  "leader_url": "http://10.244.0.12:8080",
  "lease_holder": "8f1c...",
  "backend": "kubernetes",
  "object_backend": "s3",
  "simulator": false
}
```

### 4.2 Inspecting the lease

```sh
kubectl -n barq get lease barq-leader -o yaml
```

Check `spec.holderIdentity`, `spec.renewTime` and the
`barq.io/leader-url` annotation. A `renewTime` that is older than
`BARQ_LEASE_DURATION` means the leader is unhealthy.

### 4.3 Inspecting workers

```sh
kubectl -n barq get pods -l barq.io/run-id=<run-id> -o wide
kubectl -n barq logs <pod-name> -f
```

### 4.4 Common tasks

| Task | Command |
|---|---|
| List runs | `curl -sS -H "X-Barq-Token: $TOKEN" http://127.0.0.1:8080/api/v1/runs \| jq` |
| Get one run | `curl -sS -H "X-Barq-Token: $TOKEN" http://127.0.0.1:8080/api/v1/runs/$RUN_ID \| jq` |
| Stream logs | `curl -N -H "X-Barq-Token: $TOKEN" http://127.0.0.1:8080/api/v1/runs/$RUN_ID/logs` |
| Stop a run | `curl -X POST -H "X-Barq-Token: $TOKEN" http://127.0.0.1:8080/api/v1/runs/$RUN_ID/stop` |
| Rebuild everything | `curl -X POST -H "X-Barq-Token: $TOKEN" -d '{"mode":"full"}' http://127.0.0.1:8080/api/v1/runs/$RUN_ID/rebuild` |
| Rerun the failed wave | `curl -X POST -H "X-Barq-Token: $TOKEN" -d '{"mode":"failed"}' http://127.0.0.1:8080/api/v1/runs/$RUN_ID/rebuild` |
| Register a secret | `curl -X POST -H "X-Barq-Token: $TOKEN" -d '{"name":"NPM_TOKEN","value":"..."}' http://127.0.0.1:8080/api/v1/secrets` |
| List secret metadata | `curl -H "X-Barq-Token: $TOKEN" http://127.0.0.1:8080/api/v1/secrets \| jq` |

## 5. Troubleshooting

### A run disappears from the UI

Almost always a follower without a leader. Check:

```sh
kubectl -n barq get lease barq-leader -o jsonpath='{.spec.holderIdentity}{"\n"}'
kubectl -n barq get pods -l app.kubernetes.io/name=barq -o wide
```

If the lease has no holder, the controller is probably failing to
start. Read its logs.

### A run stays `QUEUED`

The controller has not dispatched it. Check that the leader is alive
and that the run's steps are not waiting on a dependency that never
completed.

### A step shows `SPOT_MIGRATING` forever

The step is being rescheduled. Check the worker's exit code and the
controller log. If the cluster is genuinely out of capacity, the step
will eventually fail once the attempt budget is exhausted.

### Workers fail with `ImagePullBackOff`

The image does not exist or cannot be pulled. For `barq://` builtins,
load the plugin runtime image into the cluster:

```sh
docker build -t barq/plugin-runtime:1.0.0 plugins/runtime
kind load docker-image barq/plugin-runtime:1.0.0
```

### Workers fail with exit code 127

The image is missing a command the worker script needs. The contract
requires `/bin/sh`, `tar` and `curl` or `wget`. Build the worker image
`FROM barq/plugin-runtime:1.0.0`.

### Everything fails in production mode

The controller validates its configuration at startup. In production it
requires the S3 backend, S3 credentials, an API token and a 32 byte
master key. The log line names the missing field.

### The cache never hits

The fingerprint includes the image reference, the commands, the
environment, the source tree, the upstream fingerprints and the secret
versions. A cache hit requires all of them to be identical. Check that
`git_commit_tree` is set to an immutable value such as a commit SHA
rather than a branch name.

## 6. Upgrade procedure

1. Build and publish the new image.
2. Update the deployment image.
3. Watch the rollout. The leader gives up its lease on shutdown, so the
   standby takes over within `BARQ_LEASE_DURATION`.
4. Running workers are **not** killed by a controller rollout. They
   keep running and the new leader adopts them.

Do not change the object layout in the same release that changes the
controller. Roll the format changes out one at a time.

## 7. Metrics and logs

Barq emits structured JSON logs on stdout. Every line carries the
event name and the relevant identifiers.

Barq does not expose a Prometheus endpoint. The controller is a
library-first design and the endpoint is left to an exporter. The
signals worth alerting on are:

- the `barq-leader` Lease `renewTime` going stale,
- controller restarts,
- the object store returning errors,
- the number of `SPOT_MIGRATING` transitions per run.
