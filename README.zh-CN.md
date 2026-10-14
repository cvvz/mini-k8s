# mini-k8s

[English](README.md) | **简体中文**

mini-k8s 是一个用 Go 从零实现的精简版 Kubernetes。它自研 API Server、调度器、
Controller Manager、kubelet、HTTP 网关和命令行工具 **`kk`**，只复用 etcd 做存储，
用本地 Docker Engine 运行真实容器。

目标是“精简但五脏俱全”：用尽量少的代码把 Kubernetes 的核心机制完整、可观察地跑起来，
包括声明式 API、Watch、控制循环、调度、节点代理和级联删除。它不是现成发行版的封装，
不兼容 `kubectl`，也不适用于生产环境。

## 主要能力

- **K8s 风格 API**：`/api/v1/namespaces/{ns}/pods`、`/apis/apps/v1/...`，
  资源对象包含 `metadata`/`spec`/`status`，错误以 `Status` 对象返回，并提供 discovery 接口。
- **API 机制**：命名空间；UID 与 `resourceVersion` 乐观并发；`generation`/`observedGeneration`；
  `generateName`；严格解码（未知字段直接报错）；JSON merge patch；`dryRun`；
  label/field selector；**Watch**（etcd watch 流式推送，selector 变化时转为 DELETED 事件）。
- **删除语义**：finalizer；Pod 优雅终止（SIGTERM，宽限期后 SIGKILL）；强制删除；
  ownerReferences 与垃圾回收（Background / Foreground / Orphan 三种传播策略）；
  删除 Namespace 时级联清空其中的资源。
- **控制面**：Informer + WorkQueue 事件驱动；Deployment 控制器（模板变化时按 Recreate
  策略替换 Pod，并上报 status）；GC 控制器；Namespace 控制器；
  调度器（通过 binding 子资源绑定，不可调度时写入条件和事件）。
- **kubelet**：节点注册与心跳；每个 Pod 一个 worker；真实 Docker 容器；
  崩溃退避（CrashLoopBackOff）；镜像拉取失败上报（ErrImagePull/ImagePullBackOff）；
  容器级状态；日志（支持 follow/tail）；清理孤儿容器。
- **可观测性**：Events（同类事件聚合计数，1 小时后由 etcd lease 自动过期），
  `kk describe` 会展示对象关联的事件。
- **kk**：`get`（`-o wide|json|yaml|name`、`-l`、`--field-selector`、`-A`、`-w`）、`describe`、
  `apply`/`create`（先整体 dry-run 再写入）、`delete`（`--grace-period`、`--force`、
  `--cascade`）、`logs -f`、`scale`、`api-resources`、`version` 以及集群生命周期命令。

```text
                         kk
                          │ HTTP + Bearer token（127.0.0.1:18000）
                          ▼
    etcd ◀──────── kube-apiserver ◀─────────── list / watch / write ────────────┐
                          ▲                                                    │
       ┌──────────────────┼───────────────────┬───────────────────┐            │
   scheduler     controller-manager       kubelet × 2          gateway         │
                 (deployment / gc /        │ docker.sock          │           │
                  namespace)               ▼                      ▼           │
                                     业务容器 ◀──────── 127.0.0.1:18080 ───────┘
```

所有组件是同一个二进制 `mini` 的不同角色，与业务容器共享一个本地 Docker Engine 和专用
Docker 网络。worker 是**逻辑节点**，不是独立机器。

## 环境要求

| 依赖 | 要求 |
| --- | --- |
| Go 1.25 或更新版本 | `go` 已加入 `PATH`。 |
| Docker Engine（Linux 容器） | Linux 上的本地 rootful Docker Engine。worker 要求服务端接受 Docker API **v1.41**。 |
| Docker Compose v2 | `docker compose version` 可正常执行。 |
| `make`、`curl`、`tar` | 用于构建程序和获取 etcd。 |
| 本地 Docker 访问权限 | 当前用户能访问本地 Unix socket；不接受 TCP、SSH 或远程 Docker endpoint。 |
| 空闲端口 | API 使用 `127.0.0.1:18000`，网关使用 `127.0.0.1:18080`。 |

已在 Ubuntu 24.04（WSL2，x86_64）、Docker Engine 29.9、Compose v5.6、Go 1.27 上完成
端到端验证。rootless Docker 不属于已验证配置。

每个 Docker Engine 只支持一套本项目集群。宿主机构建架构必须与 Linux 容器架构一致。

> **仅用于可信的本地实验。** kubelet 挂载 Docker socket，具有较高的 Docker 主机权限。
> API 使用共享 bearer token，没有 TLS 或 RBAC。不要对外暴露，也不要运行不可信的工作负载。

