# Barq Deployment Guide

This guide covers local development, testing, production Kubernetes deployment, and how to build a Helm chart from scratch.

---

## 1. Quick Reference: Environment Variables

| Variable | Required | Production Default | Description |
|---|---|---|---|
| `BARQ_MODE` | Yes | `production` | `production` \| `local` \| `simulator` |
| `BARQ_OBJECT_BACKEND` | Yes | `s3` | `s3` \| `local` |
| `BARQ_S3_BUCKET` | Yes | — | S3 bucket for durable state |
| `BARQ_S3_ENDPOINT` | No | `https://s3.amazonaws.com` | S3 endpoint (MinIO, R2, etc.) |
| `BARQ_S3_REGION` | No | `us-east-1` | S3 region |
| `BARQ_S3_ACCESS_KEY_ID` | Yes | — | S3 access key |
| `BARQ_S3_SECRET_ACCESS_KEY` | Yes | — | S3 secret key |
| `BARQ_S3_PATH_STYLE` | No | `false` | `true` for MinIO/R2 |
| `BARQ_MASTER_KEY` | Yes | — | 32-byte base64 or raw (encrypts secrets) |
| `BARQ_API_TOKEN` | Yes | — | Control plane API token (long random string) |
| `BARQ_NAMESPACE` | No | `barq` | K8s namespace for workers |
| `BARQ_LEASE_NAME` | No | `barq-leader` | Leader election lease name |
| `BARQ_PUBLIC_URL` | Yes | — | External URL (e.g. `https://ci.example.com`) |
| `BARQ_SECURE_COOKIES` | No | `true` | Must be true behind TLS |
| `BARQ_AUTH_REQUIRED` | No | `true` | Force authentication |
| `BARQ_ALLOW_SIGNUP` | No | `false` | Allow local account creation |
| `BARQ_OIDC_ISSUER` | No | — | OIDC issuer URL |
| `BARQ_OIDC_CLIENT_ID` | No | — | OIDC client ID |
| `BARQ_OIDC_CLIENT_SECRET` | No | — | OIDC client secret |
| `BARQ_OIDC_REDIRECT_URL` | No | — | OIDC callback URL |
| `BARQ_OIDC_ALLOWED_DOMAINS` | No | — | Comma-separated domains |
| `BARQ_GITHUB_WEBHOOK_SECRET` | No | — | HMAC secret for GitHub webhooks |
| `BARQ_GITHUB_API_TOKEN` | No | — | GitHub App token for commit status |
| `BARQ_GITLAB_WEBHOOK_TOKEN` | No | — | GitLab webhook token |
| `BARQ_GITLAB_API_TOKEN` | No | — | GitLab API token |
| `BARQ_MAX_CONCURRENT_RUNS` | No | `0` | Global concurrency limit (0 = unlimited) |
| `BARQ_MAX_ATTEMPTS` | No | `3` | Preemption retries per step |
| `BARQ_POLL_INTERVAL` | No | `2s` | Reconciliation interval |
| `BARQ_LEASE_DURATION` | No | `10s` | Leader lease TTL |
| `BARQ_LEASE_RENEW_TIME` | No | `3s` | Lease renewal interval |
| `BARQ_PLUGIN_RUNTIME_IMAGE` | No | `barq/plugin-runtime:1.0.0` | Builtin plugin image |
| `BARQ_RELAY_SECRET` | No | — | HMAC for object relay URLs |

---

## 2. Local Development & Testing

### 2.1 Prerequisites

- Go 1.22+
- Docker (for simulator mode or building images)
- kubectl (for local K8s mode)
- kind or minikube (for `local` mode)

### 2.2 Simulator Mode (Fastest, No K8s)

The simulator runs everything in-process. **Never use it to claim a build succeeded.**

```bash
# Build
go build -o barq ./cmd/barq
go build -o barqctl ./cmd/barqctl

# Run (defaults to simulator when BARQ_MODE is unset)
BARQ_AUTH_REQUIRED=true BARQ_ALLOW_SIGNUP=true ./barq

# In another terminal: sign up and run a pipeline
curl -X POST http://127.0.0.1:8080/api/v1/auth/signup \
  -H 'Content-Type: application/json' \
  -d '{"email":"dev@example.com","password":"correct horse battery staple"}'

# Get CSRF token from cookie, then create API token
CSRF=$(grep barq_csrf cookies.txt | awk '{print $7}')
TOKEN=$(curl -b cookies.txt -X POST http://127.0.0.1:8080/api/v1/tokens \
  -H "X-Barq-CSRF: $CSRF" -H 'Content-Type: application/json' \
  -d '{"name":"ci","scopes":["read","write","admin"]}' | jq -r .secret)

# Run a pipeline
./barqctl --token "$TOKEN" spawn .barq.yaml
```

### 2.3 Local Mode (Real K8s, Non-Durable)

Requires a Kubernetes cluster (kind, minikube, Docker Desktop).

```bash
# 1. Create kind cluster
kind create cluster --name barq-local

# 2. Build and load image
docker build -t barq:local .
kind load docker-image barq:local --name barq-local

# 3. Create namespace and secret
kubectl create namespace barq
kubectl -n barq create secret generic barq-credentials \
  --from-literal=master-key="$(openssl rand -hex 32 | xxd -r -p | base64)" \
  --from-literal=relay-secret="$(openssl rand -hex 32)" \
  --from-literal=api-token="$(openssl rand -hex 32)"

# 4. Deploy base manifests
kubectl apply -f deploy/k8s-manifests.yaml

# 5. Wait for readiness
kubectl -n barq wait --for=condition=ready pod -l app.kubernetes.io/name=barq --timeout=60s

# 6. Port forward
kubectl -n barq port-forward svc/barq 8080:8080

# 7. Test
export BARQ_SERVER=http://127.0.0.1:8080
# Sign up, get token, run pipelines...
```

