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
   故障注入（同时停止该节点 kubelet 与其业务容器，模拟整机掉电）。默认不做 DinD/自研 CNI；
   kind 式节点作为可选方向保留在 §14，暂不排期。
2. **API 形态**：K8s 风格 REST 路径和对象结构，`kk` 是唯一受支持客户端，不承诺 `kubectl`
   兼容。
3. **严格子集**：继续拒绝未知字段。每种资源只实现文档列出的字段，宁可报错，不静默忽略。
4. **仅可信本地实验**：单一 bearer token、无 TLS；kubelet 与 kube-proxy 挂载 Docker socket。
5. **验证环境**：Linux（WSL2 Ubuntu 24.04 + Docker Engine 29）。文档只给 Linux 命令。
   语义拿不准时，在真实 AKS 集群（`kubectl`，context `testaks`）上对照实测，或查阅
   Kubernetes 文档与源码；实测只在临时命名空间中进行，用完即删。
6. **控制面自托管（2026-10-15 确认）**：仿 kubeadm，M2 之后紧接着实现（M3，见 §12.2）。
   etcd、kube-apiserver、kube-controller-manager、kube-scheduler 作为 `control-plane` 节点上的
   static Pod 运行在 `kube-system`，网关等插件也迁入 `kube-system`；Compose 最终只启动各节点的
   kubelet（相当于每台机器上用 systemd 启动 kubelet）。
7. **运行时边界**：M2 起 kubelet 通过 CRI 形状的内部接口（RunPodSandbox、CreateContainer、
   ContainerStatus、ExecSync 等）访问运行时，当前唯一实现基于 Docker（相当于 dockershim）。
   这样 kind 式节点（§14）若将来启动，只需新增 containerd CRI 实现。

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

### 3.1 控制面自托管后的拓扑（M3 起）

```text
WSL2 / Linux 宿主机
 └─ Docker Engine
     ├─ control-plane   节点容器：kubelet（发布 127.0.0.1:18000 → apiserver）
     │    └─ static Pod（hostNetwork，共享节点容器的网络命名空间），mirror Pod 位于 kube-system：
     │         etcd-control-plane / kube-apiserver-control-plane /
     │         kube-controller-manager-control-plane / kube-scheduler-control-plane
     ├─ worker-1 … N    节点容器：kubelet
     └─ 业务容器、kube-system 插件（网关、后续的 kube-proxy、DNS）——均由各节点 kubelet 创建
```

- Compose 只定义节点：`control-plane` 与 `worker`（按 `--workers N` 扩展）。节点容器像一台机器，
  kubelet 是其中唯一的常驻进程，不能也不需要以 Pod 形式运行。
- `kk cluster up` 生成 static Pod 清单（含 token）写入 `.mini-k8s/manifests/`（0600），以只读
  bind mount 挂进 control-plane 节点，等价于 kubeadm 的 `/etc/kubernetes/manifests`。
- `control-plane` 节点带 `node-role.kubernetes.io/control-plane:NoSchedule` 污点，业务 Pod
  不会调度上去；kube-system 插件按需容忍。

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
- 运行时接口：kubelet 的 Pod 同步逻辑只依赖 CRI 形状的 `RuntimeService` 接口；Docker 实现
  负责把沙箱映射为 pause 容器、把容器映射为加入其网络命名空间的 Docker 容器。
- 节点本地资源（为 M3 准备）：`hostNetwork: true` 的 Pod 共享**节点容器**的网络命名空间
  （`container:<节点容器>`），即逻辑节点的“主机网络”；`hostPath` 卷映射为按“节点名 + 路径”
  命名的 Docker named volume，即逻辑节点的“本地磁盘”，节点重启与 `cluster down/up` 后保留。
