# mini-k8s

**English** | [简体中文](README.zh-CN.md)

mini-k8s is a compact Kubernetes written from scratch in Go. It implements its own
API server, scheduler, controller manager, kubelet, CNI plugin, HTTP gateway and a CLI
called **`kk`**, reusing only etcd for storage, containerd as the container runtime and a
local Docker Engine to host the cluster's "machines".

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
- **Nodes like kind**: each node is a privileged container running its own containerd;
  the kubelet manages Pods over **CRI** (a sandbox per Pod, then its container);
  Ready only when the runtime, the Pod network and the sandbox image are ready.
- **Pods**: several containers sharing one network namespace; init containers that run
  one after another to completion; `restartPolicy` Always/OnFailure/Never with the
  Succeeded/Failed phases and the Initialized/ContainersReady/Ready conditions computed
  like the Kubernetes kubelet; liveness, readiness and startup probes (httpGet,
  tcpSocket, exec); CPU and memory requests/limits enforced through cgroups (`OOMKilled`)
  and QoS classes; `emptyDir` (disk or memory) and `hostPath` volumes; `hostNetwork`;
  `kk exec` runs commands in containers.
- **kubelet**: node registration and heartbeats; one worker per Pod; restarts with
  backoff (CrashLoopBackOff); image pull errors (ErrImagePull/ImagePullBackOff); sandbox
  recreation after a node restart; logs with follow/tail/`--previous`; orphan cleanup.
- **Pod network**: the node IPAM controller gives every node a `/24` from `10.244.0.0/16`;
  a home-grown CNI plugin (`mini-cni`: bridge, veth, IP allocation) wires each Pod; a
  host-gw route agent connects the nodes' Pod subnets, so Pods reach each other across
  nodes without NAT; Pods resolve names and reach the outside world through their node.
- **Observability**: Events (identical events aggregated with a count, expired after one
  hour by an etcd lease); `kk describe` shows an object's events.
- **kk**: `get` (`-o wide|json|yaml|name`, `-l`, `--field-selector`, `-A`, `-w`),
  `describe`, `apply`/`create` (server-side dry run of the whole batch first), `delete`
  (`--grace-period`, `--force`, `--cascade`), `logs -f/-p/-c`, `exec`, `scale`, `api-resources`,
  `version`, cluster lifecycle and `image load` (copy local images into the nodes).

```text
                         kk
                          │ HTTP + bearer token (127.0.0.1:18000)
                          ▼
    etcd ◀──────── kube-apiserver ◀──────────── list / watch / write ───────────────┐
                          ▲                                                         │
        ┌─────────────────┼────────────────────┐                                    │
   scheduler     controller-manager             │                    gateway ◀── 127.0.0.1:18080
                 (deployment / gc /             │                       │ host-gw routes
                  namespace / nodeipam)         │                       ▼
   ┌─ node container worker-1 ──────────────────┴──┐   ┌─ node container worker-2 ─────┐
   │ kubelet ──CRI──▶ containerd ──▶ Pods          │   │ kubelet ─▶ containerd ─▶ Pods │
   │ mini-cni: cni0 bridge, 10.244.1.0/24          │◀─▶│ cni0, 10.244.2.0/24           │
   └───────────────────────────────────────────────┘   └───────────────────────────────┘
```

