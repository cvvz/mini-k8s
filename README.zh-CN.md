# mini-k8s

[English](README.md) | **简体中文**

mini-k8s 是一个用 Go 编写、借鉴 Kubernetes 设计的小型容器编排系统，自行实现
API Server、Scheduler、Controller Manager、kubelet、HTTP Service 代理，
以及名为 **`kk`** 的命令行工具。

项目旨在让声明式控制循环清晰可见：提交 Deployment，调度 Pod，启动真实容器，
并在容器退出或受管理的 Pod 被删除后自动恢复。它不是现成 Kubernetes 发行版的
封装，也不提供 Kubernetes API 兼容性，不适用于生产集群。

## 主要能力

- 使用 etcd 持久化资源，以 UID 标识对象，通过版本检查处理并发更新。
- 根据新鲜心跳与已分配 Pod 数，在两个逻辑 worker 之间调度。
- 维持 Deployment 的期望副本数，支持 0–20 副本扩缩容。
- 运行真实 Docker 容器、重启退出的容器、读取容器日志。
- 通过 `http://127.0.0.1:18080` 暴露一个 HTTP Service。
- 使用 `kk` 管理集群生命周期和资源。

```text
kk --> API Server --> etcd
           ^
           | 资源读写
           |
   Scheduler + Controller Manager + kubelets
                                      |
                                  Docker Engine
                                      |
                                    业务容器
                                      ^
                                      |
localhost:18080 --> HTTP Service Proxy
```

控制面、两个 kubelet 和业务容器共享一个本地 Docker Engine，使用专用 Docker
网络。worker 是**逻辑节点**，不是独立机器，也不拥有独立容器运行时。

## 环境要求

| 依赖 | 要求 |
| --- | --- |
| Go 1.25 或更新版本 | `go` 已加入 `PATH`。 |
| Docker Engine（Linux 容器） | Linux 上的本地 rootful Docker Engine。worker 要求服务端接受 Docker API **v1.41**。 |
| Docker Compose v2 | `docker compose version` 可正常执行。 |
| `make`、`curl`、`tar` | 用于构建程序和获取 etcd。 |
| 本地 Docker 访问权限 | 当前用户能访问本地 Unix socket；不接受 TCP、SSH 或远程 Docker endpoint。 |
| 空闲端口 | API 使用 `127.0.0.1:18000`，示例服务使用 `127.0.0.1:18080`。 |

已在 Ubuntu 24.04（WSL2，x86_64）、Docker Engine 29.9、Compose v5.6、Go 1.27
上完成端到端验证。v1 最初也在 macOS ARM64 + Docker Desktop 27.4 上验证过。
rootless Docker 不属于已验证配置。

每个 Docker Engine 只支持一套本项目集群：Compose 项目名、网络名和发布端口固定。
宿主机构建架构必须与 Linux 容器架构一致。

> **仅用于可信的本地实验。** kubelet 挂载 Docker socket，具有较高的 Docker
> 主机管理权限。API 使用共享 bearer token，没有 TLS 或 RBAC。
> 不要对外暴露，也不要运行不可信工作负载。

## Quick Start