### 2.4 Running Tests

```bash
# All tests
go test ./...

# Race detector (recommended for CI)
go test -race ./...

# Specific package with verbose output
go test -v -race ./internal/queue/

# Coverage
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
```

---

## 3. Production Deployment on Kubernetes

### 3.1 Architecture Overview

```
                    ┌─────────────────────┐
                    │   Ingress / LB      │
                    │   (TLS termination) │
                    └──────────┬──────────┘
                               │
                    ┌──────────▼──────────┐
                    │  barq-controller    │  ← 2+ replicas
                    │  (leader elected)   │
                    └──────────┬──────────┘
                               │
              ┌────────────────┼────────────────┐
              ▼                ▼                ▼
        ┌──────────┐    ┌──────────┐      ┌──────────┐
        │   S3     │    │   K8s    │      │  Forge   │
        │ (state)  │    │  API     │      │  (webhook)│
        └──────────┘    └──────────┘      └──────────┘
              │
              ▼
        ┌──────────┐
        │ Workers  │  ← Per-run Pods (ephemeral)
        │  (Pods)  │
        └──────────┘
```

### 3.2 Production Requirements

| Requirement | Details |
|---|---|
| Kubernetes | 1.28+ (lease coordination, generic ephemeral volumes) |
| S3-compatible store | AWS S3, MinIO, Cloudflare R2, etc. |
| TLS termination | Ingress controller or load balancer |
| CSI driver | For `gp3` storage class (EBS) |
| NetworkPolicy | Enforced by default manifests |

### 3.3 Prepare Secrets

```bash
# Generate secrets
MASTER_KEY=$(openssl rand -hex 32 | xxd -r -p | base64)
RELAY_SECRET=$(openssl rand -hex 32)
API_TOKEN=$(openssl rand -hex 32)

# Create secret (dry-run first to verify)
kubectl -n barq create secret generic barq-credentials \
  --from-literal=master-key="$MASTER_KEY" \
  --from-literal=relay-secret="$RELAY_SECRET" \
  --from-literal=api-token="$API_TOKEN" \
  --from-literal=endpoint="https://s3.amazonaws.com" \
  --from-literal=bucket="my-barq-bucket" \
  --from-literal=region="us-east-1" \
  --from-literal=access-key-id="AKIA..." \
  --from-literal=secret-access-key="..." \
  --from-literal=github-webhook-secret="$(openssl rand -hex 32)" \
  --from-literal=github-token="ghp_..." \
  --dry-run=client -o yaml | kubectl apply -f -
```

**Never commit these values.** Use SealedSecrets, External Secrets Operator, or Vault Agent Injector.

### 3.4 Build and Push the Image

```bash
# Multi-platform build (for arm64 + amd64 manifest list)
docker buildx create --use --name barq-builder
docker buildx build --platform linux/amd64,linux/arm64 \
  -t ghcr.io/yourorg/barq:v1.2.3 \
  --push .

# Or single platform
docker build -t ghcr.io/yourorg/barq:v1.2.3 .
docker push ghcr.io/yourorg/barq:v1.2.3
```

### 3.5 Deploy with Kustomize

```bash
# 1. Edit deploy/production/kustomization.yaml to set your image
#    images:
#      - name: barq
#        newName: ghcr.io/yourorg/barq
#        newTag: v1.2.3

# 2. Apply
kubectl apply -k deploy/production

# 3. Verify
kubectl -n barq get pods -w
kubectl -n barq logs -l app.kubernetes.io/name=barq -f
```

### 3.6 Configure Ingress

```yaml
# ingress.yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: barq
  namespace: barq
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-prod
    nginx.ingress.kubernetes.io/proxy-body-size: "100m"
    nginx.ingress.kubernetes.io/proxy-read-timeout: "300"
    nginx.ingress.kubernetes.io/proxy-send-timeout: "300"
spec:
  ingressClassName: nginx
  tls:
    - hosts:
        - ci.example.com
      secretName: barq-tls
  rules:
    - host: ci.example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: barq
                port:
                  number: 8080
```

Apply: `kubectl apply -f ingress.yaml`

### 3.7 Post-Deployment Verification

```bash
# 1. Health endpoint
curl https://ci.example.com/healthz

# 2. API (should require auth)
curl https://ci.example.com/api/v1/system/health

# 3. Metrics (unauthenticated)
curl https://ci.example.com/metrics | head

# 4. UI
open https://ci.example.com

# 5. First admin user
# Sign up at /signup, then promote:
kubectl -n barq exec -it deploy/barq-controller -- \
  /barqctl admin promote <email>
```

---

## 4. Dos and Don'ts

### 4.1 Do

| Area | Action |
|---|---|
| **Secrets** | Use SealedSecrets / External Secrets / Vault. Never commit `barq-credentials`. |
| **S3** | Use IAM roles for service accounts (IRSA) or Workload Identity instead of static keys. |
| **TLS** | Terminate TLS at the ingress. Set `BARQ_SECURE_COOKIES=true`. |
| **Backups** | Enable S3 versioning + lifecycle rules. The bucket IS your backup. |
| **Monitoring** | Scrape `/metrics` with Prometheus. Alert on `barq_runs_finished_total{status="FAILURE"}`. |
| **Scaling** | Run 2+ replicas. The lease guarantees single-leader scheduling. |
| **Upgrades** | Use rolling updates. The PDB ensures `minAvailable: 1`. |
| **Plugin image** | Pin `BARQ_PLUGIN_RUNTIME_IMAGE` to a digest (`sha256:...`). |

