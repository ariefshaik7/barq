# Barq compared with Drone, Woodpecker, Jenkins and Tekton

This document compares Barq's design against the systems it is most
often compared with. It is based on the documented behaviour and the
published architecture of those projects. **No source code was copied
from any of them.** Where a specific implementation detail is
attributed, treat it as a description of that project's public
documentation rather than a line-by-line audit.

## 1. Summary

| | Barq | Drone | Woodpecker | Jenkins | Tekton |
|---|---|---|---|---|---|
| Language | Go | Go | Go | Java | Go |
| Architecture | Single controller, native K8s REST | Server + agents | Server + agents | Controller + agents | Controllers in-cluster |
| CRDs | **None** | None | None | None (uses Jobs) | **5 to 7 per pipeline** |
| External database | **None** | PostgreSQL | PostgreSQL | Jenkins home | Postgres optional |
| Plugin model | OCI container | OCI/Docker image | OCI/Docker image | `.hpi` Java | Container + `Task` CRD |
| Plugin runtime | Plain container | Plain container | Plain container | JVM plugin classloader | Container |
| HA | Lease, single writer | Server replicas | Server replicas | HA with a shared home | Controllers |
| UI | Embedded SVG DAG | React web app | React web app | Legacy + Blue Ocean | Tekton Dashboard |
| Cold storage | S3 compatible | Object storage | Object storage | Artifact storage | S3/PVR optional |

## 2. Drone CI

Drone is a Go-based CI server with a server/agent split, a Postgres
backing store, and pipelines defined in `.drone.yml`. Its agents pull
work from the server.

**What Barq takes from Drone.** The idea that a pipeline definition
should be small, declarative and pleasant to read. Drone's YAML is
excellent and Barq's subset parser follows the same spirit: strict,
predictable, and rejecting anything ambiguous.

**Where Barq differs.**

- *No server/agent split.* Drone's architecture assumes a persistent
  server that knows about every agent. Barq is a single controller
  that talks to the Kubernetes API. There is no second component to
  install, and no agent registration protocol.
- *No Postgres.* Drone uses a database for pipeline and build state.
  Barq keeps state in an S3 compatible object store. The trade is
  different: Drone's model supports richer querying, Barq's model
  removes an entire class of operational work. A build log and a run
  state are documents; they do not need relational indexes.
- *Plugin execution.* Drone plugins are containers whose entrypoint is
  invoked directly. Barq uses the same idea but wraps the execution so
  that the workspace is captured and the step can be checkpointed on
  preemption. Drone's plugin step does not have that guarantee.
- *Self-hosted agents.* Drone requires you to run and secure agents.
  Barq's workers are ordinary Pods created by the controller, with
  their own security context and no service account token.

**Where Barq is weaker.** Drone has a mature, well-documented
ecosystem of pipeline conditions, cron triggers, promotion
environments and a large plugin catalogue. Barq has none of those. If
you need a promotion pipeline with environment gating, Drone is ahead.

## 3. Woodpecker CI

Woodpecker is a fork of Drone that keeps the server/agent model, drops
the Drone trademark, and emphasises a small, hackable core. It uses
Postgres and a server that exposes a gRPC API to agents.

**What Barq takes from Woodpecker.** The idea that a CI system should
be small enough to read in an afternoon. Woodpecker's simplicity is a
deliberate design goal and Barq shares it: one binary, one process,
no plugin framework to learn.

**Where Barq differs.**

- *gRPC versus the Kubernetes API.* Woodpecker agents talk to the
  server over gRPC. Barq's workers are Kubernetes Pods, so there is no
  agent protocol to implement and no agent to secure.
- *Shared state.* Woodpecker needs Postgres because the server and the
  agents are separate processes that must agree. Barq's controller is
  the only writer, and it can use an object store.
- *No Cron or pipeline-level triggers yet.* Woodpecker has cron
  triggers built in. Barq currently triggers through the API and
  through webhooks. See the roadmap.

**Where Barq is weaker.** Woodpecker has a mature multi-server
deployment story with a documented split between the server and the
agents, and a large collection of pipeline providers. Barq's
Kubernetes-native model is narrower by design.

## 4. Jenkins

Jenkins is a Java controller with a JVM plugin ecosystem, a persistent
`JENKINS_HOME`, and a heavyweight agent protocol.

**What Barq takes from Jenkins.** The pipeline-as-code model, and the
idea that a build should be a first-class, inspectable object with
history, logs and a status.

**Where Barq differs.**

- *No JVM.* A Jenkins controller typically holds 2 to 4 GiB of heap
  and a large plugin set. A Barq controller is a static Go binary
  that idles in tens of MiB.
- *No `.hpi` plugins.* Jenkins plugins are JVM jars loaded into the
  controller's classloader. That is the source of a large fraction of
  Jenkins operational pain: classpath conflicts, version skew, and
  the need to restart the controller to load a plugin. Barq plugins
  are containers. They cannot conflict with the controller, they
  cannot crash it, and they are versioned with the pipeline.
- *Stateless controllers.* Jenkins keeps build state in
  `JENKINS_HOME`. Barq keeps run state in the object store, so a
  controller can be replaced without losing the run graph.