使用 Git 克隆项目，也可以从 [GitHub](https://github.com/cvvz/mini-k8s) 下载源码压缩包：

```sh
git clone https://github.com/cvvz/mini-k8s.git
cd mini-k8s
```

以下命令请在包含 `go.mod`、`Makefile` 和 `compose.yaml` 的项目根目录执行，
无需全局安装 `kk`。

### 1. 检查环境

先启动本地 Docker Engine，然后执行：

```sh
go version
docker version
docker compose version
docker ps
```

这些命令应正常结束。如果 `docker ps` 挂起，应先解决 Docker 运行时问题；
仅有 `docker info` 返回信息，不代表实际容器引擎可用。

### 2. 构建并启动集群

```sh
make build
./bin/kk cluster up
./bin/kk cluster status
```

`cluster up` 会构建组件、准备 etcd、构建两个本地镜像并启动 Compose 服务。
随后最多等待 90 秒，检查认证 API 和两个 worker 的新鲜 `Ready` 心跳。
构建、下载耗时不包含在这段就绪等待时间内。

首次运行需要能够下载 Go 模块，以及访问 `storage.googleapis.com` 获取官方
etcd v3.5.17 二进制。etcd 使用**宿主机 curl** 下载；两个镜像均基于 `FROM scratch`
构建，示例应用复用本地镜像，因此默认示例不需要从 Docker Hub 拉取镜像。

预期集群状态：

```text
API healthy
KIND  NAME      PHASE  NODE  REPLICAS
Node  worker-1  Ready        -
Node  worker-2  Ready        -
```

### 3. 部署并访问示例

```sh
./bin/kk apply -f examples/demo.yaml
./bin/kk get deployments
./bin/kk get pods
./bin/kk get services

curl --fail --retry 20 --retry-delay 1 --retry-connrefused --max-time 5 \
  http://127.0.0.1:18080/
```

[示例配置](examples/demo.yaml) 创建一个两副本 Web Deployment 和一个 Service。
Pod 异步启动，最初可能处于 `Pending`；没有可用后端时，网关可能暂时返回 `503`。

就绪后，响应形如：

```text
hello from <container-hostname>
```

### 4. 扩缩容与排障

```sh
./bin/kk scale deployment web --replicas 3
./bin/kk get pods
./bin/kk get deployments web -o json
```

将下列 `POD_NAME` 替换为 `get pods` 输出的实际名称：

```sh
./bin/kk describe pod POD_NAME
./bin/kk logs POD_NAME
./bin/kk delete pod POD_NAME
```

删除 Deployment 管理的 Pod 后，控制器会补建副本。`describe` 输出保存的资源，
包括状态和失败原因；`logs` 返回容器 stdout/stderr 的日志快照，不持续跟随。

也可以在当前终端中使用简短的 `kk` 命令：

```sh
export PATH="$PWD/bin:$PATH"
kk get pods
```

请保留项目 `bin/` 中的二进制位置；集群操作需要项目源码目录和本地凭据。

### 5. 停止集群或删除示例

**保留期望状态**并停止集群：

```sh
./bin/kk cluster down
```

该命令先停止协调组件，再清理本项目的业务容器并停止 Compose，
保留 etcd 数据卷和本地 token。后续执行 `./bin/kk cluster up` 会恢复保存的资源，
重新创建对应工作负载。

如果希望彻底移除示例，应在停止集群**之前**删除资源：

```sh
./bin/kk delete -f examples/demo.yaml
./bin/kk cluster down
```

etcd 持久化的是资源定义，不是应用容器文件系统中的数据；当前未实现应用持久卷。

## 支持范围

| 资源 | 支持的行为 |
| --- | --- |
| Node | worker 注册与心跳；不通过用户 manifest 创建。 |
| Pod | 一个容器，支持 `name`、`image`、`command`、`args` 和字符串 `env`。 |
| Deployment | 显式指定 `replicas: 0..20`、标签选择器与 Pod 模板；直接管理 Pod，没有 ReplicaSet。 |
| Service | 整个集群一个 HTTP 网关；一个 `port: 80`，数字 `targetPort: 1..65535`。 |

只支持 `default` 命名空间。未知字段和不支持的操作会明确报错，不会被忽略。
除 Deployment 副本数外，已有资源的 spec 和 labels 不可修改。
更新镜像或 Service 选择器需要删除后重建，不提供滚动更新。

`apply -f` 支持 YAML 多文档，发送请求前先校验所有文档，但实际应用**不是事务**：
中途 API 失败，前面的文档可能已经应用成功。更新、删除都会检测版本冲突，
不会强行覆盖并发变更。

没有 `kubectl` 兼容性、Watch API、DNS、ClusterIP、NodePort、CNI、Ingress、
健康探针、资源感知调度、节点失联迁移、ConfigMap/Secret、应用数据卷、RBAC、
HPA 或控制面 HA。停止一个逻辑 kubelet，不会停止它先前创建的业务容器。

## 开发与测试

```sh
make vet
make test
go test -race ./...
```

端到端测试是仿照 Kubernetes `test/e2e` 的 Ginkgo v2 套件（见
[test/e2e/README.md](test/e2e/README.md)），需要先启动集群并确保 `docker` 在 `PATH` 中：

```sh
make e2e                          # 全部用例，包括破坏性用例
make e2e-quick                    # 跳过带 Disruptive 标签的用例
make e2e E2E_FOCUS='sig-apps'     # 按名称运行子集
```

**只在可重建的本项目实验集群上运行。** 用例使用唯一名称创建资源并在结束后清理，
检查 API 状态、真实 Docker 容器、HTTP 流量、日志、自愈、版本冲突与校验；
Disruptive 用例会重启控制面并执行 `cluster down/up`。每个新功能都会附带 e2e 用例。

## 常见问题

| 现象 | 检查方式 |
| --- | --- |
| 找不到 `go`、`docker` 或 `make` | 安装依赖，将自己机器上的安装目录加入 `PATH`。 |
| Docker 命令挂起 | 检查实际引擎，不只检查 API 元信息（例如 `systemctl status docker`）。重启前确认对其他工作负载的影响。 |
| etcd 下载失败 | 检查宿主机访问 `storage.googleapis.com` 的网络或代理；也可在 `bin/etcd` 预置匹配架构的 Linux etcd v3.5.17 二进制。 |
| Pod 拉取镜像失败 | 使用 `describe pod` 查看原因。worker 能使用缓存镜像或拉取公共镜像；私有镜像需要事先拉入同一个 Engine。 |
| 网关返回 `503` | 等待 Pod Running，检查 Service 标签、目标端口和 worker 心跳；`Running` 不代表应用通过了健康探针。 |
| 未知字段或 spec 不可变错误 | 使用文档声明的子集；只有副本数可以原地更新。 |
| 端口或项目名冲突 | 释放所需端口或停止冲突项目；只修改 `MINI_API` 不会修改 Compose 端口映射。 |
| Docker API 版本不兼容 | worker 使用 v1.41；修改 Docker CLI 的 `DOCKER_API_VERSION` 不会改变 worker 协议。 |

`bin/etcd` 缓存不会自动校验版本或架构。更换架构时需替换缓存，它不是可跨平台
分发的预编译版本。

首次启动会生成权限为 `0600` 的 `.mini-k8s/token`。不要泄露此文件，也不要在
集群运行中删除。`MINI_API` 只覆盖 CLI 连接的 API 地址，正常快速开始使用默认值。

手动使用 Compose 排障时，需要设置 `kk` 通常会自动提供的环境变量。
不要开启 shell tracing，也不要打印 token：

```sh
export MINI_TOKEN="$(cat .mini-k8s/token)"
export MINI_DOCKER_SOCKET=/var/run/docker.sock
docker compose ps
docker compose logs --tail=100
unset MINI_TOKEN MINI_DOCKER_SOCKET
```

如果本地 Engine 的 socket 路径不同，应改成实际路径。

仅在确认是凭据助手挂起时，可尝试
`MINI_DOCKER_NO_CREDENTIALS=1 ./bin/kk cluster up`。它使用临时匿名 Docker 配置，
不会修改现有配置，但不携带私有仓库凭据或原有代理、builder 配置，也不能修复
无响应的 Docker daemon。

## 路线图

mini-k8s 正在向“精简但五脏俱全”的 Kubernetes 演进：K8s 风格 REST 与 Watch、
命名空间、ReplicaSet 与滚动更新、带探针的多容器 Pod、带 DNS 的 ClusterIP Service、
节点生命周期、ConfigMap/Secret、卷以及更多工作负载类型。架构与里程碑进度见
[v2 设计规格](docs/superpowers/specs/2026-10-14-mini-k8s-v2-design.md)。

## 设计文档

- [设计规格](docs/superpowers/specs/2026-10-14-mini-k8s-design.md)：
  目标、决策、资源契约、架构、边界与验收标准。
- [实施计划](docs/superpowers/plans/2026-10-14-mini-k8s-plan.md)：
  事后整理的任务拆分、接口、依赖与验证方法。
- [v2 设计规格](docs/superpowers/specs/2026-10-14-mini-k8s-v2-design.md)：
  “精简但五脏俱全”的架构与里程碑。
- [M0 工程基线计划](docs/superpowers/plans/2026-10-14-m0-baseline.md)。