## Quick Start

```sh
git clone https://github.com/cvvz/mini-k8s.git
cd mini-k8s
```

以下命令都在项目根目录执行。

### 1. 检查环境

```sh
go version
docker version
docker compose version
docker ps
```

### 2. 构建并启动集群

```sh
make build
./bin/kk cluster up
./bin/kk cluster status
```

`cluster up` 会构建组件、准备 etcd（首次运行时用宿主机 curl 从 `storage.googleapis.com`
下载 v3.5.17）、构建两个 `FROM scratch` 本地镜像，并启动 Compose 服务。之后最多等待
90 秒，直到 API 健康、两个 worker 都有新鲜的 Ready 心跳。

```text
API healthy
NAME       STATUS   ROLES    AGE   VERSION
worker-1   Ready    worker   5s    mini-kubelet/v2
worker-2   Ready    worker   5s    mini-kubelet/v2
```

### 3. 部署示例

```sh
./bin/kk apply -f examples/demo.yaml
./bin/kk get deployments,pods,services -o wide
curl --fail --retry 20 --retry-delay 1 --retry-all-errors http://127.0.0.1:18080/
```

[示例](examples/demo.yaml)创建一个两副本的 Deployment 和一个 Service。网关会把请求
轮询转发到就绪的 Pod，响应形如 `hello from web-997d7b744-f8shs`（Pod 名即主机名）。

### 4. 观察与操作

```sh
export PATH="$PWD/bin:$PATH"             # 之后可直接使用 kk

kk get pods -w                           # 实时观察变化（Ctrl-C 退出）
kk describe deployment web               # 详情与关联事件
kk get events
kk logs -f POD_NAME                      # POD_NAME 换成 get pods 输出的名字
kk scale deployment web --replicas 3
kk delete pod POD_NAME                   # 优雅终止，Deployment 会补建新 Pod
kk delete pod POD_NAME --force           # 立即从 API 删除

kk create namespace team
kk apply -n team -f examples/demo.yaml   # 命名空间隔离（网关仍只服务一个 Service）
kk delete namespace team                 # 级联删除其中所有资源
```

### 5. 停止集群

```sh
./bin/kk cluster down
```

该命令先停止协调组件，再清理本项目的业务容器并停止 Compose，**保留 etcd 数据卷和 token**。
之后执行 `cluster up` 会恢复保存的资源。若想彻底移除示例，先执行
`kk delete -f examples/demo.yaml` 再停止集群。

## 支持范围

| 资源 | apiVersion | 支持的字段与行为 |
| --- | --- | --- |
| Namespace | `v1` | 创建、删除（Terminating → 清空内容 → 删除）。启动时自动创建 `default`、`kube-system`。 |
| Node | `v1` | 由 kubelet 注册并心跳；`spec.unschedulable` 会被调度器遵守。 |
| Pod | `v1` | **单容器**：`name`、`image`、`command`、`args`、`env`、`ports`；`terminationGracePeriodSeconds`；`nodeName`。创建后 spec 不可变。 |
| Deployment | `apps/v1` | `replicas`（默认 1）、`selector`（matchLabels/matchExpressions，创建后不可变）、`template`。模板变化时按 Recreate 策略替换 Pod。 |
| Service | `v1` | 整个集群一个 HTTP 网关 Service：`selector`，一个 `port: 80`，`targetPort`。 |
| Event | `v1` | 由组件记录，可查询；1 小时后过期。 |

`kk apply` 采用 JSON merge patch：清单里的字段会覆盖服务端的值，**从清单中删掉的字段不会
从服务端删除**（没有 last-applied 三方合并）。批量应用前会对所有文档做服务端 dry-run，
所以任何一个文档有误都不会改动集群；但真正写入时仍不是跨资源事务。

## 当前限制（后续里程碑会解决）

- Pod 只支持一个容器，没有探针、资源限制、ConfigMap/Secret 和卷。
- Deployment 直接管理 Pod，没有 ReplicaSet，也没有滚动更新和回滚。
- 只有一个 HTTP 网关 Service，没有 ClusterIP、DNS、NodePort 和 Ingress。
- 节点失联不会被标记为 NotReady，也不会驱逐 Pod；没有资源感知调度、污点和亲和性。
- 没有 DaemonSet、Job、CronJob、StatefulSet、HPA、RBAC 和控制面高可用。

## 开发与测试

```sh
make vet
make test                # 单元测试 + 组件测试（使用 bin/etcd 启动临时 etcd）
go test -race ./...
```

