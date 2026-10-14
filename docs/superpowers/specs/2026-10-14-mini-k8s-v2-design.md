# mini-k8s v2 设计规格：精简但五脏俱全

- 日期：2026-10-14
- 状态：已确认方向，按里程碑持续实施；每个里程碑合入 main 时同步更新本文
- 前一版：[v1 设计规格](2026-10-14-mini-k8s-design.md)（30 分钟限时版，已在 v2 中被逐步替换）

## 1. 目标

v1 跑通了“Deployment → Pod → 调度 → 真实容器 → HTTP 访问”的最小闭环，但每个子系统都停在
“刚好能演示”的程度：没有 Watch、没有命名空间、只有一个 Service、节点失联无人处理、
Pod 只能单容器、没有滚动更新。

v2 的目标是在**保持自研、可读、可在单机运行**的前提下，补齐 Kubernetes 的核心器官：

| 子系统 | 必须具备的能力 |
| --- | --- |
| API 机制 | K8s 风格 REST、命名空间、通用元数据、Watch、乐观并发、优雅删除、finalizer、ownerReferences 与级联删除、Events |
| 控制器 | Informer + WorkQueue 事件驱动；ReplicaSet、Deployment 滚动更新/回滚、DaemonSet、Job、CronJob、StatefulSet、Endpoints、GC、Namespace、节点生命周期 |
| 调度 | Filter/Score 插件、资源感知、nodeSelector/亲和性、污点与容忍、binding 子资源、失败原因与事件 |
| 节点 | 多容器 Pod（pause 沙箱）、init 容器、重启策略、探针、资源限制、优雅终止、日志/exec、节点租约 |
| 网络 | 多 Service、ClusterIP、Endpoints、kube-proxy、集群 DNS、NodePort、Headless、Ingress |
| 配置与存储 | ConfigMap、Secret、emptyDir、PVC/PV 动态供给 |
| CLI | `kk` 覆盖 get/describe/apply/delete/logs/exec/scale/rollout/top/cordon/drain/node 故障注入等 |

## 2. 已确认的关键决策

1. **节点模型**：保留“多个 kubelet 共享宿主 Docker Engine、以标签划分逻辑节点”。
   补齐节点租约、NotReady 判定、污点驱逐、可配置 worker 数，以及 `kk node stop|start`
   故障注入（同时停止该节点 kubelet 与其业务容器，模拟整机掉电）。不做 DinD/自研 CNI。
2. **API 形态**：K8s 风格 REST 路径和对象结构，`kk` 是唯一受支持客户端，不承诺 `kubectl`
   兼容。
3. **严格子集**：继续拒绝未知字段。每种资源只实现文档列出的字段，宁可报错，不静默忽略。
4. **仅可信本地实验**：单一 bearer token、无 TLS；kubelet 与 kube-proxy 挂载 Docker socket。
5. **验证环境**：Linux（WSL2 Ubuntu 24.04 + Docker Engine 29）。文档只给 Linux 命令。

## 3. 拓扑

```text
                         kk (宿主机)
                            │ HTTP + Bearer token
                            ▼ 127.0.0.1:18000
 etcd ◀──────────────── kube-apiserver ◀───────────────────────────────┐
                            ▲  list/watch/write                        │
     ┌──────────────┬───────┴──────┬───────────────┬──────────────┐    │
 scheduler  controller-manager  kubelet × N   kube-proxy      cluster-dns │
                                  │ docker.sock   │ docker.sock   10.244.128.10
                                  ▼               ▼                      │
                       Pod 沙箱 + 业务容器   每个 Service 一个代理容器   │
                                                  (持有 ClusterIP)        │
                       ingress-controller ── 127.0.0.1:18080 ───────────┘
```

