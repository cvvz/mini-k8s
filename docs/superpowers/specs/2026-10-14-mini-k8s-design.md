# mini-k8s 精简版设计规格

- 日期：2026-10-14
- 状态：已确认范围的事后补写，与当前代码对应
- 实施计划：[mini-k8s Implementation Plan](../plans/2026-10-14-mini-k8s-plan.md)
- 用户入口：[中文说明](../../../README.zh-CN.md) / [English](../../../README.md)

> 本文补全此前在对话中确定、但未写入文件的设计。它不是声称在编码前已存在的
> 文档，也不把最初讨论过的完整功能版算作本轮已交付能力。若意图与具体实现
> 存在差异，以下章节会明确标注。

## 1. 意图与成功标准

目标是在本地构建一个可以阅读、修改、运行的微型容器编排系统，通过 `kk`
实际操作它，理解 Kubernetes 风格的声明式控制循环。

核心不是资源名称与组件名称齐全，而是可观察的完整链路：

1. 用户提交 Deployment 的期望状态。
2. 控制器生成实际 Pod 对象。
3. 调度器分配到逻辑节点。
4. kubelet 启动真实容器并报告运行状态。
5. HTTP Service 将请求转发到运行中的业务容器。
6. 扩缩容、容器退出和 Pod 删除后，系统持续协调到期望状态。
7. 重启管理组件或整个项目集群，仍能从持久化资源继续管理。

面向可信的本地学习和实验，不面向多租户或生产部署。不接受“只保存 YAML”
或“把 Pod 状态直接标成 Running”作为交付。

## 2. 决策演进

| 议题 | 初始讨论 | 本轮最终确定 |
| --- | --- | --- |
| 实现方式 | 封装真实 Kubernetes 或自行实现 | 自研控制面、节点 daemon、代理和 `kk`，复用 etcd 与容器运行时 |
| 语言 | Go、Rust、Python | Go |
| 节点 | Docker 节点容器内运行 containerd，经 CRI 管理 Pod | 两个 kubelet 共享外层 Docker Engine，按标签划分逻辑节点 |
| 功能范围 | 常用工作负载、存储、网络、RBAC、HPA 等 | 30 分钟编码目标下的核心运行闭环，不承诺常用功能全集 |
| 兼容性 | 完整 API 兼容或标准 YAML 子集 | 常见 YAML 结构的明确子集，以 `kk` 为受支持客户端 |
| 调度与网络 | CPU/内存调度、跨节点网络、标准 Service | 按已分配 Pod 数调度、单个本地 HTTP 网关 |
| 运行方式 | 本地 Docker；排障中考虑本机二进制控制面 | Docker 恢复后完成原 Docker 路径验收，未实现本机控制面的部署模式 |
| 文档 | 先在对话确认，不写文件 | 按后续要求补写 spec、plan，并提供双语 README |

原始独立 containerd 节点方案已被限时版的共享 Docker 方案取代；本轮不需要
`privileged` 节点容器，但 kubelet 挂载 Docker socket，仍然具有高权限。

30 分钟是当时的编码预算，不是无条件交付承诺。代码和二进制先完成，
Docker 故障处理、真实集群验收在用户允许继续后完成，不将后续时间计入“30 分钟
完成全部验收”的说法。

## 3. 目标、非目标与约束

### 3.1 必须实现

- Go 自研 API Server、Scheduler、Controller Manager、kubelet、HTTP Service 代理和 `kk`。
- etcd 持久化、资源身份、版本冲突检测、独立状态更新。
- Node、单容器 Pod、Deployment、单个 HTTP Service。
- 两个逻辑 worker、真实容器、扩缩容、容器重启和 Pod 补建。
- 日志、状态、错误原因，以及非零 CLI 失败退出码。
- 一套可运行的示例、可重复启动/停止的本地集群和行为验收测试。
- 只清理项目拥有的容器，不执行全局 prune。

### 3.2 本轮非目标

- Kubernetes 一致性认证、`kubectl` 或第三方控制器兼容。
- ReplicaSet、滚动更新、DaemonSet、StatefulSet、Job、CronJob。
- Watch/Informer、通用工作队列、多副本控制器、Leader Election。
- containerd/CRI、CNI、多机器网络、ClusterIP、NodePort、DNS、Ingress、NetworkPolicy。
- ConfigMap、Secret、业务数据卷、PV/PVC、CSI。
- 探针、CPU/内存 requests/limits、资源感知调度、HPA、配额。
- 节点失联迁移、cordon/drain、控制面高可用。
- RBAC、ServiceAccount、TLS、多租户、审计系统。
- `exec`、交互式终端、持续跟随日志、独立 Event 资源。