组件测试会在进程内启动真实的 API Server，后端是临时 etcd。etcd 二进制取自
`bin/etcd`（`make prepare-etcd` 准备）；缺失时这些测试自动跳过。

端到端测试是仿照 Kubernetes `test/e2e` 的 Ginkgo v2 套件（见
[test/e2e/README.md](test/e2e/README.md)），每个用例在独立命名空间中运行，结束后删除：

```sh
make e2e                          # 全部用例，包括 Disruptive 用例
make e2e-quick                    # 跳过 Disruptive 用例
make e2e E2E_FOCUS='sig-apps'     # 按名称运行子集
```

**只在可重建的本项目实验集群上运行。** Disruptive 用例会重启控制面并执行
`cluster down/up`。网关类用例要求集群中没有其他 Service，否则会跳过。
每个新功能都必须附带 e2e 用例。

## 常见问题

| 现象 | 检查方式 |
| --- | --- |
| 找不到 `go`、`docker` 或 `make` | 安装依赖并加入 `PATH`。 |
| Docker 命令挂起 | 检查实际引擎（例如 `systemctl status docker`），重启前确认对其他工作负载的影响。 |
| etcd 下载失败 | 检查访问 `storage.googleapis.com` 的网络或代理；也可在 `bin/etcd` 预置 Linux etcd v3.5.17。 |
| Pod 显示 `ErrImagePull`/`ImagePullBackOff` | `kk describe pod NAME` 查看事件。私有镜像需事先拉入同一个 Engine。 |
| Pod 显示 `CrashLoopBackOff` | `kk logs NAME` 查看应用日志；kubelet 以 1s 起、最长 30s 的退避重启。 |
| 网关返回 `503` | 等待 Pod 就绪，检查 Service 的 selector、targetPort 和节点心跳。 |
| `Error from server (Invalid)` | 按报错中的字段路径修改清单；未知字段同样会被拒绝。 |
| 端口或项目名冲突 | 释放端口或停止冲突项目；只修改 `MINI_API` 不会改变 Compose 端口映射。 |

首次启动会生成权限为 `0600` 的 `.mini-k8s/token`，不要泄露或在集群运行中删除。
手动使用 Compose 排障时需要设置 `kk` 通常会自动提供的环境变量（不要打印 token）：

```sh
export MINI_TOKEN="$(cat .mini-k8s/token)"
export MINI_DOCKER_SOCKET=/var/run/docker.sock
docker compose ps
docker compose logs --tail=100 controller
unset MINI_TOKEN MINI_DOCKER_SOCKET
```

仅在确认是 Docker 凭据助手挂起时，可尝试 `MINI_DOCKER_NO_CREDENTIALS=1 ./bin/kk cluster up`。
它使用临时匿名 Docker 配置，不修改现有配置。

## 路线图

| 里程碑 | 内容 | 状态 |
| --- | --- | --- |
| M0 | 工程基线、Linux 文档 | ✅ |
| E2E | Ginkgo e2e 框架 | ✅ |
| M1 | API 机制：K8s 风格 REST、Namespace、Watch、Informer/WorkQueue、GC、优雅删除、Events | ✅ |
| M2 | Pod 与 kubelet：CRI 形状的运行时接口、pause 沙箱、多容器、init 容器、探针、资源限制、exec | 计划中 |
| M3 | 控制面自托管：control-plane 节点、static/mirror Pod，etcd 与控制面组件运行在 kube-system | 计划中 |
| M4 | ReplicaSet、滚动更新与回滚 | 计划中 |
| M5 | ClusterIP、Endpoints、kube-proxy、DNS、NodePort、Ingress | 计划中 |
| M6 | 调度框架、节点租约与驱逐、污点、cordon/drain | 计划中 |
| M7 | ConfigMap、Secret、卷、PVC | 计划中 |
| M8 | DaemonSet、Job、CronJob、StatefulSet | 计划中 |
| 可选 | kind 式节点：节点内 containerd + CRI + 自研 host-gw CNI | 未排期 |

## 设计文档

- [v2 设计规格](docs/superpowers/specs/2026-10-14-mini-k8s-v2-design.md)：整体架构与里程碑。
- [M1 API 机制计划](docs/superpowers/plans/2026-10-14-m1-api-machinery.md)
- [M0 工程基线计划](docs/superpowers/plans/2026-10-14-m0-baseline.md)
- [v1 设计规格](docs/superpowers/specs/2026-10-14-mini-k8s-design.md) 与
  [v1 实施计划](docs/superpowers/plans/2026-10-14-mini-k8s-plan.md)：最初 30 分钟版本的记录。
