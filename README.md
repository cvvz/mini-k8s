# mini-k8s

**English** | [简体中文](README.zh-CN.md)

A small, Kubernetes-inspired container orchestrator written in Go. It implements
its own API server, scheduler, controller manager, kubelets, HTTP service proxy,
and a command-line client called **`kk`**.

The goal is to make the desired-state reconciliation loop easy to understand:
submit a Deployment, schedule its Pods, run real containers, and recover when a
container exits or a managed Pod is deleted. This is not a wrapper around an
existing Kubernetes distribution, a Kubernetes-compatible API, or a production
cluster.

## What it does

- Persists resources in etcd, with UID-based identity and optimistic concurrency.
- Schedules Pods across two logical workers using fresh heartbeats and assigned
  Pod counts.
- Maintains Deployment replicas and supports scaling from 0 to 20.
- Runs real Docker containers, restarts exited containers, and retrieves logs.
- Exposes one HTTP Service at `http://127.0.0.1:18080`.
- Provides `kk` commands for cluster lifecycle and resource management.

```text
kk --> API Server --> etcd
           ^
           | resource reads / writes
           |
   Scheduler + Controller Manager + kubelets
                                      |
                                  Docker Engine
                                      |
                                workload containers
                                      ^
                                      |
localhost:18080 --> HTTP Service Proxy
```

Control-plane components, two kubelets, and workload containers share one local
Docker Engine and a dedicated Docker network. Workers are **logical nodes**, not
independent machines or isolated container runtimes.

## Prerequisites

| Requirement | Notes |
| --- | --- |
| Go 1.25 or later | `go` must be available on your `PATH`. |
| Docker Engine (Linux containers) | A local, rootful Docker Engine on Linux. The worker adapter requires the daemon to accept Docker API **v1.41**. |
| Docker Compose v2 | `docker compose version` must work. |
| `make`, `curl`, and `tar` | Used to build binaries and obtain etcd. |
| Local Docker access | Your user must be able to use a local Unix socket. TCP, SSH, and remote Docker endpoints are rejected. |
| Available host ports | `127.0.0.1:18000` for the API and `127.0.0.1:18080` for the demo service. |

Verified end to end on Ubuntu 24.04 (WSL2, x86_64) with Docker Engine 29.9,
Compose v5.6, and Go 1.27. The original v1 was also exercised on macOS ARM64 with
Docker Desktop 27.4. Rootless Docker is not a validated configuration.

Only one cluster per Docker Engine is supported: Compose project names, network
names, and published ports are fixed. The host build architecture must match the
Linux container architecture.

> **Trusted local use only.** Kubelets mount the Docker socket, which grants
> extensive control over the Docker host. The API uses a shared bearer token,
> not TLS or RBAC. Do not expose it or run untrusted workloads.

## Quick start