错误通过资源状态和组件日志呈现，不把这些等同于 Kubernetes Events。

### 3.3 环境与规模

- Go 模块最低版本为 1.23，外部 Go 依赖为 `gopkg.in/yaml.v3`。
- Compose v2、本地 Unix Docker endpoint、Linux 容器；拒绝 TCP/SSH/远程 endpoint。
- worker 的 Docker API 固定为 v1.41，daemon 必须接受该版本。
- 固定一个控制面实例、两个逻辑 worker、一个 `default` 命名空间、一个 HTTP Service。
- Deployment 副本数显式指定，取值 0–20。
- 固定 Compose 项目名 `mini-k8s`、网络名 `mini-k8s`，单 Engine 一套集群。
- API 只映射至 `127.0.0.1:18000`，业务网关只映射至 `127.0.0.1:18080`。
- 宿主机构建架构与容器架构保持一致；默认使用 Go 的 `GOHOSTARCH`。

参考验收环境为 macOS ARM64、Docker Engine 27.4、Compose v2.31、
Docker VM 约 4 核/5.8 GiB；这不是经过压力测试得到的最低资源要求。
其他架构与 rootless Docker 不应被写成已完成同等验证；Linux（Ubuntu 24.04 / WSL2、
Docker Engine 29.9）已在 v2 的 M0 阶段完成端到端验证。

## 4. 组件与运行拓扑

```text
宿主机 kk
    |
    | HTTP + bearer token，127.0.0.1:18000
    v
API Server ------------------> etcd ------> Docker named volume
    ^
    | 轮询读取资源，提交带版本的 spec/status 变更
    |
    +-- Scheduler
    +-- Controller Manager
    +-- kubelet worker-1 -----+
    +-- kubelet worker-2 -----+--> 同一个 Docker Engine --> 业务容器
    +-- HTTP Service Proxy -------------------------------> 业务容器
                  ^
                  |
          127.0.0.1:18080
```

部署定义见 [compose.yaml](../../../compose.yaml)。控制面、两个 kubelet、代理及
业务容器在同一个专用 Docker 网络中；etcd 和节点日志接口不发布到宿主机。

| 组件 | 职责与边界 |
| --- | --- |
| API Server | 认证、校验、CRUD、状态更新、日志转发；唯一直接访问 etcd 的业务组件 |
| etcd | 保存资源 JSON；以 revision 支持原子比较与更新；数据目录使用 named volume |
| Scheduler | 每秒扫描未调度 Pod，将其绑定到可用且分配数较少的节点 |
| Controller Manager | 每秒协调 Deployment 副本，创建/删除直属 Pod，并回收失去 owner 的 Pod |
| kubelet | 每 2 秒独立发送心跳；约每秒协调本节点容器，提供日志读取接口 |
| HTTP Service Proxy | 每秒刷新资源快照，按标签选后端，轮询代理 HTTP 请求 |
| kk | 用户资源操作走 API；集群启停使用本地构建和 Docker/Compose 命令 |

除 etcd 外，组件共用一个程序，通过
`mini apiserver|scheduler|controller|kubelet|proxy|demo` 选择角色。
`demo` 是可重复验收用的小型 HTTP 应用，不是模拟运行时。

## 5. 资源与接口契约

### 5.1 公共结构

沿用 `apiVersion`、`kind`、`metadata`、`spec`、`status` 的组织方式，但不是完整
Kubernetes 对象模型：

- 名称使用 1–63 字符 DNS label。
- 非 Node 资源的 namespace 省略时补为 `default`，其他 namespace 拒绝。
- metadata 包括 name、namespace、labels；服务端还维护 UID、整数 resourceVersion。
- Pod 所属 Deployment 通过项目自定义的 `metadata.ownerUID` 关联，不使用完整 ownerReferences。
- 用户 manifest 禁止提交 UID、resourceVersion、ownerUID、status 或 Node。
- status 包括 phase、reason、nodeName、containerID、podIP、heartbeat、restarts。
- 未知字段、非法类型和不支持的 API version 返回明确错误。