### 4.2 Don't

| Area | Action |
|---|---|
| **Local object store** | Never use `BARQ_OBJECT_BACKEND=local` in production. Data is lost on pod restart. |
| **Simulator mode** | Never run `BARQ_MODE=simulator` in production. It is explicitly synthetic. |
| **Single replica** | Don't run 1 replica in production. No HA, no rolling upgrade safety. |
| **Shared hot tier** | Don't use a shared PVC for the hot tier. Use generic ephemeral volumes (one per replica). |
| **Static credentials** | Don't put access keys in ConfigMaps. Use the secret. |
| **Public API token** | Don't expose `BARQ_API_TOKEN` in logs or UI. It's a root-equivalent credential. |
| **Disable auth** | Don't set `BARQ_AUTH_REQUIRED=false` in production. |
| **Wildcard CORS** | The controller sets strict CSP and CORS. Don't relax them. |

### 4.3 Worker Pod Security

The worker Pods run with:
- `runAsNonRoot: true`
- `allowPrivilegeEscalation: false`
- `readOnlyRootFilesystem: true` (except `/workspace` emptyDir)
- All capabilities dropped
- `seccompProfile: RuntimeDefault`

**Don't** relax these. If a plugin needs privileges, build a custom plugin image that doesn't.

---

## 5. Building a Helm Chart from Scratch

This section walks through creating a production-grade Helm chart for Barq.

### 5.1 Chart Structure

```
barq/
├── Chart.yaml
├── values.yaml
├── values.schema.json
├── templates/
│   ├── _helpers.tpl
│   ├── namespace.yaml
│   ├── serviceaccount.yaml
│   ├── rbac.yaml
│   ├── secret.yaml
│   ├── configmap.yaml
│   ├── deployment.yaml
│   ├── service.yaml
│   ├── pdb.yaml
│   ├── networkpolicy.yaml
│   ├── ingress.yaml
│   └── servicemonitor.yaml
└── README.md
```

### 5.2 Chart.yaml

```yaml
# Chart.yaml
apiVersion: v2
name: barq
description: Barq CI/CD controller Helm chart
type: application
version: 0.1.0
appVersion: "1.2.3"
keywords:
  - ci
  - cd
  - pipeline
  - kubernetes
maintainers:
  - name: Your Name
    email: you@example.com
sources:
  - https://github.com/yourorg/barq
```

### 5.3 values.yaml (Complete)

```yaml
# values.yaml
# Default values for Barq CI.
# All values can be overridden via --set or a custom values file.

# Global settings
global:
  # Image registry (override for air-gapped)
  imageRegistry: ""
  # Image pull secrets
  imagePullSecrets: []

# Controller image
image:
  repository: ghcr.io/yourorg/barq
  tag: "v1.2.3"
  pullPolicy: IfNotPresent
  # Sha256 digest for immutable deploys
  # digest: sha256:abc123...

# Deployment settings
replicaCount: 2

# Mode: production | local | simulator
mode: production

# Object store backend: s3 | local
objectBackend: s3

# S3 Configuration (required for production)
s3:
  endpoint: "https://s3.amazonaws.com"
  region: "us-east-1"
  bucket: ""
  pathStyle: false
  # Credentials are in the secret, not here
  # accessKeyId: ""
  # secretAccessKey: ""

# Kubernetes settings
namespace: barq
serviceAccount:
  create: true
  name: barq-controller
  annotations: {}

# Resource requests/limits
resources:
  requests:
    cpu: 100m
    memory: 128Mi
  limits:
    cpu: "1"
    memory: 512Mi

# Hot tier storage (per-replica encrypted EBS via generic ephemeral volume)
hotTier:
  storageClass: gp3
  storageSize: 10Gi

# Security context (distroless nonroot = 65532)
securityContext:
  runAsNonRoot: true
  runAsUser: 65532
  runAsGroup: 65532
  fsGroup: 65532
  seccompProfile:
    type: RuntimeDefault

# Network policy
networkPolicy:
  enabled: true
  # Additional egress rules for private registries
  additionalEgress: []

# Ingress
ingress:
  enabled: true
  className: nginx
  host: ci.example.com
  tls: true
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-prod
    nginx.ingress.kubernetes.io/proxy-body-size: "100m"
    nginx.ingress.kubernetes.io/proxy-read-timeout: "300"
    nginx.ingress.kubernetes.io/proxy-send-timeout: "300"

# Public URL (required for relay URLs)
publicUrl: "https://ci.example.com"

# Authentication
auth:
  required: true
  allowSignup: false
  secureCookies: true
  sessionTTL: 12h

# OIDC (optional)
oidc:
  enabled: false
  issuer: ""
  clientId: ""
  clientSecret: ""
  redirectUrl: ""
  allowedDomains: []

# GitHub integration (optional)
github:
  enabled: false
  webhookSecret: ""
  apiToken: ""
  apiBase: "https://api.github.com"
  owner: ""
  repository: ""

# GitLab integration (optional)
gitlab:
  enabled: false
  webhookToken: ""
  apiToken: ""
  apiBase: "https://gitlab.com"

# Concurrency
maxConcurrentRuns: 0  # 0 = unlimited

# Runtime tuning
pollInterval: 2s
leaseDuration: 10s
leaseRenewTime: 3s
maxAttempts: 3
shutdownGrace: 20s

# Plugin runtime image (pin to digest in production)
pluginRuntimeImage: barq/plugin-runtime:1.0.0

# Service monitor for Prometheus Operator
serviceMonitor:
  enabled: true
  interval: 30s
  namespace: monitoring

# Pod disruption budget
pdb:
  minAvailable: 1

# Affinity and tolerations
affinity: {}
tolerations: []

# Topology spread constraints
topologySpreadConstraints: []
  # - maxSkew: 1
  #   topologyKey: topology.kubernetes.io/zone
  #   whenUnsatisfiable: ScheduleAnyway
  #   labelSelector:
  #     matchLabels:
  #       app.kubernetes.io/name: barq

# Priority class
priorityClassName: ""

# Extra environment variables (for advanced use)
extraEnv: []

# Extra volumes and volume mounts
extraVolumes: []
extraVolumeMounts: []
```