- 所有组件是同一个二进制 `mini` 的不同角色，镜像 `mini-k8s:local`（`FROM scratch`）。
- 专用 Docker 网络 `mini-k8s`：子网 `10.244.0.0/16`；Docker 动态分配段 `10.244.0.0/17`
  （控制面与 Pod）；Service ClusterIP 段 `10.244.128.0/17`，由 API Server 分配；
  集群 DNS 固定 `10.244.128.10`。
- worker 数量由 `kk cluster up --workers N` 决定（默认 2），Compose 服务 `worker` 按 N 扩展，
  kubelet 从自身容器名推导节点名 `worker-1..N`。
- 对宿主机只发布：API `127.0.0.1:18000`、Ingress `127.0.0.1:18080`、
  NodePort `127.0.0.1:30000-32767`。

## 4. API 机制

### 4.1 路径

| 组 | 前缀 | 资源 |
| --- | --- | --- |
| core | `/api/v1` | namespaces, nodes, pods, services, endpoints, events, configmaps, secrets, persistentvolumes, persistentvolumeclaims |
| apps | `/apis/apps/v1` | deployments, replicasets, daemonsets, statefulsets |
| batch | `/apis/batch/v1` | jobs, cronjobs |
| networking.k8s.io | `/apis/networking.k8s.io/v1` | ingresses |
| coordination.k8s.io | `/apis/coordination.k8s.io/v1` | leases |
| autoscaling | `/apis/autoscaling/v2` | horizontalpodautoscalers（后期） |

- 命名空间资源：`{prefix}/namespaces/{ns}/{resource}[/{name}[/{subresource}]]`；
  `{prefix}/{resource}` 表示跨命名空间列表。
- 集群资源：`{prefix}/{resource}[/{name}[/{subresource}]]`。
- 子资源：`status`（PUT）、`pods/binding`（POST）、`pods/log`（GET）、`pods/exec`（POST）、
  `pods/eviction`（POST）、`deployments|replicasets|statefulsets/scale`（GET/PUT）。
- 动词：GET（get/list/watch）、POST、PUT、PATCH（`application/merge-patch+json`）、DELETE。
- 查询参数：`labelSelector`、`fieldSelector`（`metadata.name`、`metadata.namespace`、
  `spec.nodeName`、`status.phase`、`involvedObject.*`）、`watch`、`resourceVersion`、
  `timeoutSeconds`；DELETE 支持 `DeleteOptions`（`gracePeriodSeconds`、
  `propagationPolicy`、`preconditions`）。

### 4.2 对象

- `TypeMeta{apiVersion, kind}` + `ObjectMeta{name, generateName, namespace, uid,
  resourceVersion, generation, creationTimestamp, deletionTimestamp,
  deletionGracePeriodSeconds, labels, annotations, ownerReferences, finalizers}`。
- 列表为 `{Kind}List`，`metadata.resourceVersion` 为本次读取的 etcd revision。
- 错误统一返回 `Status{status: Failure, reason, message, code}`；reason 取
  `NotFound/AlreadyExists/Conflict/Invalid/BadRequest/Unauthorized/MethodNotAllowed/Expired/InternalError`。
- `generation` 只在 spec 变化时递增；控制器在 status 中回写 `observedGeneration`。
- `metadata.generateName` 由服务端补随机后缀。

### 4.3 存储与并发

- etcd v3 JSON gateway（`/v3/kv/*`、`/v3/watch`），不引入 gRPC 依赖。
- Key：`/registry/{resource}/{namespace}/{name}` 或 `/registry/{resource}/{name}`。
- 创建比较 `create_revision == 0`；更新/删除比较 `mod_revision`；对象的
  `resourceVersion` 即 `mod_revision`。
- etcd 开启 `--auto-compaction-mode=periodic --auto-compaction-retention=10m`；
  Watch 遇到被压缩的版本返回 `410 Expired`，客户端重新 list。

### 4.4 Watch

`GET ...?watch=true&resourceVersion=N` 返回分块传输的换行分隔 JSON：
`{"type":"ADDED|MODIFIED|DELETED|ERROR","object":{...}}`。API Server 为每个 watch 打开一个 etcd
watch（`prev_kv=true`），在服务端应用 label/field selector；对象从“匹配”变为“不匹配”时
发送 `DELETED`，与 Kubernetes 语义一致。