具体类型见 [model.go](../../../internal/mini/model.go)，YAML 输入规则见
[manifest.go](../../../cmd/kk/manifest.go)。资源内部字段与用户可填写字段不能混为一谈。

### 5.2 支持矩阵

| Kind / apiVersion | 创建内容 | 更新规则 |
| --- | --- | --- |
| Node / `v1` | kubelet 注册，spec 为 `{}`，节点不带 namespace | kubelet 更新心跳和状态 |
| Pod / `v1` | 恰好一个 container；name、image 必填，command、args、字符串 env 可选 | spec、labels 不可变；调度后不再改绑节点 |
| Deployment / `apps/v1` | replicas 必须为 0–20；selector.matchLabels 非空并匹配 template.metadata.labels；模板是单容器 Pod | 仅 replicas 可变，模板和 labels 不可变 |
| Service / `v1` | selector 非空；ports 恰好一项，port 为 80，targetPort 为 1–65535 数字 | spec、labels 不可变；全局至多一个 Service |

Pod 的退出后重启是固定行为，不提供 restartPolicy 字段。Service 的 `port: 80`
描述此子集的逻辑 HTTP 服务，不表示为它创建了 Kubernetes ClusterIP 或 NodePort。

### 5.3 HTTP API

所有接口都要求共享 bearer token。规范 kind 为 `Node`、`Pod`、`Deployment`、
`Service`，CLI 同时支持常见类型别名。

| 方法与路径 | 行为 |
| --- | --- |
| `GET /healthz` | 检查 API 能否访问 etcd |
| `GET /api/{Kind}` | 返回资源数组，不是 Kubernetes List 包装对象 |
| `GET /api/{Kind}/{name}` | 返回一个资源对象 |
| `POST /api/{Kind}` | 创建资源；服务端分配身份与初始状态 |
| `PUT /api/{Kind}/{name}` | 按 UID/resourceVersion 检查并更新允许修改的 spec |
| `PUT /api/{Kind}/{name}/status` | 更新内部状态，不覆盖已存 spec |
| `DELETE /api/{Kind}/{name}?version={resourceVersion}` | 带版本前置条件删除 |
| `GET /api/Pod/{name}/logs` | 通过所属 kubelet 读取真实容器日志 |

401 表示未认证；400 表示输入/操作不支持；404 表示资源不存在；
409 表示并发冲突或操作前提不满足；存储/组件错误明确返回失败。
不支持 patch、Watch、分页、API discovery 或 server-side apply。

### 5.4 持久化与并发

- etcd key 前缀为 `/mini/{Kind}/`，普通资源按名称存储。
- Service 使用固定 singleton key；两个不同名称也不能绕过单 Service 限制。
- 资源内容是 JSON，读回时以 etcd ModRevision 作为 resourceVersion。
- 创建比较 revision 为 0；更新/删除比较当前 revision，使用 etcd transaction 提交。
- 正常资源更新保留 UID、owner 和 status；状态更新保留原 spec。
- CLI 的多文档输入先整体解析校验，再顺序调用 API；跨资源操作不是原子事务。
- CLI 发现更新冲突返回错误，不强制覆盖；控制循环通过下一次读取重新协调。

## 6. 控制循环与故障语义

### 6.1 调度

只处理尚无 nodeName 的 Pod。候选节点 phase 为 Ready 且心跳不超过 15 秒。
按已分配 Pod 数选择最少者，平局按节点名称排序；本轮成功绑定立即计入负载，
避免同批 Pod 都落在同一节点。

没有候选节点时保持 Pending，并写入 Unschedulable 原因。不根据 CPU/内存、
亲和性或污点选择节点，也不迁移已绑定 Pod。

### 6.2 Deployment

根据 ownerUID 而非仅靠相同标签辨认自身 Pod：

- 副本不足：从模板创建具有新名称和 UID 的 Pod。
- 副本过多：删除多余 Pod，kubelet 随后清理对应容器。
- 用户删除一个受管理 Pod：新建替代 Pod，而非复用已删除 UID。
- Deployment 被删除：控制器发现 owner 已消失，回收其 Pod。
- 同名 Deployment 重建具有新 UID，不接管旧 owner 的 Pod。

不创建 ReplicaSet，不实现滚动更新；已有 Pod 即使 Failed，也由 kubelet 重试其
容器，而不是将每次运行错误都处理为一次 Deployment 扩容。