Clone the project with Git, or download its source archive from
[GitHub](https://github.com/cvvz/mini-k8s):

```sh
git clone https://github.com/cvvz/mini-k8s.git
cd mini-k8s
```

Run the following commands from the project root, which contains `go.mod`,
`Makefile`, and `compose.yaml`. There is no need to install `kk` globally.

### 1. Check your environment

Start your local Docker Engine, then run:

```sh
go version
docker version
docker compose version
docker ps
```

These commands must complete successfully. If `docker ps` hangs, fix the Docker
runtime first; a successful `docker info` alone does not establish that the
container engine is responsive.

### 2. Build and start the cluster

```sh
make build
./bin/kk cluster up
./bin/kk cluster status
```

`cluster up` builds the components, prepares etcd, builds two local images, and
starts the Compose services. It then waits up to 90 seconds for the authenticated
API and fresh `Ready` heartbeats from both workers; the build/download time is
separate from this readiness timeout.

The first run needs access to Go modules and the official etcd binary download
at `storage.googleapis.com`. It downloads etcd v3.5.17 using **host-side curl**.
Both images are built `FROM scratch`, and the example reuses the local image, so
the default demo does not require Docker Hub image pulls.

Expected cluster status:

```text
API healthy
KIND  NAME      PHASE  NODE  REPLICAS
Node  worker-1  Ready        -
Node  worker-2  Ready        -
```

### 3. Deploy and access the example

```sh
./bin/kk apply -f examples/demo.yaml
./bin/kk get deployments
./bin/kk get pods
./bin/kk get services

curl --fail --retry 20 --retry-delay 1 --retry-connrefused --max-time 5 \
  http://127.0.0.1:18080/
```

The [example](examples/demo.yaml) creates a two-replica Web Deployment and one
Service. Pod creation is asynchronous: Pods may initially be `Pending`, and the
gateway may return `503` until backends are available.

Once running, the response looks like:

```text
hello from <container-hostname>
```

### 4. Scale and inspect

```sh
./bin/kk scale deployment web --replicas 3
./bin/kk get pods
./bin/kk get deployments web -o json
```

Replace `POD_NAME` below with a name printed by `get pods`:

```sh
./bin/kk describe pod POD_NAME
./bin/kk logs POD_NAME
./bin/kk delete pod POD_NAME
```

Deleting a Deployment-managed Pod causes the controller to create a replacement.
`describe` prints the stored resource, including its status and failure reason;
`logs` returns a non-following snapshot of container stdout/stderr.

Optionally add the local CLI to your current shell:

```sh
export PATH="$PWD/bin:$PATH"
kk get pods
```

Keep the binary in the project's `bin/` directory; `kk` uses the source tree and
project-local credentials for cluster operations.

### 5. Stop or remove the example

To stop the cluster **while keeping its desired state**:

```sh
./bin/kk cluster down
```

The command stops reconcilers, removes this cluster's workload containers, and
stops Compose. It preserves the etcd volume and local token. A later
`./bin/kk cluster up` restores the saved resources and recreates their workloads.

To remove the example permanently, delete its resources **before** stopping:

```sh
./bin/kk delete -f examples/demo.yaml
./bin/kk cluster down
```

etcd persistence preserves resource definitions, not application filesystem data.
Persistent application volumes are not implemented.

## Supported subset

| Resource | Supported behavior |
| --- | --- |
| Node | Worker registration and heartbeats; not created through user manifests. |
| Pod | One container, with `name`, `image`, `command`, `args`, and string `env` values. |
| Deployment | Explicit `replicas: 0..20`, label selector and Pod template; directly owns Pods without a ReplicaSet. |
| Service | One HTTP gateway for the entire cluster; one `port: 80` and numeric `targetPort: 1..65535`. |

Only the `default` namespace is supported. Unknown fields and unsupported
operations are errors, not ignored input. Existing specs and labels are immutable,
except for Deployment replicas. Changing an image or Service selector requires
deletion and recreation; there is no rolling update.

`apply -f` accepts multi-document YAML and validates every document before making
API calls. Applying the batch is **not transactional**: an API failure can leave
earlier documents applied. Updates and deletes detect version conflicts rather
than forcefully overwriting concurrent changes.

There is no `kubectl` compatibility, Watch API, DNS, ClusterIP, NodePort, CNI,
Ingress, health probes, resource-aware scheduling, node-failure migration,
ConfigMap/Secret, application volumes, RBAC, HPA, or control-plane HA. Stopping a
logical kubelet does not stop the containers it previously created.

## Development

```sh
make vet
make test
go test -race ./...
```

End-to-end tests are a Ginkgo v2 suite modelled after Kubernetes' `test/e2e`
(see [test/e2e/README.md](test/e2e/README.md)). They need a running cluster and
`docker` on `PATH`:

```sh
make e2e                          # every spec, including disruptive ones
make e2e-quick                    # skip specs labelled Disruptive
make e2e E2E_FOCUS='sig-apps'     # run a subset by name
```

**Run them only against a disposable project cluster.** Specs create uniquely
named resources and clean them up, and check API state, real Docker containers,
HTTP traffic, logs, self-healing, version conflicts, and validation. Disruptive
specs restart the control plane and run `cluster down/up`. Every new feature
ships with e2e specs.

## Troubleshooting

| Symptom | What to check |
| --- | --- |
| `go`, `docker`, or `make` not found | Install the prerequisites and add their installation directories to your own `PATH`. |
| Docker commands hang | Check the actual engine, not just API metadata (for example `systemctl status docker`). Restart only after considering other workloads. |
| etcd download fails | Check host network/proxy access to `storage.googleapis.com`, or provide a matching Linux etcd v3.5.17 binary at `bin/etcd`. |
| Pod fails to pull an image | Use `describe pod` to inspect the error. The worker can use cached images or pull public images; private images must be pre-pulled into the same Engine. |
| Gateway returns `503` | Wait for running Pods; check Service labels, target port, and worker heartbeats. `Running` is not an application health probe. |
| Unsupported field or immutable-spec error | Use the documented subset. Only replicas can be updated in place. |
| Port or project-name conflict | Free the required ports or stop the conflicting project; changing `MINI_API` alone does not change Compose port mappings. |
| Docker API version rejected | The worker uses v1.41. Changing the Docker CLI's `DOCKER_API_VERSION` does not change that worker protocol. |

`bin/etcd` is reused without version/architecture verification. Replace it when
changing architecture. This cache is not a portable prebuilt release.

The first startup creates `.mini-k8s/token` with mode `0600`. Keep it private and
do not delete it while the cluster is running. `MINI_API` overrides the CLI's API
address only; the normal quick start uses its default.

For Compose diagnostics, set the same variables that `kk` normally provides
automatically. Do not enable shell tracing or print the token:

```sh
export MINI_TOKEN="$(cat .mini-k8s/token)"
export MINI_DOCKER_SOCKET=/var/run/docker.sock
docker compose ps
docker compose logs --tail=100
unset MINI_TOKEN MINI_DOCKER_SOCKET
```

Substitute the actual local Engine socket path if it differs.

For a specifically diagnosed credential-helper hang, optional
`MINI_DOCKER_NO_CREDENTIALS=1 ./bin/kk cluster up` uses a temporary anonymous Docker
configuration without modifying your existing one. It does not repair an
unresponsive daemon and does not carry private-registry credentials or your
existing proxy/builder configuration.

## Roadmap

mini-k8s is evolving toward a compact but complete Kubernetes: Kubernetes-style
REST and Watch, namespaces, ReplicaSets and rolling updates, multi-container Pods
with probes, ClusterIP Services with DNS, node lifecycle, ConfigMaps/Secrets,
volumes, and more workload types. See the
[v2 design specification (Chinese)](docs/superpowers/specs/2026-10-14-mini-k8s-v2-design.md)
for the architecture and milestone status.

## Design documentation

- [Design specification (Chinese)](docs/superpowers/specs/2026-10-14-mini-k8s-design.md):
  goals, decisions, resource contracts, architecture, limitations, and acceptance.
- [Implementation plan (Chinese)](docs/superpowers/plans/2026-10-14-mini-k8s-plan.md):
  retrospective task breakdown, interfaces, dependencies, and verification.
- [v2 design specification (Chinese)](docs/superpowers/specs/2026-10-14-mini-k8s-v2-design.md):
  the "compact but complete" architecture and milestones.
- [M0 baseline plan (Chinese)](docs/superpowers/plans/2026-10-14-m0-baseline.md).