- *No Groovy CPS.* Jenkins serialises the continuation of a pipeline
  step to disk between steps. Barq's steps are separate Pods, so there
  is no continuation to serialise. The cost is a Pod per step
  instead of a process on a long-lived agent.
- *No controller JVM to crash.* A plugin crash is a container exit
  code. A controller crash is a Pod restart and a leader election.

**Where Barq is weaker.** Jenkins is 15 years old and has an
enormous ecosystem, a mature RBAC and audit model, distributed
masters, and a plugin marketplace with thousands of extensions. None of
that is reproducible in a weekend project, and Barq does not try.

## 5. Tekton Pipelines

Tekton is the Kubernetes-native reference implementation and the
closest competitor in spirit.

**What Barq takes from Tekton.** Running each step as a Pod, declaring
the whole pipeline in version control, and using the Kubernetes API as
the execution substrate.

**Where Barq differs.**

- *No CRDs.* A Tekton pipeline needs `Pipeline`, `PipelineRun`,
  `Task`, `TaskRun`, and usually `TriggerTemplate`, `TriggerBinding`
  and `EventListener`. Every one of them is an object in `etcd`. A
  large installation writes thousands of `PipelineRun` and `TaskRun`
  status objects, which is the direct cause of API server latency and
  storage pressure. Barq installs **zero** CRDs. The only coordination
  object is a single `Lease`.
- *No external database.* Tekton's results are usually kept in a
  separate service with its own database. Barq's durable tier is an
  object store that you probably already operate.
- *A bounded control plane.* Tekton's controllers reconcile
  `PipelineRun` objects that live indefinitely. Barq's controller
  reconciles an in-memory run graph that is rebuilt from the object
  store when needed.
- *Content-addressable caching.* Tekton has no built-in step cache.
  Barq computes a Merkle fingerprint and skips Pod creation entirely on
  a hit, restoring the original workspace so downstream outputs are
  preserved.
- *Spot preemption.* Tekton does not treat a preempted node as a
  retryable condition. Barq's worker traps `SIGTERM`, checkpoints the
  workspace and exits `75`, and the controller reschedules the step.
  This is the single biggest practical difference for teams running
  Spot capacity.

**Where Barq is weaker.** Tekton is a Kubernetes initiative with a
large ecosystem: Chains for provenance, Results for history, the
Dashboard, and a deep body of integration with the rest of the
platform. Barq implements one coherent execution model instead of a
suite of composable ones. Tekton also supports arbitrarily complex
step graphs; Barq's DAG is a straightforward dependency graph.

## 6. Feature gaps, stated plainly

Barq does **not** currently implement:

| Feature | Drone | Woodpecker | Jenkins | Tekton | Barq |
|---|---|---|---|---|---|
| Cron triggers | yes | yes | plugin | yes | **no** |
| Pipeline conditions / `when` | yes | yes | plugin | yes | **no** |
| Multi-branch / PR triggers | yes | yes | plugin | yes | partial (webhook) |
| Manual approval steps | plugin | plugin | plugin | yes | **no** |
| Artifact registry | external | external | plugin | Chains | **no** |
| Build provenance / signing | no | no | plugin | Chains | **no** |
| Pipeline templates | yes | yes | plugin | yes | **no** |
| REST API for triggering | yes | yes | yes | yes | yes |
| UI | yes | yes | yes | separate | embedded |
| Multi-cluster | no | no | plugin | no | **no** |
| Queue and concurrency control | yes | yes | plugin | yes | **no** |
| Secrets management | external | external | plugin | external | **yes, built in** |
| Step-level caching | no | no | plugin | no | **yes** |
| Preemption recovery | no | no | no | no | **yes** |

## 7. Where Barq is genuinely better

Three things Barq does that the others do not, in a way that is
architectural rather than incremental:

1. **Spot preemption is a retry, not a failure.** Every other system
   treats a reclaimed node as a broken build. Barq treats it as a
   reschedule, with a checkpoint. On a Spot fleet this is the
   difference between a usable CI system and an unusable one.

2. **The cache restores outputs.** Most caching stories mark a step
   successful and move on. Barq stores the workspace object key in the
   cache record, so a downstream step still receives the artifacts and
   the exported variables of the cached step. A cache that silently
   drops outputs is worse than no cache.

3. **Zero CRDs and zero database.** A Barq installation adds one
   Deployment, one Service, one Role and one Lease to a cluster. It
   does not add a database to operate, back up and secure, and it does
   not add CRDs to the API server that every other tenant in the
   cluster also has to tolerate.

## 8. Choosing between them

| Use Barq when | Use something else when |
|---|---|
| You run CI on Spot capacity and preemption is your top problem | You need a large plugin ecosystem you cannot rebuild |
| You want a CI system with no database and no CRDs | You need manual approval gates, cron and promotion pipelines today |
| Your team will maintain the CI system in-house | You want a supported product with an SLA |
| You want a single binary you can read and audit | You need a UI that non-engineers can operate comfortably |
| You need step-level caching that preserves outputs | You need a general workflow engine with dynamic fan-out |
| You want secrets managed with a real threat model | You need multi-cluster or cross-region execution |