### 6.3 kubelet 与 Docker

容器名称基于 Pod UID，使用三类所有权标签：

```text
mini.cluster = mini-k8s
mini.node    = worker-1 或 worker-2
mini.pod     = Pod UID
```

优先复用本节点拥有的匹配容器；镜像已缓存则不拉取，缺失时可拉取公共镜像。
不向运行时注入私有仓库认证，私有镜像应由用户预先拉取。
业务容器加入专用网络，不挂载宿主机目录，不使用 privileged。

退出后重启由 kubelet 实施，不依靠 Docker 自动重启策略伪装控制循环。
失败使用退避，间隔上限 30 秒；普通 Docker 请求设超时，镜像拉取最长 5 分钟，
错误进入 Pod 状态和组件日志。

节点每次取得完整 Pod 列表后，清理本集群、本节点且存在 Pod UID 标签的孤儿容器，
不删除其他节点或其他用途的容器。日志读取先检查所有权，再拆解非 TTY
stdout/stderr 数据帧。

**限制：** Node 的 Ready 心跳不等于运行时或应用健康探测。节点超时后，调度器和
代理不再选它，但当前实现不会主动把存储中的 phase 改成 NotReady。
停止 kubelet 不会停止此前创建的容器，也不会触发跨节点迁移。

### 6.4 HTTP Service

代理读取唯一 Service、Pod 和 Node 的最新快照，选择同时满足以下条件的后端：

- 标签符合 Service selector。
- Pod phase 为 Running，具有 Pod IP。
- 所属节点具有可接受的 Ready 心跳。

按请求轮询后端，转发至 Pod IP 与 targetPort，保留请求路径和查询参数。
没有后端或资源刷新失败时返回 503；后端连接失败返回 502。
Running 不保证应用已经监听端口，也不等于通过 readinessProbe。

## 7. CLI、启动与停止

用户命令及示例见 [中文 Quick Start](../../../README.zh-CN.md)。

| 命令 | 输出或语义 |
| --- | --- |
| `cluster up` | 构建、启动、检查就绪；不自动部署示例 |
| `cluster status` | API 健康检查与 Node 表格，非全系统健康评估 |
| `cluster down` | 清理项目运行实例，保留 etcd 数据与 token |
| `apply/delete -f FILE` | 多文档 YAML；非事务批次 |
| `get KIND [NAME] [-o json]` | 列表 JSON 为数组，单个资源 JSON 为对象 |
| `describe pod NAME` | 资源 JSON，包括 status/reason |
| `logs NAME` | 不跟随的真实 stdout/stderr 快照 |
| `scale deployment NAME --replicas N` | 带版本更新 replicas |
| `delete pod/deployment/service NAME` | 先读取版本，再有条件删除；Node 由 worker 管理 |

### 7.1 启动流程

1. 从当前目录或项目 `bin/kk` 位置定位源码根目录。
2. 在 `.mini-k8s/token` 创建或读取随机 token；文件权限 0600。
3. 解析当前 Docker context/环境变量，要求本地 Unix endpoint。
4. 使用同一命令环境执行 5 秒 `docker info` 预检；此预检不是完整引擎健康保证。
5. `make build prepare-etcd`：构建本机 `kk` 与 Linux `mini`，获取或复用 etcd。
6. 构建 `mini-k8s:local` 与 `mini-k8s-etcd:3.5.17` 两个 scratch 镜像。
7. `docker compose up -d --pull never`。
8. 最多等待 90 秒，要求认证 API 正常且 worker-1、worker-2 均有新鲜 Ready 心跳。

etcd 来自官方 `storage.googleapis.com/etcd/v3.5.17/` Linux 归档，宿主机 curl
下载最长 90 秒，提取所需二进制后放入 `bin/etcd`。已存在缓存直接复用，
没有版本、架构或发布校验和的自动校验；预置缓存由操作者负责。

启动失败可能留下已启动组件，不提供跨构建/Compose 的事务回滚。
需查看错误及组件日志，修正原因后重试或显式停止，不能把部分启动当作成功。

### 7.2 停止与数据生命周期

按顺序执行：

1. 停止两个 worker、Controller Manager、Scheduler，防止清理过程中补建。
2. 只删除带项目 cluster 标签且带 pod 标签的业务容器。
3. 执行 Compose down，不附加 `-v`。