### 5.4 values.schema.json (Validation)

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "properties": {
    "mode": { "type": "string", "enum": ["production", "local", "simulator"] },
    "objectBackend": { "type": "string", "enum": ["s3", "local"] },
    "replicaCount": { "type": "integer", "minimum": 1, "maximum": 10 },
    "s3": {
      "type": "object",
      "properties": {
        "endpoint": { "type": "string", "format": "uri" },
        "region": { "type": "string" },
        "bucket": { "type": "string", "minLength": 1 },
        "pathStyle": { "type": "boolean" }
      },
      "required": ["bucket"]
    },
    "publicUrl": { "type": "string", "format": "uri" },
    "ingress": {
      "type": "object",
      "properties": {
        "enabled": { "type": "boolean" },
        "host": { "type": "string" },
        "tls": { "type": "boolean" }
      }
    },
    "hotTier": {
      "type": "object",
      "properties": {
        "storageClass": { "type": "string" },
        "storageSize": { "type": "string", "pattern": "^[0-9]+[GM]i?$" }
      }
    }
  },
  "required": ["mode", "objectBackend", "s3", "publicUrl"],
  "additionalProperties": true
}
```

### 5.5 templates/_helpers.tpl

```gotemplate
{{/*
Expand the name of the chart.
*/}}
{{- define "barq.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "barq.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "barq.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "barq.labels" -}}
helm.sh/chart: {{ include "barq.chart" . }}
{{ include "barq.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "barq.selectorLabels" -}}
app.kubernetes.io/name: {{ include "barq.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Secret name
*/}}
{{- define "barq.secretName" -}}
{{- printf "%s-credentials" (include "barq.fullname" .) }}
{{- end }}

{{/*
ConfigMap name
*/}}
{{- define "barq.configMapName" -}}
{{- printf "%s-config" (include "barq.fullname" .) }}
{{- end }}

{{/*
Service account name
*/}}
{{- define "barq.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "barq.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Image with registry override
*/}}
{{- define "barq.image" -}}
{{- if .Values.global.imageRegistry }}
{{- printf "%s/%s" .Values.global.imageRegistry .Values.image.repository }}
{{- else }}
{{- .Values.image.repository }}
{{- end }}
{{- end }}

{{/*
toYaml with indent
*/}}
{{- define "barq.toYaml" -}}
{{- toYaml . | nindent 2 }}
{{- end }}
```

### 5.6 templates/namespace.yaml

```yaml
{{- if .Values.namespace }}
apiVersion: v1
kind: Namespace
metadata:
  name: {{ .Values.namespace }}
  labels:
    {{- include "barq.labels" . | nindent 4 }}
    pod-security.kubernetes.io/enforce: restricted
    pod-security.kubernetes.io/audit: restricted
    pod-security.kubernetes.io/warn: restricted
{{- end }}
```

### 5.7 templates/serviceaccount.yaml

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: {{ include "barq.serviceAccountName" . }}
  namespace: {{ .Values.namespace }}
  labels:
    {{- include "barq.labels" . | nindent 4 }}
  annotations:
    {{- toYaml .Values.serviceAccount.annotations | nindent 4 }}
automountServiceAccountToken: true
```

### 5.8 templates/rbac.yaml

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: {{ include "barq.fullname" . }}
  namespace: {{ .Values.namespace }}
  labels:
    {{- include "barq.labels" . | nindent 4 }}
rules:
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list", "watch", "create", "delete"]
  - apiGroups: [""]
    resources: ["pods/log"]
    verbs: ["get"]
  - apiGroups: ["coordination.k8s.io"]
    resources: ["leases"]
    verbs: ["get", "list", "watch", "create", "update", "patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: {{ include "barq.fullname" . }}
  namespace: {{ .Values.namespace }}
  labels:
    {{- include "barq.labels" . | nindent 4 }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: {{ include "barq.fullname" . }}
subjects:
  - kind: ServiceAccount
    name: {{ include "barq.serviceAccountName" . }}
    namespace: {{ .Values.namespace }}
```

### 5.9 templates/secret.yaml

```yaml
{{/*
The secret is managed externally (SealedSecrets, External Secrets, etc.).
This template declares the structure so the chart is self-documenting.
DO NOT put real values here. Use --set-file or external secret controllers.
*/}}
apiVersion: v1
kind: Secret
metadata:
  name: {{ include "barq.secretName" . }}
  namespace: {{ .Values.namespace }}
  labels:
    {{- include "barq.labels" . | nindent 4 }}
type: Opaque
stringData:
  # MASTER_KEY: 32-byte base64 (REQUIRED in production)
  master-key: ""
  # RELAY_SECRET: HMAC for presigned URLs
  relay-secret: ""
  # API_TOKEN: Control plane API token
  api-token: ""
  # S3 credentials (use IRSA/Workload Identity instead when possible)
  endpoint: ""
  bucket: ""
  region: ""
  access-key-id: ""
  secret-access-key: ""
  # Webhook secrets
  github-webhook-secret: ""
  github-token: ""
  gitlab-webhook-token: ""
  gitlab-token: ""
```

### 5.10 templates/configmap.yaml

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ include "barq.configMapName" . }}
  namespace: {{ .Values.namespace }}
  labels:
    {{- include "barq.labels" . | nindent 4 }}
data:
  BARQ_MODE: {{ .Values.mode | quote }}
  BARQ_OBJECT_BACKEND: {{ .Values.objectBackend | quote }}
  BARQ_LEASE_NAME: {{ default "barq-leader" .Values.leaseName | quote }}
  BARQ_NAMESPACE: {{ .Values.namespace | quote }}
  BARQ_HOT_ROOT: "/var/lib/barq/ebs"
  BARQ_LOCAL_OBJECT_ROOT: "/var/lib/barq/ebs/objects"
  BARQ_OBJECT_PREFIX: "barq"
  BARQ_PLUGIN_RUNTIME_IMAGE: {{ .Values.pluginRuntimeImage | quote }}
  BARQ_POLL_INTERVAL: {{ .Values.pollInterval | quote }}
  BARQ_LEASE_DURATION: {{ .Values.leaseDuration | quote }}
  BARQ_LEASE_RENEW_TIME: {{ .Values.leaseRenewTime | quote }}
  BARQ_MAX_ATTEMPTS: {{ .Values.maxAttempts | quote }}
  BARQ_SESSION_TTL: {{ .Values.auth.sessionTTL | quote }}
  BARQ_ALLOW_SIGNUP: {{ .Values.auth.allowSignup | quote }}
  BARQ_AUTH_REQUIRED: {{ .Values.auth.required | quote }}
  BARQ_SECURE_COOKIES: {{ .Values.auth.secureCookies | quote }}
  BARQ_MAX_CONCURRENT_RUNS: {{ .Values.maxConcurrentRuns | quote }}
  {{- if .Values.oidc.enabled }}
  BARQ_OIDC_ISSUER: {{ .Values.oidc.issuer | quote }}
  BARQ_OIDC_CLIENT_ID: {{ .Values.oidc.clientId | quote }}
  BARQ_OIDC_REDIRECT_URL: {{ .Values.oidc.redirectUrl | quote }}
  BARQ_OIDC_ALLOWED_DOMAINS: {{ .Values.oidc.allowedDomains | join "," | quote }}
  {{- end }}
  {{- if .Values.github.enabled }}
  BARQ_GITHUB_INTEGRATION: "true"
  BARQ_GITHUB_OWNER: {{ .Values.github.owner | quote }}
  BARQ_GITHUB_REPOSITORY: {{ .Values.github.repository | quote }}
  {{- end }}
  {{- range $key, $value := .Values.extraEnv }}
  {{ $key }}: {{ $value | quote }}
  {{- end }}
```

### 5.11 templates/deployment.yaml

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ include "barq.fullname" . }}
  namespace: {{ .Values.namespace }}
  labels:
    {{- include "barq.labels" . | nindent 4 }}
spec:
  replicas: {{ .Values.replicaCount }}
  selector:
    matchLabels:
      {{- include "barq.selectorLabels" . | nindent 6 }}
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
  template:
    metadata:
      labels:
        {{- include "barq.selectorLabels" . | nindent 8 }}
        app.kubernetes.io/component: controller
      annotations:
        checksum/config: {{ include (print $.Template.BasePath "/configmap.yaml") . | sha256sum }}
        checksum/secret: {{ include (print $.Template.BasePath "/secret.yaml") . | sha256sum }}
    spec:
      serviceAccountName: {{ include "barq.serviceAccountName" . }}
      securityContext:
        {{- toYaml .Values.securityContext | nindent 8 }}
      terminationGracePeriodSeconds: {{ .Values.shutdownGrace | int }}
      topologySpreadConstraints:
        {{- if .Values.topologySpreadConstraints }}
        {{- toYaml .Values.topologySpreadConstraints | nindent 8 }}
        {{- else }}
        - maxSkew: 1
          topologyKey: kubernetes.io/hostname
          whenUnsatisfiable: ScheduleAnyway
          labelSelector:
            matchLabels:
              {{- include "barq.selectorLabels" . | nindent 14 }}
        {{- end }}
      {{- with .Values.affinity }}
      affinity:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with .Values.tolerations }}
      tolerations:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with .Values.priorityClassName }}
      priorityClassName: {{ . }}
      {{- end }}
      containers:
        - name: controller
          image: {{ include "barq.image" . }}:{{ .Values.image.tag }}
          {{- if .Values.image.digest }}
          imagePullPolicy: IfNotPresent
          {{- else }}
          imagePullPolicy: {{ .Values.image.pullPolicy }}
          {{- end }}
          ports:
            - name: http
              containerPort: 8080
              protocol: TCP
          env:
            - name: BARQ_IDENTITY
              valueFrom:
                fieldRef:
                  fieldPath: metadata.uid
            - name: BARQ_POD_IP
              valueFrom:
                fieldRef:
                  fieldPath: status.podIP
            - name: BARQ_LEADER_URL
              value: "http://$(BARQ_POD_IP):8080"
            - name: BARQ_PUBLIC_URL
              value: {{ .Values.publicUrl | quote }}
            - name: BARQ_RELAY_SECRET
              valueFrom:
                secretKeyRef:
                  name: {{ include "barq.secretName" . }}
                  key: relay-secret
                  optional: true
            - name: BARQ_API_TOKEN
              valueFrom:
                secretKeyRef:
                  name: {{ include "barq.secretName" . }}
                  key: api-token
                  optional: true
            - name: BARQ_MASTER_KEY
              valueFrom:
                secretKeyRef:
                  name: {{ include "barq.secretName" . }}
                  key: master-key
                  optional: true
            - name: BARQ_S3_ENDPOINT
              valueFrom:
                secretKeyRef:
                  name: {{ include "barq.secretName" . }}
                  key: endpoint
                  optional: true
            - name: BARQ_S3_BUCKET
              valueFrom:
                secretKeyRef:
                  name: {{ include "barq.secretName" . }}
                  key: bucket
                  optional: true
            - name: BARQ_S3_REGION
              valueFrom:
                secretKeyRef:
                  name: {{ include "barq.secretName" . }}
                  key: region
                  optional: true
            - name: BARQ_S3_ACCESS_KEY_ID
              valueFrom:
                secretKeyRef:
                  name: {{ include "barq.secretName" . }}
                  key: access-key-id
                  optional: true
            - name: BARQ_S3_SECRET_ACCESS_KEY
              valueFrom:
                secretKeyRef:
                  name: {{ include "barq.secretName" . }}
                  key: secret-access-key
                  optional: true
            - name: BARQ_S3_PATH_STYLE
              value: {{ .Values.s3.pathStyle | quote }}
            - name: BARQ_GITHUB_WEBHOOK_SECRET
              valueFrom:
                secretKeyRef:
                  name: {{ include "barq.secretName" . }}
                  key: github-webhook-secret
                  optional: true
            - name: BARQ_GITHUB_API_TOKEN
              valueFrom:
                secretKeyRef:
                  name: {{ include "barq.secretName" . }}
                  key: github-token
                  optional: true
            - name: BARQ_GITLAB_WEBHOOK_TOKEN
              valueFrom:
                secretKeyRef:
                  name: {{ include "barq.secretName" . }}
                  key: gitlab-webhook-token
                  optional: true
            - name: BARQ_GITLAB_API_TOKEN
              valueFrom:
                secretKeyRef:
                  name: {{ include "barq.secretName" . }}
                  key: gitlab-token
                  optional: true
            {{- if .Values.oidc.enabled }}
            - name: BARQ_OIDC_CLIENT_SECRET
              valueFrom:
                secretKeyRef:
                  name: {{ include "barq.secretName" . }}
                  key: oidc-client-secret
                  optional: true
            {{- end }}
          envFrom:
            - configMapRef:
                name: {{ include "barq.configMapName" . }}
          resources:
            {{- toYaml .Values.resources | nindent 12 }}
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: ["ALL"]
          livenessProbe:
            httpGet:
              path: /healthz
              port: http
            initialDelaySeconds: 5
            periodSeconds: 10
            timeoutSeconds: 3
            failureThreshold: 3
          readinessProbe:
            httpGet:
              path: /healthz
              port: http
            initialDelaySeconds: 2
            periodSeconds: 5
            timeoutSeconds: 3
            failureThreshold: 3
          volumeMounts:
            - name: hot
              mountPath: /var/lib/barq/ebs
            - name: tmp
              mountPath: /tmp
            {{- range .Values.extraVolumeMounts }}
            {{- toYaml . | nindent 12 }}
            {{- end }}
      volumes:
        - name: hot
          {{- if eq .Values.mode "production" }}
          ephemeral:
            volumeClaimTemplate:
              spec:
                accessModes: ["ReadWriteOnce"]
                storageClassName: {{ .Values.hotTier.storageClass }}
                resources:
                  requests:
                    storage: {{ .Values.hotTier.storageSize }}
          {{- else }}
          emptyDir:
            sizeLimit: {{ .Values.hotTier.storageSize }}
          {{- end }}
        - name: tmp
          emptyDir:
            sizeLimit: 64Mi
        {{- range .Values.extraVolumes }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
      {{- with .Values.imagePullSecrets }}
      imagePullSecrets:
        {{- toYaml . | nindent 8 }}
      {{- end }}
```

### 5.12 templates/service.yaml

```yaml
apiVersion: v1
kind: Service
metadata:
  name: {{ include "barq.fullname" . }}
  namespace: {{ .Values.namespace }}
  labels:
    {{- include "barq.labels" . | nindent 4 }}
spec:
  type: ClusterIP
  selector:
    {{- include "barq.selectorLabels" . | nindent 4 }}
    app.kubernetes.io/component: controller
  ports:
    - name: http
      port: 8080
      targetPort: http
      protocol: TCP
```

### 5.13 templates/pdb.yaml

```yaml
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: {{ include "barq.fullname" . }}
  namespace: {{ .Values.namespace }}
  labels:
    {{- include "barq.labels" . | nindent 4 }}
spec:
  minAvailable: {{ .Values.pdb.minAvailable }}
  selector:
    matchLabels:
      {{- include "barq.selectorLabels" . | nindent 6 }}
      app.kubernetes.io/component: controller
```

### 5.14 templates/networkpolicy.yaml

```yaml
{{- if .Values.networkPolicy.enabled }}
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: {{ include "barq.fullname" . }}
  namespace: {{ .Values.namespace }}
  labels:
    {{- include "barq.labels" . | nindent 4 }}
spec:
  podSelector:
    matchLabels:
      {{- include "barq.selectorLabels" . | nindent 6 }}
      app.kubernetes.io/component: controller
  policyTypes: ["Ingress", "Egress"]
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: {{ .Values.namespace }}
      ports:
        - protocol: TCP
          port: 8080
  egress:
    # Kubernetes API server
    - to:
        - namespaceSelector: {}
          podSelector:
            matchLabels:
              component: kube-apiserver
      ports:
        - protocol: TCP
          port: 443
    # DNS
    - to:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: kube-system
          podSelector:
            matchLabels:
              k8s-app: kube-dns
      ports:
        - protocol: UDP
          port: 53
        - protocol: TCP
          port: 53
    # Object store and internet (HTTPS/HTTP)
    - to:
        - namespaceSelector: {}
      ports:
        - protocol: TCP
          port: 443
        - protocol: TCP
          port: 80
    {{- range .Values.networkPolicy.additionalEgress }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
{{- end }}
```

### 5.15 templates/ingress.yaml

```yaml
{{- if .Values.ingress.enabled }}
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: {{ include "barq.fullname" . }}
  namespace: {{ .Values.namespace }}
  labels:
    {{- include "barq.labels" . | nindent 4 }}
  annotations:
    {{- toYaml .Values.ingress.annotations | nindent 4 }}
spec:
  ingressClassName: {{ .Values.ingress.className }}
  {{- if .Values.ingress.tls }}
  tls:
    - hosts:
        - {{ .Values.ingress.host }}
      secretName: {{ include "barq.fullname" . }}-tls
  {{- end }}
  rules:
    - host: {{ .Values.ingress.host }}
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: {{ include "barq.fullname" . }}
                port:
                  number: 8080
{{- end }}
```

### 5.16 templates/servicemonitor.yaml

```yaml
{{- if .Values.serviceMonitor.enabled }}
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: {{ include "barq.fullname" . }}
  namespace: {{ .Values.serviceMonitor.namespace }}
  labels:
    {{- include "barq.labels" . | nindent 4 }}
spec:
  selector:
    matchLabels:
      {{- include "barq.selectorLabels" . | nindent 6 }}
      app.kubernetes.io/component: controller
  endpoints:
    - port: http
      path: /metrics
      interval: {{ .Values.serviceMonitor.interval }}
      scheme: http
  namespaceSelector:
    matchNames:
      - {{ .Values.namespace }}
{{- end }}
```

### 5.17 Testing the Chart

```bash
# 1. Lint
helm lint ./barq

# 2. Template render (dry run)
helm template barq ./barq \
  --set mode=production \
  --set objectBackend=s3 \
  --set s3.bucket=my-bucket \
  --set publicUrl=https://ci.example.com \
  --namespace barq

# 3. Install (dry run)
helm install barq ./barq \
  --namespace barq --create-namespace \
  --set mode=production \
  --set objectBackend=s3 \
  --set s3.bucket=my-bucket \
  --set publicUrl=https://ci.example.com \
  --dry-run --debug

# 4. Real install
helm install barq ./barq \
  --namespace barq --create-namespace \
  --set mode=production \
  --set objectBackend=s3 \
  --set s3.bucket=my-bucket \
  --set publicUrl=https://ci.example.com \
  --set-file masterKey=/path/to/master.key \
  --set-file relaySecret=/path/to/relay.secret \
  --set-file apiToken=/path/to/api.token \
  --set s3.accessKeyId=$(aws configure get aws_access_key_id) \
  --set s3.secretAccessKey=$(aws configure get aws_secret_access_key)

# 5. Upgrade
helm upgrade barq ./barq \
  --namespace barq \
  --set image.tag=v1.2.4

# 6. Uninstall
helm uninstall barq -n barq
```

### 5.8 Using External Secrets Operator (Recommended)

```yaml
# external-secret.yaml
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata:
  name: barq-credentials
  namespace: barq
spec:
  refreshInterval: 1h
  secretStoreRef:
    name: vault-backend
    kind: ClusterSecretStore
  target:
    name: barq-credentials
    creationPolicy: Owner
  data:
    - secretKey: master-key
      remoteRef:
        key: barq/production
        property: master_key
    - secretKey: relay-secret
      remoteRef:
        key: barq/production
        property: relay_secret
    - secretKey: api-token
      remoteRef:
        key: barq/production
        property: api_token
    - secretKey: access-key-id
      remoteRef:
        key: barq/production/s3
        property: access_key_id
    - secretKey: secret-access-key
      remoteRef:
        key: barq/production/s3
        property: secret_access_key
    - secretKey: github-webhook-secret
      remoteRef:
        key: barq/production/github
        property: webhook_secret
    - secretKey: github-token
      remoteRef:
        key: barq/production/github
        property: api_token
```

Then in `values.yaml` remove the secret template or set `secret.enabled: false`.

---

## 6. Operational Runbooks

### 6.1 Rotating the Master Key

```bash
# 1. Generate new key
NEW_KEY=$(openssl rand -hex 32 | xxd -r -p | base64)

# 2. Update secret (External Secrets will sync, or manual)
kubectl -n barq patch secret barq-credentials \
  -p '{"stringData":{"master-key":"'"$NEW_KEY"'"}}'

# 3. Rolling restart to pick up new key
kubectl -n barq rollout restart deploy/barq-controller

# 4. Verify
kubectl -n barq logs -l app.kubernetes.io/name=barq | grep "master key rotated"
```

### 6.2 Rotating the API Token

```bash
# 1. Generate new token
NEW_TOKEN=$(openssl rand -hex 32)

# 2. Update secret
kubectl -n barq patch secret barq-credentials \
  -p '{"stringData":{"api-token":"'"$NEW_TOKEN"'"}}'

# 3. Rolling restart
kubectl -n barq rollout restart deploy/barq-controller

# 4. Update all CI clients (barqctl, webhooks, etc.)
```

### 6.3 Disaster Recovery

| Scenario | Recovery |
|---|---|
| **Controller pod lost** | Lease expires, new leader elected. Runs in queue are re-queued. No data loss (S3 is source of truth). |
| **S3 bucket deleted** | **Catastrophic.** Restore from S3 versioning + cross-region replication. |
| **Master key lost** | **Catastrophic.** All encrypted secrets unrecoverable. Rotate every secret in every pipeline. |
| **Leader stuck** | Delete the lease: `kubectl -n barq delete lease barq-leader`. New leader elected. |
| **Worker Pods orphaned** | Controller adoption on restart reconciles them. Or manual: `kubectl -n barq delete pod -l barq.io/run-id=<id>` |

### 6.4 Scaling the Controller

```bash
# Scale replicas (requires production mode + S3)
kubectl -n barq scale deploy/barq-controller --replicas=3

# Or via Helm
helm upgrade barq ./barq --namespace barq --set replicaCount=3
```

### 6.5 Upgrading Barq

```bash
# 1. Build new image
docker buildx build --platform linux/amd64,linux/arm64 \
  -t ghcr.io/yourorg/barq:v1.2.4 --push .

# 2. Update Helm values
helm upgrade barq ./barq \
  --namespace barq \
  --set image.tag=v1.2.4 \
  --set image.digest=sha256:abc123...

# 3. Watch rollout
kubectl -n barq rollout status deploy/barq-controller

# 4. Verify
curl https://ci.example.com/healthz
curl https://ci.example.com/metrics | grep barq_runs
```

---

## 7. Troubleshooting

### 7.1 Controller Won't Start

| Symptom | Cause | Fix |
|---|---|---|
| `production mode requires BARQ_OBJECT_BACKEND=s3` | Mode mismatch | Set `BARQ_OBJECT_BACKEND=s3` |
| `production mode requires BARQ_S3_BUCKET` | Missing bucket | Set bucket in secret |
| `master key must be 32 raw bytes` | Wrong key format | Generate 32-byte base64: `openssl rand -hex 32 \| xxd -r -p \| base64` |
| `lease already held` | Stale leader | `kubectl delete lease barq-leader -n barq` |
| `dial tcp: connection refused` (S3) | Network policy | Check egress to 443, DNS |
| `ImagePullBackOff` | Wrong image/tag | Check `image.repository`, `image.tag`, credentials |

### 7.2 Workers Not Starting

| Symptom | Cause | Fix |
|---|---|---|
| `PodPending` | No resources | Check node capacity, quotas |
| `ImagePullBackOff` | Plugin image missing | Push `BARQ_PLUGIN_RUNTIME_IMAGE` to registry |
| `CreateContainerConfigError` | Secret missing | Check `barq-credentials` exists |
| `Forbidden` (RBAC) | SA permissions | Verify Role/RoleBinding applied |

### 7.3 Webhooks Not Firing

| Symptom | Cause | Fix |
|---|---|---|
| 401 from forge | Wrong secret | Verify `BARQ_GITHUB_WEBHOOK_SECRET` matches GitHub app |
| Timeout | Network policy | Allow egress to GitHub/GitLab IPs |
| No runs created | Branch not allowed | Check `BARQ_ALLOWED_BRANCHES` |

### 7.4 Debug Commands

```bash
# Controller logs
kubectl -n barq logs -l app.kubernetes.io/name=barq -f --tail=200

# Describe pod
kubectl -n barq describe pod -l app.kubernetes.io/name=barq

# Check lease
kubectl -n barq get lease barq-leader -o yaml

# Check runs in S3
aws s3 ls s3://my-barq-bucket/barq/runs/ --recursive

# Port forward for local debugging
kubectl -n barq port-forward svc/barq 8080:8080

# Exec into controller
kubectl -n barq exec -it deploy/barq-controller -- /barqctl runs

# Check metrics
curl http://127.0.0.1:8080/metrics | grep barq_
```

---

## 8. Appendix: Complete Values Reference

```bash
# All configurable values with descriptions
helm show values ./barq
```

### 8.1 Minimal Production Values File

```yaml
# values-production.yaml
mode: production
objectBackend: s3
publicUrl: "https://ci.mycompany.com"

s3:
  bucket: "my-company-barq"
  region: "us-east-1"

replicaCount: 2

ingress:
  enabled: true
  host: ci.mycompany.com
  tls: true

auth:
  required: true
  allowSignup: false
  secureCookies: true

# Secrets provided via External Secrets Operator
# masterKey, relaySecret, apiToken, s3 credentials, webhook secrets
```

Deploy:

```bash
helm install barq ./barq -n barq --create-namespace -f values-production.yaml
```

---

## 9. Appendix: Plugin Runtime Image

The built-in plugins run in a separate image. Build and publish it:

```dockerfile
# plugins/runtime/Dockerfile
FROM alpine:3.20
RUN apk add --no-cache git bash curl jq docker-cli
# Add any tools your builtin plugins need
ENTRYPOINT ["/barq-plugin-runtime"]
```

```bash
docker build -t ghcr.io/yourorg/barq-plugin-runtime:v1.0.0 ./plugins/runtime
docker push ghcr.io/yourorg/barq-plugin-runtime:v1.0.0

# Then in values.yaml:
pluginRuntimeImage: ghcr.io/yourorg/barq-plugin-runtime:v1.0.0
```

Pin to digest in production:

```yaml
pluginRuntimeImage: ghcr.io/yourorg/barq-plugin-runtime@sha256:abc123...
```

---

*This guide is maintained alongside the code. See `deploy/` for the Kustomize base and production overlay, and `docs/operations.md` for runtime configuration reference.*