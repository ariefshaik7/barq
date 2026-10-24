# Barq Security Model

Barq runs arbitrary code supplied by whoever can create a pipeline. That
is the nature of a CI system. This document describes what Barq
protects, what it deliberately does not, and where the real risks are.

## 1. Trust boundaries

```text
                 ┌──────────────────────────────────────────┐
  Internet ─────▶│ Ingress / API gateway                    │
                 │  • TLS termination                       │
                 │  • Authentication (OIDC, mTLS, ...)      │
                 └──────────────────┬───────────────────────┘
                                    │
                 ┌──────────────────▼───────────────────────┐
  GitHub /      │ Webhook endpoints                        │
  GitLab ──────▶│  • HMAC / token verification             │
                 │  • Replay protection                     │
                 └──────────────────┬───────────────────────┘
                                    │
                 ┌──────────────────▼───────────────────────┐
  Operators ───▶│ Control plane API                        │
                 │  • Bearer token, constant-time compare   │
                 │  • Followers proxy to the leader         │
                 └──────────────────┬───────────────────────┘
                                    │
                 ┌──────────────────▼───────────────────────┐
  Pipeline ─────▶│ Worker Pods                             │
  authors        │  • No service account token              │
                 │  • Read-only root filesystem             │
                 │  • Dropped capabilities                  │
                 │  • No privilege escalation               │
                 │  • Resource requests and limits          │
                 └──────────────────────────────────────────┘
```

**A webhook signature authenticates the sender, not the operator.**
The two domains are separate. A valid GitHub signature does not
authorise a request to create a run, and the API token does not
authorise an inbound webhook.

## 2. Authentication

### 2.1 Control plane

Barq has two credential types and they are deliberately separated.

**Session cookies** are the interactive credential. A sign-in issues a
session that is stored in an `HttpOnly` cookie with `SameSite=Lax`.
A state changing request that uses a session cookie must present a
CSRF token in the `X-Barq-CSRF` header; a cross site form post cannot
reuse the session.

**API tokens** are the programmatic credential. They are presented as
`Authorization: Bearer <token>` and carry explicit scopes:

| Scope | Grants |
|---|---|
| `read` | Read runs, logs and statistics |
| `write` | Create, stop and rebuild runs |
| `admin` | Everything, including users, secrets and alerts |

A bearer token is never attached automatically by a browser, so it is
not vulnerable to cross site request forgery and it does not require a
CSRF token. The comparison is constant time in both cases.

The plaintext of an API token is shown exactly once. Only a keyed
digest is stored, so a leak of the object store does not leak usable
credentials.

### 2.2 Passwords

Local account passwords are hashed with **PBKDF2-HMAC-SHA256** at
600,000 iterations, a 16 byte random salt and a 32 byte derived key.
The salt makes every hash unique, and the iteration count makes an
offline cracking attack against a leaked hash database expensive.

The hash format is self describing (`pbkdf2-sha256$iterations$salt$key`),
so the parameters can be raised later without invalidating existing
hashes.

**Why not Argon2id.** Argon2id is the stronger algorithm and would be
the right choice, but the Go standard library provides no memory-hard
KDF and adding `golang.org/x/crypto` would break the "one binary, no
module graph" property that this project is built around. PBKDF2 with
a high iteration count is the NIST-approved fallback. If the zero
dependency constraint is ever relaxed, switch the hashing in
`internal/auth/password.go` and keep the format self describing so
that both can coexist during a migration.

**Password policy.** At least 12 characters, mixing letters with digits
or symbols. The policy is length based rather than composition based,
because length is the property that actually matters and composition
rules push users towards predictable substitutions.

**Timing.** A login attempt against an account that does not exist
performs a dummy hash verification, so a missing account and a wrong
password take a similar amount of time and the endpoint cannot be used
to enumerate accounts.

### 2.3 Roles

| Role | Can |
|---|---|
| `viewer` | Read runs, logs and statistics |
| `builder` | Create, stop and rebuild runs |
| `admin` | Everything, including users, secrets, alerts and configuration |

The roles form a hierarchy. A `builder` satisfies a `viewer`
requirement, and an `admin` satisfies any requirement.

The first account created on a fresh installation becomes an
administrator, so a new deployment can be bootstrapped without an
out-of-band secret.

### 2.4 Single sign-on

OIDC uses the authorization code flow with PKCE.

- The ID token signature is verified against the issuer's published
  key set.
- The algorithm is **pinned** to the set that Barq accepts. It is
  never taken from the token, which is the classic JWT confusion
  vulnerability where an attacker signs a token with `alg: none` or
  with a public key as the HMAC secret.
- The issuer, the audience, the expiry and the nonce are all
  validated.
- An account is matched on the issuer and subject, never on the email
  address. The email address is only a claim; the subject is the
  stable identifier.
- A domain allow list restricts which accounts may sign in.

The OIDC issuer must use `https`. There is no plain text fallback.

### 2.5 Webhooks

**GitHub.** The `X-Hub-Signature-256` header is `sha256=` followed by
the hex HMAC-SHA256 of the **raw request body**. The raw body matters:
re-serialising the JSON changes the bytes and breaks the signature.
The comparison is constant time.

**GitLab.** The `X-Gitlab-Token` header is compared to the configured
token in constant time.

Both endpoints:

- read the body with a 4 MiB limit,
- reject a missing or malformed signature,
- record the delivery identifier so a replayed delivery is
  acknowledged but does not start a second run,
- apply the branch allow list,
- fetch the pipeline definition from the **pushed commit** through the
  provider API rather than trusting a definition supplied in the
  request.

### 2.6 Object relay