- static Pod（M3）：kubelet 监视清单目录，不经过 API 直接运行其中的 Pod；API 可用后在 Pod
  所在命名空间创建 mirror Pod（名称 `<pod>-<node>`，注解 `kubernetes.io/config.mirror`）并
  持续上报其状态。删除 mirror Pod 不会停止 static Pod，kubelet 会重建 mirror Pod；修改或删除
  清单文件才会替换或停止 static Pod。

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
| M2 | Pod 与 kubelet：CRI 形状的运行时接口、沙箱、多容器、init、重启策略、探针、资源、logs/exec、hostNetwork、hostPath |
| M3 | 控制面自托管：control-plane 节点、static Pod 与 mirror Pod、etcd 与控制面组件进入 kube-system、control-plane 污点（最小污点/容忍）、网关迁入 kube-system |
| M4 | ReplicaSet、Deployment 滚动更新与回滚、`kk rollout` |
| M5 | 网络：ClusterIP、Endpoints、kube-proxy、DNS、NodePort、Headless、Ingress |
| M6 | 调度框架、节点租约与生命周期、污点驱逐、cordon/drain、worker 扩展、故障注入 |
| M7 | ConfigMap、Secret、卷、PVC/PV |
| M8 | DaemonSet、Job、CronJob、StatefulSet |
| M9+ | 指标/top/HPA、Leader Election、RBAC 简化版、持续重构与缺陷修复 |
| 可选 | kind 式节点：privileged 节点容器 + containerd + CRI + 自研 host-gw CNI（未排期，见 §14） |

### 12.1 M1 落地记录与过渡态

M1 已实现第 4、5 节的 API 机制，并把全部组件迁到 informer/workqueue。以下是有意保留、
由后续里程碑替换的过渡实现：

| 过渡实现 | 替换里程碑 |
| --- | --- |
| Deployment 直接拥有 Pod（`pod-template-hash` 标签），模板变化按 Recreate 替换 | M4 ReplicaSet + RollingUpdate |
| Pod 只允许单容器，kubelet 重启同一个容器 | M2 沙箱与多容器 |
| 控制面组件与网关由 Compose 直接启动，不在 API 中可见 | M3 控制面自托管 |
| Service 仍是全局唯一的 HTTP 网关（准入保证），无 ClusterIP | M5 |
| 节点通过 Node status 的 Ready 条件每 5 秒心跳，消费者按 20 秒新鲜度判断就绪；无 Lease、无驱逐 | M6 |
| 调度器只按 Pod 数选最少者，遵守 `spec.unschedulable` | M3 加入污点过滤，M6 调度框架 |

实现中确认的细节：

- etcd key 为 `/registry/{resource}/{ns}/{name}`；Event 共享按分钟申请的 lease（TTL 1h+1m）。
- 未调度或已结束的 Pod 删除时宽限期为 0；kubelet 完成终止后以 `gracePeriodSeconds=0` 加
  UID 前置条件完成删除。
- 主资源更新与 PATCH 在“存储形态不变”时不写 etcd，`kk apply` 据此输出 `unchanged`。
- `kk apply` 先对整批文档做服务端 dry-run（同批新建的命名空间中的对象除外），再逐个写入。
- 控制器以 expectations 防止 informer 滞后导致的重复创建/删除；缩容优先删除未调度、
  Pending、未就绪、较新的 Pod。

### 12.2 M3 控制面自托管设计要点

参照 kubeadm 与 AKS 实测（AKS 的 kube-system 中有 kube-proxy DaemonSet、coredns 等插件，
托管控制面不可见；kubeadm 以 static Pod 运行控制面以解决“apiserver 尚未启动、无法通过 API
调度 apiserver”的自举问题）：

1. **节点**：Compose 新增 `control-plane` 节点容器，删除 etcd/apiserver/scheduler/controller/proxy
   五个服务；`kk cluster up` 在 `.mini-k8s/manifests/` 生成四个 static Pod 清单并挂载进该节点。
2. **kubelet 自举**：清单目录的 Pod 不依赖 API 即可运行；节点注册、mirror Pod、普通 Pod 的
   informer 在 apiserver 就绪前持续重试，互不阻塞。
3. **静态 Pod 运行方式**：均为 `hostNetwork`（共享 control-plane 节点容器的网络，apiserver
   监听其上，经节点容器发布到 `127.0.0.1:18000`）；etcd 数据目录为 hostPath，即按节点命名的
   Docker volume，`cluster down/up` 后保留。镜像沿用本地构建的 `mini-k8s:local` 与 etcd 镜像。