停止协调组件失败时，不继续删除业务容器。不得用全局 prune 代替精确清理。
etcd named volume、token、构建缓存保留；再次启动会根据原期望状态恢复。
如不希望工作负载再次出现，应在关集群前删除相应资源。

容器内部应用文件不在持久化承诺内。多个项目副本连接同一 Engine 也共享固定集群
身份，不能视作独立隔离集群。

### 7.3 环境变量与边界

| 变量 | 用途 |
| --- | --- |
| `MINI_API` | 覆盖 CLI 的连接地址；不会改变 Compose 发布端口 |
| `MINI_TOKEN` | 组件间共享认证；`kk` 从项目 token 文件向 Compose 注入 |
| `MINI_DOCKER_SOCKET` | Compose worker 的挂载源；macOS 使用 VM 内 `/var/run/docker.sock` |
| `MINI_DOCKER_NO_CREDENTIALS=1` | 可选临时匿名 Docker 配置；不改用户配置，不携带其凭据/代理/builder 设置 |
| `DOCKER_API_VERSION` | Docker CLI 行为配置；不会改变 worker 固定的 v1.41 协议 |
| `MINI_E2E=1` | 显式启用会修改真实集群的集成测试 |

默认启动不自动重启 Docker，不修改系统配置。涉及其他工作负载的环境操作应由
操作者明确决定。纯宿主机控制面和其他虚拟机/运行时替代方案未实现。

## 8. 安全与可靠性边界

- 本地 HTTP + 共享 token 是最低限度访问限制，不是身份/权限体系。
- Docker socket 相当于高权限控制通道；所有权标签是程序约束，不是安全沙箱。
- workload 与控制面共用 Docker 网络，不能视为网络隔离。
- 不向宿主机发布 etcd 和 kubelet 端口，不意味着集群内部不可访问它们。
- 业务 HTTP 网关没有应用身份认证。
- scratch 镜像不包含 shell、调试工具或 CA 证书。
- 明确暴露输入、API、存储、运行时错误；不能使用“成功形态”的降级响应。
- 有界重试、幂等重建和版本检测不等于生产级故障恢复、事务或数据安全保证。

## 9. 验收矩阵

| 验收项 | 必须观察的结果 |
| --- | --- |
| 启动 | 认证 API 可用，两个 worker 具有新鲜 Ready 心跳 |
| 部署 | 两个 Pod 绑定不同逻辑节点；真实 Docker 容器运行 |
| Service | 本机 HTTP 请求得到业务容器响应 |
| 日志 | `kk logs` 返回应用实际日志 |
| 扩缩容 | 2 → 3 → 1 时，API Pod 数与受管理 Docker 容器数最终一致 |
| 容器故障 | 终止受管理容器后恢复 Running，重启计数增加 |
| Pod 删除 | Deployment 创建不同 UID 的替代 Pod |
| 并发更新 | 旧 resourceVersion 被拒绝 |
| 控制面重启 | Deployment UID 保持，协调功能恢复 |
| down/up | down 后无项目业务容器；up 后原 Deployment UID 与期望副本恢复 |
| 所有权 | 单元测试验证不删除/读取其他所有者的容器 |
| 非法输入 | 未知字段、错误类型、越界副本及不可变更新明确失败 |

现有测试覆盖入口：

```sh
go test -race -count=1 ./...
go vet ./...
MINI_E2E=1 go test ./internal/mini -run '^TestLiveCluster$' -count=1 -v -timeout 5m
```

上述单元检查和真实集群验收已于 2026-10-14 在参考环境通过；这不是其他环境
自动兼容的证明。本次文档补写不会重新启动或修改运行中的集群。
实时测试会修改示例并执行 down/up，只能在可重建的本项目实验集群运行。

## 10. 未来演进与仍需决策的部分

以下是原始目标的后续方向，不是已交付或已排期的承诺：

1. 独立 containerd/CRI 节点及真实节点故障语义。
2. ReplicaSet、滚动更新、探针、资源感知调度。
3. DNS、标准 Service 网络、ConfigMap/Secret 与存储。
4. 更多工作负载、权限、指标、伸缩及运维能力。

每一阶段都需要重新确定兼容范围、隔离要求、资源预算和验收标准。尤其不能在保持
现有“逻辑节点”实现不变的情况下，宣称已经获得多机器隔离或故障迁移能力。