### 4.5 删除语义

- 有 finalizers 或需要优雅终止的对象：DELETE 只设置 `deletionTimestamp`，对象保留。
- 已绑定节点的 Pod：设置 `deletionTimestamp` 与宽限期（默认 30s），kubelet 先 SIGTERM，
  超时 SIGKILL，清理完成后以 `gracePeriodSeconds=0` 真正删除。未绑定 Pod 立即删除。
- `propagationPolicy`：`Background`（默认，GC 异步删除依赖对象）、`Foreground`
  （finalizer `foregroundDeletion`，依赖删完再删自身）、`Orphan`（finalizer `orphan`，
  GC 移除依赖对象的 ownerReference）。
- Namespace 删除进入 `Terminating`，namespace 控制器清空其中全部资源后移除 finalizer。

### 4.6 准入与默认值

API Server 内置的准入链：命名空间存在且未终止 → 默认值 → 严格校验 → 资源特定逻辑
（Service 分配 ClusterIP/NodePort，Pod 追加默认容忍 `not-ready/unreachable:NoExecute`
30 秒，Secret `stringData` 合并到 `data`）。启动时确保 `default`、`kube-system`、
`kube-node-lease` 三个命名空间存在。

## 5. 客户端机制

- `client`：通用 REST 客户端与按资源类型参数化的泛型客户端，Status 错误转换为可判断的错误。
- `informer`：list → watch → 本地缓存 → 事件回调，带 resync；410 时重新 list；
  `SharedInformerFactory` 让同一进程共享缓存。
- `workqueue`：去重、延迟入队、按 key 指数退避；控制器 `Run(workers)`。
- `record`：Event 记录器，按（对象，reason，message）聚合计数。

## 6. 控制器（controller-manager）

| 控制器 | 行为 |
| --- | --- |
| ReplicaSet | 以 ownerReference + selector 维护 Pod 数量；不计入正在删除的 Pod；缩容优先删未调度、未就绪、较新的 Pod；回写 replicas/ready/available |
| Deployment | `pod-template-hash` 区分版本；RollingUpdate（maxSurge/maxUnavailable）与 Recreate；修订号注解、revisionHistoryLimit；`kk rollout undo` 将旧 RS 模板写回 |
| DaemonSet | 每个可调度节点一个 Pod（nodeSelector、容忍），模板变化时滚动替换 |
| Job | completions/parallelism/backoffLimit，重启策略 OnFailure/Never，Complete/Failed 条件 |
| CronJob | 5 段 cron 表达式，concurrencyPolicy、suspend、历史保留 |
| StatefulSet | 有序创建/删除、稳定名称 `name-N`、volumeClaimTemplates、配合 Headless Service 的稳定 DNS |
| Endpoints | 为有 selector 的 Service 维护 ready/notReady 地址，解析命名端口 |
| NodeLifecycle | 租约超过 15s 未续约 → Ready=Unknown，加 `unreachable` 污点；不容忍的 Pod 在容忍期后被驱逐 |
| GarbageCollector | 处理 ownerReferences 与三种删除传播策略 |
| Namespace | 清空终止中的命名空间 |
| PersistentVolume | PVC 动态供给为 Docker named volume 型 PV、绑定、回收 |
| HPA（后期） | 根据 kubelet 指标调整 scale 子资源 |

## 7. 调度器

- 队列：未绑定且 `schedulerName` 为 `default-scheduler` 的 Pod；失败后指数退避，
  节点或 Pod 变化时重新入队。
- Filter：NodeUnschedulable、NodeName、TaintToleration、NodeAffinity/nodeSelector、
  NodeResourcesFit（requests 对比 allocatable）。