The local object relay issues HMAC capabilities scoped to the HTTP
method, the object key and an expiry. The signature covers all three,
so a `GET` capability cannot be replayed as a `PUT` and a capability
for one key cannot be used for another.

The relay rejects:

- requests with no signature,
- requests with a wrong signature,
- expired capabilities,
- the wrong HTTP method,
- keys containing path traversal segments,
- keys that are absolute.

The relay is only mounted when the local object store is selected. In
production the S3 backend issues native SigV4 pre-signed URLs and no
relay endpoint exists.

## 3. Secrets

### 3.1 Storage

Secret values are encrypted with AES-256-GCM before they are written
to the object store. The master key is supplied from outside the
secret database:

- `BARQ_MASTER_KEY_FILE` pointing at a mounted file, or
- `BARQ_MASTER_KEY` containing 32 raw bytes or their base64 encoding.

The key must be exactly 32 bytes. It is required in production mode.

The entire database is one authenticated blob. A single bit of
tampering fails the GCM authentication tag and the read is rejected.
There is no partial decryption and no silent fallback to plaintext.

### 3.2 The API

`GET /api/v1/secrets` returns **metadata only**: name, version,
creation and update timestamps, and a truncated SHA-256 fingerprint.
It never returns a value. The fingerprint exists so an operator can
tell whether a secret changed without learning it.

`POST /api/v1/secrets` accepts a value. `DELETE /api/v1/secrets/{name}`
removes one.

### 3.3 In workers

A step requests a secret by name. The controller injects the value
into the worker environment under that name.

**Limitation, stated plainly:** an environment variable is visible in
the Pod spec to anyone with `get pods` permission, and a process can
read its own environment. Barq does not mount secrets as files and
does not use a CSI secret provider. If your threat model includes
"cluster operators who can read Pod specs", restrict RBAC accordingly
or place a secret provider in front of Barq.

### 3.4 In logs

Secret values are redacted **before** they reach the hot tier, the
object store or a live SSE subscriber. The masker is boundary safe: it
retains the longest possible partial secret between chunks, so a secret
split across two log chunks is still redacted.

**Limitation:** redaction is literal byte matching. It will not catch
a secret that a step base64-encodes, hex-encodes, splits across
multiple writes with a delay, or prints in a transformed form. Treat
log redaction as defence in depth, not as a guarantee.

## 4. Worker isolation

Worker Pods are configured with:

| Control | Value |
|---|---|
| `automountServiceAccountToken` | `false` |
| `allowPrivilegeEscalation` | `false` |
| `readOnlyRootFilesystem` | `true` |
| `capabilities.drop` | `["ALL"]` |
| `runAsNonRoot` | `true`, uid `65532` |
| `restartPolicy` | `Never` |
| Resource requests | 100m CPU, 128 MiB |
| Resource limits | 2 CPU, 2 GiB |
| Workspace volume | `emptyDir`, 10 GiB limit |

The worker ServiceAccount has no RoleBinding. A worker cannot read
Pods, Leases or Secrets in the namespace.

**Not implemented:** a NetworkPolicy. Without one, a worker can reach
the whole cluster network, including the API server and the metadata
service. On a shared cluster, add a default-deny policy for the
namespace and allow egress only to the object store and to the
internet endpoints your pipelines genuinely need.

**Not implemented:** per-tenant quotas or concurrency limits. A single
pipeline can currently fan out to as many workers as its matrix and
`parallelism` settings allow, bounded only by the cluster's capacity
and the `parallelism: 64` limit in the parser. Add a LimitRange or an
admission policy if you need a hard ceiling.

## 5. Supply chain

- The controller image is `gcr.io/distroless/static-debian12:nonroot`
  plus a static binary. It contains no shell, no package manager and
  no interpreter that an attacker could reach. The only files are the
  binary and the CA bundle.
- The build verifies that the binary is static. A dynamic binary would
  need a libc in the final image, which defeats the purpose of the
  distroless base.
- The build is multi-platform by default. The target platform follows
  the builder, so the same Dockerfile produces an arm64 image on
  Apple Silicon and an amd64 image on Intel. Use buildx to produce a
  manifest list for both:
  `docker buildx build --platform linux/amd64,linux/arm64 -t barq .`
- The plugin runtime image is `alpine:3.20` with `curl`, `git` and
  `jq`. **Pin it by digest in production.** A mutable tag is a supply
  chain risk and also breaks the Merkle cache, because the fingerprint
  includes the image reference.
- Custom plugin images should be pinned by digest for the same
  reasons.
- The Merkle cache includes the image *reference*. If you use a
  mutable tag, a changed image can produce a cache hit for different
  content. Use a digest if that matters to you.

## 6. Known risks

| Risk | Status | Mitigation |
|---|---|---|
| Worker can reach the cluster network | Mitigated | A namespace NetworkPolicy ships with the manifests; extend it for your egress rules |
| Secret visible in the Pod spec | Open | Restrict RBAC or use a secret provider |
| Transformed secrets in logs | Not solvable in general | Do not print secrets |
| Plugin runs arbitrary code | By design | Treat plugin images as trusted |
| No admission control on pipeline count | Open | Add a LimitRange or a quota |
| No per-tenant isolation | Open | Use one namespace per team |
| Webhook delivery floods | Partially handled | Branch allow list, rate limiter at the ingress and in the controller |
| Token in browser `localStorage` | Accepted for internal use | Use an authenticating gateway for public exposure |
| PBKDF2 rather than Argon2id | Accepted | Documented in section 2.2; the hash format allows a later migration |
| OIDC issuer compromise | Inherent | The issuer is trusted; pin the issuer URL and restrict the domain allow list |

## 7. Reporting a vulnerability

Open a security advisory on the repository. Please do not open a
public issue for an unpatched vulnerability.
