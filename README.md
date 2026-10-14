# mini-k8s

**English** | [简体中文](README.zh-CN.md)

mini-k8s is a compact Kubernetes written from scratch in Go. It implements its own
API server, scheduler, controller manager, kubelet, HTTP gateway and a CLI called
**`kk`**, reusing only etcd for storage and a local Docker Engine to run real
containers.

The goal is "small but complete": the core Kubernetes mechanisms — declarative API,
watch, control loops, scheduling, node agent and cascading deletion — implemented in
as little code as possible, end to end and observable. It is not a wrapper around a
Kubernetes distribution, it is not `kubectl`-compatible, and it is not for production.

## What it does

- **Kubernetes-style API**: `/api/v1/namespaces/{ns}/pods`, `/apis/apps/v1/...`, objects
  with `metadata`/`spec`/`status`, errors as `Status` objects, discovery endpoints.
- **API machinery**: namespaces; UIDs and `resourceVersion` optimistic concurrency;
  `generation`/`observedGeneration`; `generateName`; strict decoding (unknown fields are
  errors); JSON merge patch; `dryRun`; label and field selectors; **watch** (streamed
  from etcd watches; objects leaving a selector are reported as DELETED).
- **Deletion semantics**: finalizers; graceful Pod termination (SIGTERM, then SIGKILL
  after the grace period); force deletion; ownerReferences and garbage collection with
  Background / Foreground / Orphan propagation; deleting a Namespace deletes its contents.
- **Control plane**: event-driven informers and work queues; a Deployment controller
  (Recreate replacement on template changes, status reporting); garbage collector;
  namespace controller; scheduler binding through the `binding` subresource and
  reporting unschedulable Pods with conditions and events.
- **kubelet**: node registration and heartbeats; one worker per Pod; real Docker
  containers; CrashLoopBackOff; image pull errors (ErrImagePull/ImagePullBackOff);
  per-container status; logs with follow/tail; orphan container cleanup.
- **Observability**: Events (identical events aggregated with a count, expired after one
  hour by an etcd lease); `kk describe` shows an object's events.
- **kk**: `get` (`-o wide|json|yaml|name`, `-l`, `--field-selector`, `-A`, `-w`),
  `describe`, `apply`/`create` (server-side dry run of the whole batch first), `delete`
  (`--grace-period`, `--force`, `--cascade`), `logs -f`, `scale`, `api-resources`,
  `version`, and cluster lifecycle commands.

```text
                         kk
                          │ HTTP + bearer token (127.0.0.1:18000)
                          ▼
    etcd ◀──────── kube-apiserver ◀─────────── list / watch / write ────────────┐
                          ▲                                                    │
       ┌──────────────────┼───────────────────┬───────────────────┐            │
   scheduler     controller-manager       kubelet × 2          gateway         │
                 (deployment / gc /        │ docker.sock          │           │
                  namespace)               ▼                      ▼           │
                                     workload containers ◀── 127.0.0.1:18080 ──┘
```

Every component is a role of the single `mini` binary. Components and workloads share
one local Docker Engine and a dedicated Docker network; workers are **logical nodes**,
not separate machines.

## Prerequisites

| Requirement | Notes |
| --- | --- |
| Go 1.25 or later | `go` must be on your `PATH`. |
| Docker Engine (Linux containers) | A local, rootful Docker Engine on Linux. Workers require the daemon to accept Docker API **v1.41**. |
| Docker Compose v2 | `docker compose version` must work. |
| `make`, `curl`, `tar` | Used to build binaries and obtain etcd. |
| Local Docker access | A local Unix socket; TCP, SSH and remote Docker endpoints are rejected. |
| Free host ports | `127.0.0.1:18000` (API) and `127.0.0.1:18080` (gateway). |

Verified end to end on Ubuntu 24.04 (WSL2, x86_64) with Docker Engine 29.9, Compose
v5.6 and Go 1.27. Rootless Docker is not a validated configuration.

One project cluster per Docker Engine. The host build architecture must match the
Linux container architecture.

> **Trusted local use only.** Kubelets mount the Docker socket, which grants extensive
> control over the Docker host. The API uses a shared bearer token, without TLS or RBAC.
> Do not expose it or run untrusted workloads.

## Quick start