- Score：LeastAllocated、同属主 Pod 打散、亲和性偏好、PreferNoSchedule。
- 绑定通过 `pods/binding`；失败写 `PodScheduled=False` 条件和 `FailedScheduling` 事件，
  信息形如 `0/3 nodes are available: 1 Insufficient memory, 2 node(s) had untolerated taint`。

## 8. kubelet

- 只 watch `spec.nodeName=自己` 的 Pod；每个 Pod 一个 worker goroutine，慢操作（拉镜像）
  不阻塞其他 Pod。
- 沙箱：每个 Pod 先创建 pause 容器（`/mini pause`）加入集群网络，设置 hostname、集群 DNS
  与搜索域；业务容器使用 `container:<pause>` 网络模式共享网络命名空间，Pod IP 即沙箱 IP。
- 容器命名沿用 dockershim 风格 `k8s_{container}_{pod}_{ns}_{uid}_{restartCount}`，
  每次重启创建新容器并保留上一个，以支持 `logs --previous`。
- init 容器按序运行至成功；`restartPolicy` Always/OnFailure/Never；退避 1s 起翻倍、上限 30s。
- 状态：Pod phase、conditions（PodScheduled/Initialized/ContainersReady/Ready）、
  每个容器的 state/lastState/ready/restartCount；**只在变化时写 status**。
- 探针：liveness/readiness/startup，支持 httpGet、tcpSocket、exec。
- 资源：limits → Docker `Memory`/`NanoCpus`，requests.cpu → `CpuShares`；记录 OOMKilled。
- 卷：emptyDir（Pod 级 Docker volume）、configMap/secret（写入 Pod 级 volume，周期刷新）、
  persistentVolumeClaim（绑定 PV 对应的 Docker named volume）。
- 节点：注册 capacity/allocatable（`MINI_NODE_CPU`、`MINI_NODE_MEMORY`）、地址、标签；
  每 2s 续约 `kube-node-lease` 中的 Lease，节点 status 变化或每 10s 更新一次。
- 服务端口 10250：日志（含 follow/previous/tail/container）、exec、指标摘要。
- 清理：只删除带本集群、本节点标签且不再属于任何 Pod 的容器与 volume。

## 9. 网络

- **ClusterIP**：API Server 从 `10.244.128.0/17` 分配并持久化在 Service 上。
- **kube-proxy**：`mini proxy` 为每个有 ClusterIP 的 Service 维护一个代理容器，
  该容器以静态 IP=ClusterIP 加入集群网络，按 Endpoints 轮询转发 TCP（userspace 模式），
  支持多端口和 `sessionAffinity: ClientIP`。NodePort 发布到 `127.0.0.1:{nodePort}`。
  这是因逻辑节点共享网络而做的简化：语义上等价于“任意节点上的 kube-proxy”。
- **Headless**（`clusterIP: None`）：不创建代理，DNS 直接返回就绪端点 IP。
- **集群 DNS**：`mini dns` 在 `10.244.128.10:53/udp` 提供
  `svc.ns.svc.cluster.local`、`hostname.svc.ns.svc.cluster.local`、ExternalName CNAME，
  其余查询转发给 Docker 内置 DNS。Pod 默认 `ndots:5`，搜索域
  `ns.svc.cluster.local svc.cluster.local cluster.local`。
- **Ingress**：`mini ingress` 在 `127.0.0.1:18080` 按 host + path（Prefix/Exact，
  最长匹配）把 HTTP 请求直接转发到 Service 的就绪端点。

## 10. kk

`kk` 只调用 API（集群生命周期命令除外）。输出列尽量对齐 kubectl（READY、STATUS、
RESTARTS、AGE、IP、NODE 等）。全局参数 `-n/--namespace`、`-A`、`-l`、
`-o wide|json|yaml|name`、`-w`。命令清单随里程碑在 README 中维护。

## 11. 测试策略