4. **mirror Pod**：名称 `etcd-control-plane` 等，位于 `kube-system`，带 `kubernetes.io/config.mirror`
   注解与指向 Node 的 ownerReference；status 由 kubelet 上报。API 拒绝更新 mirror Pod 的 spec；
   删除后由 kubelet 重建。
5. **污点**：引入 `spec.taints` 与 Pod `spec.tolerations` 的最小子集（NoSchedule/NoExecute 的
   Equal/Exists 匹配），调度器过滤不容忍的节点；control-plane 节点注册时自带
   `node-role.kubernetes.io/control-plane:NoSchedule`。完整的污点驱逐留在 M6。
6. **插件**：HTTP 网关改为 `kube-system` 中容忍 control-plane 污点、固定在 control-plane 节点的
   Pod（hostNetwork，经节点容器发布 `127.0.0.1:18080`）；M5 的 kube-proxy、DNS 同样部署在
   `kube-system`，DaemonSet 实现前用 Deployment/static Pod 过渡。
7. **生命周期**：`cluster down` 先停 worker 与 control-plane 的 kubelet，再清理所有带本集群标签的
   容器（含 static Pod），最后 Compose down；控制面组件崩溃由 kubelet 按 CrashLoopBackOff 重启。
8. **验收**：`kk get pods -n kube-system` 可见四个控制面组件且 Running；`kk logs -n kube-system
   kube-apiserver-control-plane` 可用；杀死 apiserver 容器后由 kubelet 拉起；删除 mirror Pod 后被
   重建且控制面不中断；业务 Pod 不会调度到 control-plane；`cluster down/up` 保留全部状态。

## 13. 非目标

kubectl 兼容、CRD/聚合 API、多机器部署、真正的 CNI（见 §14 可选方向）与网络策略、CSI、
控制面 TLS 与多租户隔离、生产级性能与可用性承诺。

## 14. 可选方向：kind 式节点（未排期）

**现状与取舍**：逻辑节点共享外层 Docker Engine，Pod 是宿主 Docker 上的平级容器，所有节点共用
内核、运行时、镜像缓存与同一个 Docker 网桥。容器、重启、日志、网络通信都是真实的；
节点隔离、节点级故障与 CNI 是逻辑上的。kind 式节点用更多复杂度换取更真实的节点语义。

| | 逻辑节点（现状） | kind 式节点（可选） |
| --- | --- | --- |
| 节点 | 只运行 kubelet 的容器 | privileged 容器，运行自己的 containerd + kubelet |
| kubelet ↔ 运行时 | Docker Engine API（类 dockershim） | CRI gRPC → 节点内 containerd |
| Pod 所在位置 | 宿主 Docker 上的平级容器 | 嵌套在节点容器内 |
| Pod 网络 | 同一 Docker 网桥，Docker IPAM 分配 IP | 自研 CNI：每节点一个 Pod 网段（`spec.podCIDR` 由 node IPAM 控制器分配），节点内网桥，跨节点 host-gw 路由，出网 MASQUERADE |
| Service | 每个 Service 一个代理容器持有 ClusterIP | 每节点 kube-proxy（iptables，DaemonSet，hostNetwork + NET_ADMIN） |
| 节点故障 | 需 `kk node stop` 模拟 | 停掉节点容器即真实宕机 |
| 镜像 | 共享宿主镜像缓存 | 需 `kk image load` 导入每个节点（类似 `kind load docker-image`） |

**若启动的工作量**：节点镜像（基于发行版镜像，含 containerd、runc、iptables、iproute2，需要
从镜像仓库拉取基础镜像）；kubelet 的 containerd CRI 实现（引入 gRPC 与 CRI API 依赖）；
`mini-cni` 插件与 node IPAM 控制器；iptables 版 kube-proxy；DNS 改为 ClusterIP 后面的
Deployment；`cluster up` 的镜像预加载。预计 4 小时以上，环境风险集中在嵌套 cgroup v2、
privileged 容器内的 containerd 与跨节点路由。

**排序考量**：若决定实施，最好在网络里程碑（M5）之前完成，否则 kube-proxy 与 DNS 要按两种
网络模型各做一遍；M2 引入的 CRI 形状运行时接口保证 kubelet 上层逻辑无需返工。实施后逻辑节点
模式退役，避免长期维护两套网络实现。