```sh
git clone https://github.com/cvvz/mini-k8s.git
cd mini-k8s
```

Run everything below from the project root.

### 1. Check your environment

```sh
go version
docker version
docker compose version
docker ps
```

### 2. Build and start the cluster

```sh
make build
./bin/kk cluster up
./bin/kk cluster status
```

`cluster up` builds the components, prepares etcd (on first use it downloads v3.5.17
from `storage.googleapis.com` with host-side curl), builds two `FROM scratch` images and
starts the Compose services. It then waits up to 90 seconds for a healthy API and fresh
Ready heartbeats from both workers.

```text
API healthy
NAME       STATUS   ROLES    AGE   VERSION
worker-1   Ready    worker   5s    mini-kubelet/v2
worker-2   Ready    worker   5s    mini-kubelet/v2
```

### 3. Deploy the example

```sh
./bin/kk apply -f examples/demo.yaml
./bin/kk get deployments,pods,services -o wide
curl --fail --retry 20 --retry-delay 1 --retry-all-errors http://127.0.0.1:18080/
```

The [example](examples/demo.yaml) creates a two-replica Deployment and a Service. The
gateway round-robins across ready Pods; responses look like
`hello from web-997d7b744-f8shs` (the Pod name is its hostname).

### 4. Observe and operate

```sh
export PATH="$PWD/bin:$PATH"             # use kk directly from now on

kk get pods -w                           # watch changes live (Ctrl-C to stop)
kk describe deployment web               # details and related events
kk get events
kk logs -f POD_NAME                      # a name printed by kk get pods
kk scale deployment web --replicas 3
kk delete pod POD_NAME                   # graceful; the Deployment replaces it
kk delete pod POD_NAME --force           # removed from the API immediately

kk create namespace team
kk apply -n team -f examples/demo.yaml   # namespaced copy (the gateway still serves one Service)
kk delete namespace team                 # deletes everything inside
```

### 5. Stop the cluster

```sh
./bin/kk cluster down
```

This stops reconcilers, removes this cluster's workload containers and stops Compose,
**keeping the etcd volume and token**. A later `cluster up` restores saved resources.
To remove the example for good, run `kk delete -f examples/demo.yaml` first.

## Supported subset

| Resource | apiVersion | Supported fields and behaviour |
| --- | --- | --- |
| Namespace | `v1` | Create and delete (Terminating → contents removed → deleted). `default` and `kube-system` exist at startup. |
| Node | `v1` | Registered and heartbeated by kubelets; `spec.unschedulable` is honoured by the scheduler. |
| Pod | `v1` | **One container**: `name`, `image`, `command`, `args`, `env`, `ports`; `terminationGracePeriodSeconds`; `nodeName`. The spec is immutable. |
| Deployment | `apps/v1` | `replicas` (default 1), `selector` (matchLabels/matchExpressions, immutable), `template`. Template changes replace Pods (Recreate). |
| Service | `v1` | One cluster-wide HTTP gateway Service: `selector`, one `port: 80`, `targetPort`. |
| Event | `v1` | Recorded by components, queryable, expire after one hour. |

`kk apply` uses JSON merge patch: fields in the manifest overwrite server values, but
**fields removed from a manifest are not removed from the server** (there is no
last-applied three-way merge). Every document of a batch is validated with a
server-side dry run first, so one bad document changes nothing; applying the batch is
still not a cross-resource transaction.

## Current limitations (addressed by upcoming milestones)

- Pods have one container and no probes, resource limits, ConfigMaps/Secrets or volumes.
- Deployments manage Pods directly: no ReplicaSets, rolling updates or rollbacks.
- One HTTP gateway Service; no ClusterIP, DNS, NodePort or Ingress.
- Lost nodes are not marked NotReady and their Pods are not evicted; no resource-aware
  scheduling, taints or affinity.
- No DaemonSets, Jobs, CronJobs, StatefulSets, HPA, RBAC or control-plane HA.

## Development

```sh
make vet
make test                # unit and component tests (a throwaway etcd from bin/etcd)
go test -race ./...
```

Component tests run the real API server in-process against a temporary etcd started
from `bin/etcd` (prepared by `make prepare-etcd`); they are skipped when it is missing.