- 单元测试：类型/校验/选择器/数量解析、调度插件、控制器纯函数、Docker 运行时（假 Docker HTTP）。
- 组件集成测试：测试辅助程序启动真实 `bin/etcd`（不存在则跳过），在进程内运行 API Server，
  对存储、Watch、删除语义、控制器做黑盒验证。
- 端到端测试：仿照 Kubernetes `test/e2e` 的 Ginkgo v2 + Gomega 套件（`test/e2e`），
  `framework` 包提供 TestContext、按 spec 清理的 Framework、等待与诊断工具；用例按
  `[sig-*]` 分包，破坏性用例带 `Disruptive` 标签并串行执行。**每个功能必须附带 e2e 用例**，
  合入前在 WSL 中 `make e2e` 全绿。
- 所有测试在 Linux（WSL2）执行：`go vet ./...`、`go test -race ./...`、`make e2e`。

## 12. 里程碑

每个里程碑一个 PR，通过全部测试后合入 main，同时更新中英文 README 与本文：

| # | 内容 |
| --- | --- |
| M0 ✅ | 工程基线：LF 换行、etcd 压缩、kubelet 仅在变化时写状态、修正 v1 缺陷、Linux 文档 |
| E2E ✅ | Ginkgo v2 e2e 框架，迁移全部 live 用例（22 个 spec） |
| M1 ✅ | API 机制重构 + 所有组件迁移到 informer/workqueue；Namespace、Event、GC、优雅删除 |
| M2 | Pod 与 kubelet：沙箱、多容器、init、重启策略、探针、资源、logs/exec |
| M3 | ReplicaSet、Deployment 滚动更新与回滚、`kk rollout` |
| M4 | 网络：ClusterIP、Endpoints、kube-proxy、DNS、NodePort、Headless、Ingress |
| M5 | 调度框架、节点租约与生命周期、污点驱逐、cordon/drain、worker 扩展、故障注入 |
| M6 | ConfigMap、Secret、卷、PVC/PV |
| M7 | DaemonSet、Job、CronJob、StatefulSet |
| M8+ | 指标/top/HPA、Leader Election、RBAC 简化版、持续重构与缺陷修复 |

### 12.1 M1 落地记录与过渡态

M1 已实现第 4、5 节的 API 机制，并把全部组件迁到 informer/workqueue。以下是有意保留、
由后续里程碑替换的过渡实现：

| 过渡实现 | 替换里程碑 |
| --- | --- |
| Deployment 直接拥有 Pod（`pod-template-hash` 标签），模板变化按 Recreate 替换 | M3 ReplicaSet + RollingUpdate |
| Pod 只允许单容器，kubelet 重启同一个容器 | M2 沙箱与多容器 |
| Service 仍是全局唯一的 HTTP 网关（准入保证），无 ClusterIP | M4 |
| 节点通过 Node status 的 Ready 条件每 5 秒心跳，消费者按 20 秒新鲜度判断就绪；无 Lease、无驱逐 | M5 |
| 调度器只按 Pod 数选最少者，遵守 `spec.unschedulable` | M5 调度框架 |

实现中确认的细节：

- etcd key 为 `/registry/{resource}/{ns}/{name}`；Event 共享按分钟申请的 lease（TTL 1h+1m）。
- 未调度或已结束的 Pod 删除时宽限期为 0；kubelet 完成终止后以 `gracePeriodSeconds=0` 加
  UID 前置条件完成删除。
- 主资源更新与 PATCH 在“存储形态不变”时不写 etcd，`kk apply` 据此输出 `unchanged`。
- `kk apply` 先对整批文档做服务端 dry-run（同批新建的命名空间中的对象除外），再逐个写入。
- 控制器以 expectations 防止 informer 滞后导致的重复创建/删除；缩容优先删除未调度、
  Pending、未就绪、较新的 Pod。

## 13. 非目标

kubectl 兼容、CRD/聚合 API、多机器部署、真正的 CNI 与网络策略、CSI、
控制面 TLS 与多租户隔离、生产级性能与可用性承诺。