Every component is a role of the single `mini` binary. The control plane runs in plain
containers; each worker is a **privileged node container** (like a
[kind](https://kind.sigs.k8s.io) node) whose own containerd runs the Pods, so Pods are
not visible to the host's `docker ps`. All containers share one Docker network, which
plays the role of the machines' L2 network.

## What is built here and what is reused

The split mirrors the Kubernetes project itself: everything Kubernetes implements is
written from scratch here; what Kubernetes delegates to other projects (storage, the
container runtime) uses those open-source components as-is, configured but not modified.

**Built here** (Go, all in this repository):

| Component | Responsibility | Status |
| --- | --- | --- |
| kube-apiserver | Kubernetes-style REST, watch, admission and validation, deletion semantics, subresources | done |
| controller-manager | Deployment, garbage collector, namespace and node IPAM controllers; later ReplicaSet, Endpoints, node lifecycle and more | done, growing |
| scheduler | Picks a node for each Pod and binds it | done |
| kubelet | Node registration and heartbeats, Pod lifecycle over CRI (the client side; `pkg/kubelet/kuberuntime`), status, graceful termination, logs, CNI configuration, node-local DNS forwarder | done |
| client libraries | Generic REST client, informers, work queues, event recorder | done |
| gateway | Forwards HTTP on `127.0.0.1:18080` to a Service's ready Pods | done |
| `kk` | CLI, cluster lifecycle, image loading | done |
| demo app | HTTP server for examples and tests; `mini fetch` is a tiny HTTP client for images without a shell | done |
| `mini-cni` + host-gw route agent | Pod networking: bridge, veth, IP allocation, loopback, cross-node routes, egress masquerading | done |
| `mini node-init`, pause image | Node container entrypoint (cgroup delegation, containerd configuration and start, then the kubelet); Pod sandbox process | done |
| kube-proxy, cluster DNS, Ingress | iptables rules for ClusterIP/NodePort; service discovery | M6 |

**Open-source components used as-is**:

| Component | Use | Status |
| --- | --- | --- |
| [etcd](https://etcd.io) v3.5.17 | The only state store (accessed through its v3 JSON gateway) | in use |
| Docker Engine + Compose | Hosts the control-plane containers and the node containers (the "machines"); never runs Pods | in use |
| [containerd](https://containerd.io) + [runc](https://github.com/opencontainers/runc) | Container runtime inside each node (Ubuntu packages); containerd's built-in CRI plugin is the CRI server, the kubelet is the client | in use |
| [crictl](https://github.com/kubernetes-sigs/cri-tools) | Debugging tool in the node image (`crictl ps`), used by e2e tests | in use |
| Ubuntu 24.04 base image, iptables, iproute2 | Operating environment of the node containers | in use |
| Linux kernel features | namespaces, cgroups, netfilter, bridges, veth | in use |

**Go dependencies**: `gopkg.in/yaml.v3` (manifests), `google.golang.org/grpc` and
`k8s.io/cri-api` (the CRI protocol definitions only, no implementation); tests use
Ginkgo/Gomega. The project depends on neither client-go nor any Kubernetes component's
implementation.

## Code structure

Directories and package names follow the Kubernetes source tree, so the two can be read
side by side (full mapping in
[v2 design specification §15](docs/superpowers/specs/2026-10-14-mini-k8s-v2-design.md)):

```text
cmd/mini/                    multi-call binary: mini kube-apiserver | kube-controller-manager | kube-scheduler |
                             node-init | gateway | demo | pause | fetch; as mini-cni/loopback the CNI plugin
cmd/kk/                      CLI (kubectl plus cluster lifecycle)
build/images/<name>/         Dockerfiles: mini (components, demo), node, pause, etcd
pkg/apis/{meta,core,apps}/v1 resource types (metav1, corev1, appsv1) and validation
pkg/api/resource, util/intstr quantities ("500m", "64Mi") and int-or-string values
pkg/labels, fields, watch    selectors and watch event types (as in apimachinery)
pkg/scheme                   resource registry
pkg/apiserver                generic API server (as k8s.io/apiserver)
pkg/registry/<group>/<res>   per-resource strategies: defaults, validation, mutability, field selectors
pkg/storage/etcd3            etcd storage
pkg/client/...               client, informers, workqueue, record (as client-go)
pkg/controller/<name>        deployment, garbagecollector, namespace, nodeipam controllers
pkg/scheduler                scheduler
pkg/kubelet                  node agent; kuberuntime (CRI client), prober, dns (node-local forwarder)
pkg/network/{cni,hostgw}     CNI plugin and host-gw route agent
pkg/nodeinit                 node container entrypoint
pkg/gateway                  HTTP gateway
test/e2e, test/integration   end-to-end and integration tests
```

## Prerequisites

| Requirement | Notes |
| --- | --- |
| Go 1.26 or later | `go` must be on your `PATH`. |
| Docker Engine (Linux containers) | A local, rootful Docker Engine on Linux that may run **privileged** containers, with **cgroup v2** (`stat -fc %T /sys/fs/cgroup` prints `cgroup2fs`). |
| Docker Compose v2 | `docker compose version` must work. |
| `make`, `curl`, `tar` | Used to build binaries and obtain etcd. |
| Local Docker access | A local Unix socket; TCP, SSH and remote Docker endpoints are rejected. |
| Free host ports | `127.0.0.1:18000` (API) and `127.0.0.1:18080` (gateway). |

| Environment | Status |
| --- | --- |
| Ubuntu 24.04 (WSL2, x86_64) with Docker Engine 29.9 installed inside WSL, Compose v5.6, Go 1.27 | ✅ verified end to end |
| Other Linux distributions (x86_64/arm64) with cgroup v2, rootful Docker | expected to work, not individually verified |
| WSL2 with Docker Desktop's WSL integration | not verified |
| macOS with Docker Desktop | not yet verified for kind-style nodes (Docker Desktop uses cgroup v2 and allows privileged containers); component tests need `bin/etcd`, a Linux binary, so they are skipped on macOS |
| cgroup v1 hosts, rootless Docker, native Windows | not supported |

On WSL, clone the repository into the WSL Linux filesystem (for example `~/`), not into a
mounted Windows drive, where builds are much slower and file permissions behave differently.

The first run needs network access to Go modules (`proxy.golang.org`; set `GOPROXY` to use
a mirror), to the etcd download (`storage.googleapis.com`), and, for the node image, to
Docker Hub (`ubuntu:24.04`), the Ubuntu package archive (containerd, runc, iptables,
iproute2) and GitHub (crictl). Later runs reuse the cached images. Pods use locally built
images that `kk` loads into the nodes, so no registry pulls are needed for the examples.

One project cluster per Docker Engine: the Compose project, network, volume names and
published ports are fixed. A clone in another directory takes over the same cluster and the
same etcd data (see step 5 to reset). The host build architecture must match the Linux
container architecture.

> **Trusted local use only.** Node containers are privileged, which grants them extensive
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
from `storage.googleapis.com` with host-side curl), builds four images from
`build/images/` (components, node, pause and etcd; the node image takes about a minute
the first time), starts the Compose services, loads the component and pause images into
each node's containerd, and waits up to 90 seconds for a healthy API and fresh Ready
heartbeats from both workers.

```text
API healthy
NAME       STATUS   ROLES    AGE   VERSION
worker-1   Ready    worker   5s    mini-kubelet/v2
worker-2   Ready    worker   5s    mini-kubelet/v2
```

`kk get nodes -o wide` also shows each node's address and `containerd://` runtime.

### 3. Deploy the example

```sh
./bin/kk apply -f examples/demo.yaml
./bin/kk get deployments,pods,services -o wide
curl -sS --fail --retry 20 --retry-delay 1 --retry-all-errors http://127.0.0.1:18080/
```

The [example](examples/demo.yaml) creates a two-replica Deployment and a Service; its
container has CPU/memory requests and limits and readiness/liveness probes. The Pods get
addresses from their nodes' Pod CIDRs (`10.244.x.y`, see `-o wide`); the gateway
round-robins across ready Pods; responses look like `hello from web-997d7b744-f8shs` (the
Pod name is its hostname). The gateway may answer `503` while the Pods start; the curl
command above retries.

### 4. Observe and operate

```sh
export PATH="$PWD/bin:$PATH"             # use kk directly from now on

kk get pods -w                           # watch changes live (Ctrl-C to stop)
kk describe deployment web               # details and related events
kk get events
kk logs -f POD_NAME                      # a name printed by kk get pods
kk logs -p POD_NAME                      # the previous instance after a restart
kk exec POD_NAME -- /mini fetch http://127.0.0.1:8080/   # run a command in the Pod
kk exec POD_NAME -- /mini fetch http://127.0.0.1:8080/fail-ready  # its readiness probe now fails:
kk get pods                              # 0/1 after ~2s; the gateway only uses the other Pod
kk exec POD_NAME -- /mini fetch http://127.0.0.1:8080/recover
kk exec POD_NAME -- /mini cat /sys/fs/cgroup/memory.max          # the 64Mi limit: 67108864
kk scale deployment web --replicas 3
kk delete pod POD_NAME                   # graceful; the Deployment replaces it
kk delete pod POD_NAME --force           # removed from the API immediately

kk create namespace team
kk apply -n team -f examples/hello.yaml  # deploy into another namespace (no Service)
kk get pods -A                           # list across namespaces
kk delete namespace team                 # deletes everything inside

kk apply -f examples/init-and-sidecar.yaml   # init container + two containers sharing a network and a volume
kk get pod web-with-sidecar              # Init:0/1, then 2/2 Running
kk exec web-with-sidecar -c sidecar -- /mini fetch http://127.0.0.1:8080/
kk exec web-with-sidecar -c sidecar -- /mini write /shared/note "written by the sidecar"
kk exec web-with-sidecar -c web -- /mini cat /shared/note

docker exec mini-k8s-worker-1-1 crictl ps    # look inside a node, like on a real machine
```

Pods run inside the nodes, so to run your own image, build it with Docker and copy it
into the nodes (like `kind load docker-image`); Pods that use it need no registry:

```sh
docker build -t my-app:v1 PATH_TO_YOUR_APP
kk image load my-app:v1
```

### 5. Stop the cluster

```sh
./bin/kk cluster down
```

This stops the reconcilers and the nodes (their Pods stop with them) and removes the
Compose containers, **keeping the etcd data, the nodes' disks and the token**. A later
`cluster up` restores saved resources and the kubelets recreate their Pods. To remove the
example for good, run `kk delete -f examples/demo.yaml` first.

To **start from scratch** (dropping every resource in the cluster and the images loaded
into the nodes), also delete the volumes:

```sh
./bin/kk cluster down --volumes
./bin/kk cluster up
```

## Supported subset

| Resource | apiVersion | Supported fields and behaviour |
| --- | --- | --- |
| Namespace | `v1` | Create and delete (Terminating → contents removed → deleted). `default` and `kube-system` exist at startup. |
| Node | `v1` | Registered and heartbeated by kubelets with `capacity`/`allocatable` (cpu, memory, pods); `spec.podCIDR` assigned once by node IPAM; `spec.unschedulable` is honoured by the scheduler. |
| Pod | `v1` | `containers` (one or more) and `initContainers`: `name`, `image`, `command`, `args`, `env`, `ports`, `resources` (cpu, memory), `livenessProbe`/`readinessProbe`/`startupProbe` (not for init containers), `volumeMounts`; `volumes` (`emptyDir` with `medium`/`sizeLimit`, `hostPath` with `type`); `hostNetwork`; `restartPolicy` (Always, OnFailure, Never); `terminationGracePeriodSeconds`; `nodeName`. The spec is immutable. Subresources `log` and `exec`. |
| Deployment | `apps/v1` | `replicas` (default 1), `selector` (matchLabels/matchExpressions, immutable), `template` (`restartPolicy` must be Always). Template changes replace Pods (Recreate). |
| Service | `v1` | One cluster-wide HTTP gateway Service: `selector`, one `port: 80`, `targetPort`. |
| Event | `v1` | Recorded by components, queryable, expire after one hour. |

`kk apply` uses JSON merge patch: fields in the manifest overwrite server values, but
**fields removed from a manifest are not removed from the server** (there is no
last-applied three-way merge). Every document of a batch is validated with a
server-side dry run first, so one bad document changes nothing; applying the batch is
still not a cross-resource transaction.

## Current limitations (addressed by upcoming milestones)

- No ConfigMaps, Secrets or persistent volumes yet (M8); no `subPath` or `hostPort`; the
  scheduler ignores resource requests (resource-aware scheduling is M7). `kk exec` runs a
  command to completion and returns its output; there is no interactive session (stdin/tty),
  unlike Kubernetes' streaming exec.
- Deployments manage Pods directly: no ReplicaSets, rolling updates or rollbacks.
- One HTTP gateway Service; no ClusterIP, cluster DNS, NodePort or Ingress (Pods resolve
  the Docker network's names, e.g. `apiserver`, through their node).
- Two worker nodes, fixed in `compose.yaml`; nodes are containers sharing the host kernel.
- Lost nodes are not marked NotReady and their Pods are not evicted; no resource-aware
  scheduling, taints or affinity.
- No DaemonSets, Jobs, CronJobs, StatefulSets, HPA, RBAC or control-plane HA.

## Development

```sh
make vet
make test                # unit and component tests (a throwaway etcd from bin/etcd)
go test -race ./...
```

Integration tests (`test/integration`, and package tests using `test/integration/framework`)
run the real API server in-process against a temporary etcd started from `bin/etcd`
(prepared by `make prepare-etcd`); they are skipped when it is missing.

End-to-end tests are a Ginkgo v2 suite modelled after Kubernetes' `test/e2e` (see
[test/e2e/README.md](test/e2e/README.md)). Each spec runs in its own namespace, which is
deleted afterwards:

```sh
make e2e                          # every spec, including Disruptive ones
make e2e-quick                    # skip specs labelled Disruptive
make e2e E2E_FOCUS='sig-apps'     # run a subset by name
```

**Run them only against a disposable project cluster.** Disruptive specs restart the
control plane and a node container and run `cluster down/up`. The gateway serves a single Service for now, so
gateway specs require that no other Service exists and are skipped otherwise. If you
deployed the quick-start example, delete it before running the full suite:

```sh
./bin/kk delete -f examples/demo.yaml
make e2e
```

Every new feature ships with e2e specs.

## Troubleshooting

| Symptom | What to check |
| --- | --- |
| `go`, `docker` or `make` not found | Install the prerequisites and add them to your `PATH`. |
| Docker commands hang | Check the engine itself (for example `systemctl status docker`) before restarting it. |
| etcd download fails | Check access to `storage.googleapis.com`, or provide a Linux etcd v3.5.17 binary at `bin/etcd`. |
| Pod shows `ErrImagePull`/`ImagePullBackOff` | `kk describe pod NAME` shows the events. Nodes pull public images themselves; local or private images must be copied in with `kk image load IMAGE`. |
| Node stays `NotReady` | `kk describe node NAME` shows the reason (runtime, network plugin or sandbox image not ready); `docker logs mini-k8s-worker-1-1` shows containerd and kubelet output; `docker exec mini-k8s-worker-1-1 crictl pods` lists sandboxes. |
| Pod shows `CreatePodSandboxError` | The message comes from containerd and the CNI plugin; check the node's logs as above. |
| Node build fails at `apt-get` or the crictl download | The node image needs Docker Hub, the Ubuntu archive and GitHub on first build; retry or configure a proxy/mirror. |
| Node containers exit at start with a cgroup error | The host must use cgroup v2 and allow privileged containers. |
| Pod shows `CrashLoopBackOff` | `kk logs NAME` shows the application output and `kk logs -p NAME` the previous run's; restarts back off from 1s up to 30s. |
| Gateway returns `503` | Wait for ready Pods; check the Service selector, targetPort and node heartbeats. |
| `Error from server (Invalid)` | Fix the field named in the message; unknown fields are rejected too. |
| Port or project-name conflict | Free the ports or stop the conflicting project; `MINI_API` alone does not change Compose ports. |
| `only one Service is supported` | The gateway serves a single Service for now; in other namespaces use Service-less manifests such as `examples/hello.yaml`. |
| A fresh clone shows old resources | One Docker Engine hosts one cluster and one etcd volume; reset with `kk cluster down --volumes` as in step 5. |

The first start creates `.mini-k8s/token` with mode `0600`; keep it private and do not
delete it while the cluster runs. For manual Compose diagnostics, export the variable
`kk` normally provides (never print the token):

```sh
export MINI_TOKEN="$(cat .mini-k8s/token)"
docker compose ps
docker compose logs --tail=100 controller
unset MINI_TOKEN
```

Only for a diagnosed credential-helper hang, `MINI_DOCKER_NO_CREDENTIALS=1 ./bin/kk cluster up`
uses a temporary anonymous Docker configuration without modifying yours.

## Roadmap

| Milestone | Scope | Status |
| --- | --- | --- |
| M0 | Engineering baseline, Linux docs | ✅ |
| E2E | Ginkgo e2e framework | ✅ |
| M1 | API machinery: Kubernetes-style REST, namespaces, watch, informers/work queues, GC, graceful deletion, events | ✅ |
| M2 | kind-style nodes: containerd inside each node, kubelet managing Pods over CRI, home-grown CNI (host-gw routing) | ✅ |
| M3a | Pods: multiple and init containers, restart policies and terminal phases, `kk exec` | ✅ |
| M3b | Pods: liveness/readiness/startup probes, resource requests and limits, QoS, node capacity | ✅ |
| M3c | Pods: volumes (emptyDir, hostPath), hostNetwork | ✅ |
| M4 | Self-hosted control plane: control-plane node, static/mirror Pods, etcd and control-plane components in kube-system | planned |
| M5 | ReplicaSets, rolling updates and rollbacks | planned |
| M6 | ClusterIP, Endpoints, iptables kube-proxy, DNS, NodePort, Ingress | planned |
| M7 | Scheduling framework, node leases and eviction, taints, cordon/drain | planned |
| M8 | ConfigMaps, Secrets, volumes, PVCs | planned |
| M9 | DaemonSets, Jobs, CronJobs, StatefulSets | planned |

## Design documentation

- [v2 design specification (Chinese)](docs/superpowers/specs/2026-10-14-mini-k8s-v2-design.md): architecture and milestones.
- [M3c volumes and host network plan (Chinese)](docs/superpowers/plans/2026-10-15-m3c-volumes-hostnetwork.md)
- [M3b probes and resources plan (Chinese)](docs/superpowers/plans/2026-10-15-m3b-probes-resources.md)
- [M3a multi-container Pods plan (Chinese)](docs/superpowers/plans/2026-10-15-m3a-multi-container.md)
- [M2 kind-style nodes plan (Chinese)](docs/superpowers/plans/2026-10-15-m2-kind-nodes.md)
- [M1 API machinery plan (Chinese)](docs/superpowers/plans/2026-10-14-m1-api-machinery.md)
- [kind-style node feasibility spike (Chinese)](docs/superpowers/spikes/2026-10-15-kind-node-spike.md): verified recipe for containerd in node containers, CRI and host-gw networking
- [M0 baseline plan (Chinese)](docs/superpowers/plans/2026-10-14-m0-baseline.md)
- [v1 design specification](docs/superpowers/specs/2026-10-14-mini-k8s-design.md) and
  [v1 implementation plan](docs/superpowers/plans/2026-10-14-mini-k8s-plan.md) (Chinese):
  records of the original 30-minute version.