End-to-end tests are a Ginkgo v2 suite modelled after Kubernetes' `test/e2e` (see
[test/e2e/README.md](test/e2e/README.md)). Each spec runs in its own namespace, which is
deleted afterwards:

```sh
make e2e                          # every spec, including Disruptive ones
make e2e-quick                    # skip specs labelled Disruptive
make e2e E2E_FOCUS='sig-apps'     # run a subset by name
```

**Run them only against a disposable project cluster.** Disruptive specs restart the
control plane and run `cluster down/up`. Gateway specs require that no other Service
exists and are skipped otherwise. Every new feature ships with e2e specs.

## Troubleshooting

| Symptom | What to check |
| --- | --- |
| `go`, `docker` or `make` not found | Install the prerequisites and add them to your `PATH`. |
| Docker commands hang | Check the engine itself (for example `systemctl status docker`) before restarting it. |
| etcd download fails | Check access to `storage.googleapis.com`, or provide a Linux etcd v3.5.17 binary at `bin/etcd`. |
| Pod shows `ErrImagePull`/`ImagePullBackOff` | `kk describe pod NAME` shows the events. Private images must be pre-pulled into the same Engine. |
| Pod shows `CrashLoopBackOff` | `kk logs NAME` shows the application output; restarts back off from 1s up to 30s. |
| Gateway returns `503` | Wait for ready Pods; check the Service selector, targetPort and node heartbeats. |
| `Error from server (Invalid)` | Fix the field named in the message; unknown fields are rejected too. |
| Port or project-name conflict | Free the ports or stop the conflicting project; `MINI_API` alone does not change Compose ports. |

The first start creates `.mini-k8s/token` with mode `0600`; keep it private and do not
delete it while the cluster runs. For manual Compose diagnostics, export the variables
`kk` normally provides (never print the token):

```sh
export MINI_TOKEN="$(cat .mini-k8s/token)"
export MINI_DOCKER_SOCKET=/var/run/docker.sock
docker compose ps
docker compose logs --tail=100 controller
unset MINI_TOKEN MINI_DOCKER_SOCKET
```

Only for a diagnosed credential-helper hang, `MINI_DOCKER_NO_CREDENTIALS=1 ./bin/kk cluster up`
uses a temporary anonymous Docker configuration without modifying yours.

## Roadmap

| Milestone | Scope | Status |
| --- | --- | --- |
| M0 | Engineering baseline, Linux docs | ✅ |
| E2E | Ginkgo e2e framework | ✅ |
| M1 | API machinery: Kubernetes-style REST, namespaces, watch, informers/work queues, GC, graceful deletion, events | ✅ |
| M2 | kind-style nodes: containerd inside each node, kubelet managing Pods over CRI, home-grown CNI (host-gw routing) | planned |
| M3 | Pods and kubelet: multiple and init containers, probes, resource limits, exec | planned |
| M4 | Self-hosted control plane: control-plane node, static/mirror Pods, etcd and control-plane components in kube-system | planned |
| M5 | ReplicaSets, rolling updates and rollbacks | planned |
| M6 | ClusterIP, Endpoints, iptables kube-proxy, DNS, NodePort, Ingress | planned |
| M7 | Scheduling framework, node leases and eviction, taints, cordon/drain | planned |
| M8 | ConfigMaps, Secrets, volumes, PVCs | planned |
| M9 | DaemonSets, Jobs, CronJobs, StatefulSets | planned |

## Design documentation

- [v2 design specification (Chinese)](docs/superpowers/specs/2026-10-14-mini-k8s-v2-design.md): architecture and milestones.
- [M1 API machinery plan (Chinese)](docs/superpowers/plans/2026-10-14-m1-api-machinery.md)
- [kind-style node feasibility spike (Chinese)](docs/superpowers/spikes/2026-10-15-kind-node-spike.md): verified recipe for containerd in node containers, CRI and host-gw networking
- [M0 baseline plan (Chinese)](docs/superpowers/plans/2026-10-14-m0-baseline.md)
- [v1 design specification](docs/superpowers/specs/2026-10-14-mini-k8s-design.md) and
  [v1 implementation plan](docs/superpowers/plans/2026-10-14-mini-k8s-plan.md) (Chinese):
  records of the original 30-minute version.
