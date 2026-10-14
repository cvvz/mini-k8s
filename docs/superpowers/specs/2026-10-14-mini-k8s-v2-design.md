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
| 节点 | kind 式节点（节点内 containerd，kubelet 经 CRI 管理 Pod）；多容器 Pod、init 容器、重启策略、探针、资源限制、优雅终止、日志/exec、节点租约；static Pod 自托管控制面 |
| 网络 | 自研 CNI（host-gw 跨节点路由）；多 Service、ClusterIP、Endpoints、iptables kube-proxy、集群 DNS、NodePort、Headless、Ingress |
| 配置与存储 | ConfigMap、Secret、emptyDir、PVC/PV 动态供给 |
| CLI | `kk` 覆盖 get/describe/apply/delete/logs/exec/scale/rollout/top/cordon/drain/node 故障注入等 |

## 2. 已确认的关键决策

1. **节点模型（2026-10-15 修订）**：M1 及以前是“多个 kubelet 共享宿主 Docker Engine、以标签
   划分的逻辑节点”。经可行性验证（[spike 报告](../spikes/2026-10-15-kind-node-spike.md)），
   **M2 改为 kind 式节点**：每个节点是一个 privileged 容器，内含自己的 containerd 与 kubelet，
   Pod 由节点内 containerd 经 CRI 创建，Pod 网络由自研 CNI（`mini-cni` + host-gw 路由）提供。
   M2 完成后逻辑节点模式退役，不长期维护两套运行时与网络实现。节点故障即停止节点容器，
   无需再模拟。
2. **API 形态**：K8s 风格 REST 路径和对象结构，`kk` 是唯一受支持客户端，不承诺 `kubectl`
   兼容。
3. **严格子集**：继续拒绝未知字段。每种资源只实现文档列出的字段，宁可报错，不静默忽略。
4. **仅可信本地实验**：单一 bearer token、无 TLS；节点容器以 privileged 运行（与 kind 相同），
   等同于拥有宿主 Docker 主机的高权限。
5. **验证环境**：Linux（WSL2 Ubuntu 24.04 + Docker Engine 29）。文档只给 Linux 命令。
   语义拿不准时，在真实 AKS 集群（`kubectl`，context `testaks`）上对照实测，或查阅
   Kubernetes 文档与源码；实测只在临时命名空间中进行，用完即删。
6. **控制面自托管（2026-10-15 确认）**：仿 kubeadm，在 kind 式节点（M2）与 Pod 能力（M3）之后
   实现（M4，见 §12.2）。etcd、kube-apiserver、kube-controller-manager、kube-scheduler 作为
   `control-plane` 节点上的 static Pod 运行在 `kube-system`，网关等插件也迁入 `kube-system`；
   Compose 最终只启动节点容器（相当于每台机器上用 systemd 启动 containerd 与 kubelet）。
7. **运行时边界**：M2 起 kubelet 只通过 CRI（gRPC，`k8s.io/cri-api`）访问节点内 containerd，
   与真实 kubelet 相同；为此接受 `google.golang.org/grpc` 与 `k8s.io/cri-api` 两个依赖。
   kubelet 内部以 CRI 形状的接口封装运行时，单元测试使用假 CRI 实现。

## 3. 拓扑

### 3.0 M2（kind 式节点）之后、M4 之前

```text
WSL2 / Linux 宿主机
 └─ Docker Engine ── 网络 mini-k8s（节点与控制面容器所在的“机房网络”）
     ├─ etcd / kube-apiserver / scheduler / controller-manager / gateway   Compose 容器（M4 前过渡）
     ├─ worker-1  privileged 节点容器
     │    ├─ mini node-init → containerd + kubelet
     │    └─ cni0 网桥 10.244.1.0/24 ── Pod（pause 沙箱 + 业务容器，由 containerd 经 CRI 创建）
     └─ worker-2  privileged 节点容器
          ├─ mini node-init → containerd + kubelet
          └─ cni0 网桥 10.244.2.0/24 ── Pod
   跨节点：每个节点上有“去往其他节点 Pod 网段 via 该节点地址”的 host-gw 路由
```

- 所有 Go 组件是同一个二进制 `mini` 的不同角色。镜像：`mini-k8s:local`（`FROM scratch`，
  控制面与 demo 应用）、`mini-k8s-node:local`（基于 Ubuntu 24.04，含 containerd、runc、iptables、
  iproute2、`mini`）、`mini-k8s-pause:local`（`/mini pause`）、etcd 镜像。
- 地址规划：节点与控制面容器使用 Docker 网络 `mini-k8s` 的地址；Pod 网段 `10.244.0.0/16`，
  每个节点一个 `/24`（node IPAM 控制器写入 `Node.spec.podCIDR`）；Service 网段 `10.96.0.0/12`
  （M6 起，由 kube-proxy 在每个节点内用 iptables 实现，不占用 Docker 地址）；集群 DNS `10.96.0.10`。
- worker 数量由 `kk cluster up --workers N` 决定（默认 2），节点名 `worker-1..N`。
- M4 之前网关仍是 Compose 容器，需要 `NET_ADMIN` 并运行同一套 host-gw 路由代理才能访问 Pod IP。
- 对宿主机只发布：API `127.0.0.1:18000`、网关/Ingress `127.0.0.1:18080`，M6 起 NodePort 经节点
  容器发布到 `127.0.0.1`。
- 本地镜像（`mini-k8s:local`、pause、etcd 等）在 `cluster up` 时导入每个节点的 containerd；
  `kk image load IMAGE` 用于导入用户自己构建的镜像（同 `kind load docker-image`）。

### 3.1 控制面自托管后的拓扑（M4 起）

```text
WSL2 / Linux 宿主机
 └─ Docker Engine
     ├─ control-plane   节点容器：containerd + kubelet（发布 127.0.0.1:18000 → apiserver）
     │    └─ static Pod（hostNetwork，即节点容器自身的网络），mirror Pod 位于 kube-system：
     │         etcd-control-plane / kube-apiserver-control-plane /
     │         kube-controller-manager-control-plane / kube-scheduler-control-plane
     ├─ worker-1 … N    节点容器：containerd + kubelet
     └─ 业务 Pod、kube-system 插件（网关、kube-proxy、DNS）——均在节点内由 kubelet 经 CRI 创建
```

- Compose 只定义节点：`control-plane` 与 `worker`（按 `--workers N` 扩展）。节点容器像一台机器，
  containerd 与 kubelet 是其中的常驻进程，kubelet 不能也不需要以 Pod 形式运行。
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

kubelet 运行在节点容器内，经 Unix socket 以 CRI gRPC 调用同一节点内的 containerd（M2 起）。

- 只 watch `spec.nodeName=自己` 的 Pod；每个 Pod 一个 worker goroutine，慢操作（拉镜像）
  不阻塞其他 Pod。
- 沙箱：`RunPodSandbox` 由 containerd 创建 pause 容器与网络命名空间并调用 CNI；kubelet 在
  `PodSandboxConfig` 中给出 hostname、DNS（集群 DNS 与搜索域，M6 起）、端口映射与
  `hostNetwork`。Pod IP 来自 `PodSandboxStatus`。pause 镜像为本地构建的 `mini-k8s-pause:local`。
- 容器：`CreateContainer`/`StartContainer`，CRI 元数据中的 `attempt` 即重启次数；每次重启创建
  新容器并保留上一个，以支持 `logs --previous`。日志由 containerd 写入
  `/var/log/pods/{ns}_{pod}_{uid}/{container}/{attempt}.log`（CRI 日志格式），kubelet 读取文件
  提供 logs/follow/tail。
- init 容器按序运行至成功；`restartPolicy` Always/OnFailure/Never；退避 1s 起翻倍、上限 30s（M3）。
- 状态：Pod phase、conditions（PodScheduled/Initialized/ContainersReady/Ready）、
  每个容器的 state/lastState/ready/restartCount；**只在变化时写 status**。
- 探针：liveness/readiness/startup，支持 httpGet、tcpSocket、exec（`ExecSync`）（M3）。
- 资源：requests/limits 映射为 CRI `LinuxContainerResources`（cpu shares/quota、memory limit），
  由 runc 写入 cgroup；记录 OOMKilled（M3）。
- 卷：emptyDir（节点内 Pod 目录）、hostPath（节点容器的文件系统，节点数据目录在 volume 上，
  节点重启与 `cluster down/up` 后保留）、configMap/secret（kubelet 写入 Pod 目录后挂载并周期刷新）、
  persistentVolumeClaim（M8）。
- `hostNetwork: true` 的 Pod 使用节点容器自身的网络命名空间，即真实的“主机网络”。
- 节点：注册 capacity/allocatable（`MINI_NODE_CPU`、`MINI_NODE_MEMORY`）、地址、标签；
  每 2s 续约 `kube-node-lease` 中的 Lease，节点 status 变化或每 10s 更新一次（M7）。
- 服务端口 10250：日志（含 follow/previous/tail/container）、exec、指标摘要。
- 清理：kubelet 只管理本节点 containerd 中带 CRI Pod 元数据的沙箱与容器；API 中已不存在的
  Pod 的沙箱在 housekeeping 中停止并删除。节点重启后残留的旧沙箱同样清理后按 API 重新创建。
- static Pod（M4）：kubelet 监视清单目录，不经过 API 直接运行其中的 Pod；API 可用后在 Pod
  所在命名空间创建 mirror Pod（名称 `<pod>-<node>`，注解 `kubernetes.io/config.mirror`）并
  持续上报其状态。删除 mirror Pod 不会停止 static Pod，kubelet 会重建 mirror Pod；修改或删除
  清单文件才会替换或停止 static Pod。

## 9. 网络

- **Pod 网络（M2）**：
  - node IPAM 控制器（controller-manager）为每个 Node 分配 `spec.podCIDR`（`10.244.N.0/24`）。
  - `mini-cni` 是自研 CNI 插件（遵循 CNI 1.0 规范，由 containerd 调用）：创建 veth 对，一端接入
    节点网桥 `cni0`（网关为网段第一个地址），一端移入 Pod 网络命名空间；在节点网段内分配地址
    （文件锁保护的 host-local 式分配）；DEL 时回收。kubelet 根据 `podCIDR` 生成 CNI 配置。
  - host-gw 路由代理（kubelet 内的一个循环）监视 Node 列表，在本节点维护
    “其他节点 Pod 网段 via 该节点 InternalIP”的路由；对 `10.244.0.0/16 → 集群外` 做 MASQUERADE，
    Pod 之间通信保留源 IP。节点容器同在一个 Docker 网桥上，下一跳二层直达。
- **ClusterIP（M6）**：API Server 从 `10.96.0.0/12` 分配并持久化在 Service 上；Endpoints 控制器
  维护就绪地址。
- **kube-proxy（M6）**：`kube-system` 中的 DaemonSet（`hostNetwork`，`NET_ADMIN`），在每个节点内
  用 iptables 实现 ClusterIP 与 NodePort：`KUBE-SERVICES` → `KUBE-SVC-*`（按概率均分）→
  `KUBE-SEP-*`（DNAT 到 Pod），与真实 kube-proxy 的 iptables 模式一致；支持多端口与
  `sessionAffinity: ClientIP`。DaemonSet 实现前以 static Pod 过渡。
- **Headless**（`clusterIP: None`）：不分配 ClusterIP，DNS 直接返回就绪端点 IP。
- **集群 DNS（M6）**：`mini dns` 以 Deployment 运行在 `kube-system`，Service 固定 ClusterIP
  `10.96.0.10`；提供 `svc.ns.svc.cluster.local`、`hostname.svc.ns.svc.cluster.local`、
  ExternalName CNAME，其余查询转发上游。Pod 默认 `ndots:5`，搜索域
  `ns.svc.cluster.local svc.cluster.local cluster.local`。
- **Ingress（M6）**：`mini ingress` 在 `127.0.0.1:18080` 按 host + path（Prefix/Exact，
  最长匹配）把 HTTP 请求转发到 Service 的就绪端点。

## 10. kk

`kk` 只调用 API（集群生命周期命令除外）。输出列尽量对齐 kubectl（READY、STATUS、
RESTARTS、AGE、IP、NODE 等）。全局参数 `-n/--namespace`、`-A`、`-l`、
`-o wide|json|yaml|name`、`-w`。命令清单随里程碑在 README 中维护。

## 11. 测试策略

- 单元测试：类型/校验/选择器/数量解析、调度插件、控制器纯函数、kubelet 运行时（进程内假 CRI gRPC 服务）、CNI 插件（网络命名空间内的实测，需要 root 时跳过）。
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
| M2 ✅ | kind 式节点：节点镜像与 `mini node-init`、节点内 containerd、kubelet 的 CRI 客户端、`mini-cni` + node IPAM + host-gw 路由、本地镜像导入与自建 pause；退役逻辑节点模式（设计见 §14） |
| M3a ✅ | Pod：多容器、init 容器、重启策略与终态、exec（非交互，CRI `ExecSync`）；logs `--previous` 已在 M2 完成（[计划](../plans/2026-10-15-m3a-multi-container.md)） |
| M3b ✅ | 探针（liveness/readiness/startup；httpGet/tcpSocket/exec）、资源 requests/limits（cgroup、OOMKilled）、QoS、节点容量（[计划](../plans/2026-10-15-m3b-probes-resources.md)） |
| M3c | Pod 与 kubelet：hostNetwork、emptyDir/hostPath |
| M4 | 控制面自托管：control-plane 节点、static Pod 与 mirror Pod、etcd 与控制面组件进入 kube-system、control-plane 污点（最小污点/容忍）、网关迁入 kube-system |
| M5 | ReplicaSet、Deployment 滚动更新与回滚、`kk rollout` |
| M6 | 网络：ClusterIP、Endpoints、iptables kube-proxy、DNS、NodePort、Headless、Ingress |
| M7 | 调度框架、节点租约与生命周期、污点驱逐、cordon/drain、worker 扩展 |
| M8 | ConfigMap、Secret、卷、PVC/PV |
| M9 | DaemonSet、Job、CronJob、StatefulSet |
| M10+ | 指标/top/HPA、Leader Election、RBAC 简化版、持续重构与缺陷修复 |

### 12.1 M1 落地记录与过渡态

M1 已实现第 4、5 节的 API 机制，并把全部组件迁到 informer/workqueue。以下是有意保留、
由后续里程碑替换的过渡实现：

| 过渡实现 | 替换里程碑 |
| --- | --- |
| 逻辑节点：kubelet 经 Docker API 直接管理宿主 Docker 上的容器，所有节点共享一个网桥 | M2 kind 式节点（已替换） |
| Deployment 直接拥有 Pod（`pod-template-hash` 标签），模板变化按 Recreate 替换 | M5 ReplicaSet + RollingUpdate |
| Pod 只允许单容器，kubelet 重启同一个容器 | M3 多容器与重启语义（M2/M3a 已替换） |
| 控制面组件与网关由 Compose 直接启动，不在 API 中可见 | M4 控制面自托管 |
| Service 仍是全局唯一的 HTTP 网关（准入保证），无 ClusterIP | M6 |
| 节点通过 Node status 的 Ready 条件每 5 秒心跳，消费者按 20 秒新鲜度判断就绪；无 Lease、无驱逐 | M7 |
| 调度器只按 Pod 数选最少者，遵守 `spec.unschedulable` | M4 加入污点过滤，M7 调度框架 |

实现中确认的细节：

- etcd key 为 `/registry/{resource}/{ns}/{name}`；Event 共享按分钟申请的 lease（TTL 1h+1m）。
- 未调度或已结束的 Pod 删除时宽限期为 0；kubelet 完成终止后以 `gracePeriodSeconds=0` 加
  UID 前置条件完成删除。
- 主资源更新与 PATCH 在“存储形态不变”时不写 etcd，`kk apply` 据此输出 `unchanged`。
- `kk apply` 先对整批文档做服务端 dry-run（同批新建的命名空间中的对象除外），再逐个写入。
- 控制器以 expectations 防止 informer 滞后导致的重复创建/删除；缩容优先删除未调度、
  Pending、未就绪、较新的 Pod。

### 12.2 M4 控制面自托管设计要点

参照 kubeadm 与 AKS 实测（AKS 的 kube-system 中有 kube-proxy DaemonSet、coredns 等插件，
托管控制面不可见；kubeadm 以 static Pod 运行控制面以解决“apiserver 尚未启动、无法通过 API
调度 apiserver”的自举问题）：

1. **节点**：Compose 新增 `control-plane` 节点容器，删除 etcd/apiserver/scheduler/controller/proxy
   五个服务；`kk cluster up` 在 `.mini-k8s/manifests/` 生成四个 static Pod 清单并挂载进该节点。
2. **kubelet 自举**：清单目录的 Pod 不依赖 API 即可运行；节点注册、mirror Pod、普通 Pod 的
   informer 在 apiserver 就绪前持续重试，互不阻塞。
3. **静态 Pod 运行方式**：均为 `hostNetwork`（即 control-plane 节点容器自身的网络，apiserver
   监听其上，经节点容器发布到 `127.0.0.1:18000`）；etcd 数据目录为 hostPath
   `/var/lib/etcd`，位于节点的数据 volume 上，`cluster down/up` 后保留。镜像沿用本地构建的
   `mini-k8s:local` 与 etcd 镜像（`cluster up` 时导入节点）。
4. **mirror Pod**：名称 `etcd-control-plane` 等，位于 `kube-system`，带 `kubernetes.io/config.mirror`
   注解与指向 Node 的 ownerReference；status 由 kubelet 上报。API 拒绝更新 mirror Pod 的 spec；
   删除后由 kubelet 重建。
5. **污点**：引入 `spec.taints` 与 Pod `spec.tolerations` 的最小子集（NoSchedule/NoExecute 的
   Equal/Exists 匹配），调度器过滤不容忍的节点；control-plane 节点注册时自带
   `node-role.kubernetes.io/control-plane:NoSchedule`。完整的污点驱逐留在 M7。
6. **插件**：HTTP 网关改为 `kube-system` 中容忍 control-plane 污点、固定在 control-plane 节点的
   Pod（hostNetwork，经节点容器发布 `127.0.0.1:18080`），不再需要单独的路由代理；M6 的
   kube-proxy、DNS 同样部署在 `kube-system`，DaemonSet 实现前用 static Pod/Deployment 过渡。
7. **生命周期**：`cluster down` 停止所有节点容器（节点内的 Pod 随之停止，包括 static Pod），
   再 Compose down；节点数据 volume 与 token 保留。控制面组件崩溃由 kubelet 按
   CrashLoopBackOff 重启。
8. **验收**：`kk get pods -n kube-system` 可见四个控制面组件且 Running；`kk logs -n kube-system
   kube-apiserver-control-plane` 可用；杀死 apiserver 容器后由 kubelet 拉起；删除 mirror Pod 后被
   重建且控制面不中断；业务 Pod 不会调度到 control-plane；`cluster down/up` 保留全部状态。

## 13. 非目标

kubectl 兼容、CRD/聚合 API、多机器部署、网络策略、CSI、
控制面 TLS 与多租户隔离、生产级性能与可用性承诺。

## 14. kind 式节点设计（M2）

2026-10-15 决定由逻辑节点改为 kind 式节点，作为后续里程碑的地基。风险最大的环节已在本地验证
通过，配方与注意事项见 [可行性验证报告](../spikes/2026-10-15-kind-node-spike.md)。

| | 逻辑节点（M1 及以前） | kind 式节点（M2 起） |
| --- | --- | --- |
| 节点 | 只运行 kubelet 的容器 | privileged 容器，运行自己的 containerd + kubelet |
| kubelet ↔ 运行时 | Docker Engine API（类 dockershim） | CRI gRPC → 节点内 containerd |
| Pod 所在位置 | 宿主 Docker 上的平级容器 | 嵌套在节点容器内 |
| Pod 网络 | 同一 Docker 网桥，Docker IPAM 分配 IP | `mini-cni`：每节点一个 Pod 网段，节点内网桥，跨节点 host-gw 路由，出网 MASQUERADE |
| Service | 单一 HTTP 网关 | 每节点 kube-proxy（iptables，M6） |
| 节点故障 | 停 kubelet 不影响 Pod | 停掉节点容器即真实宕机 |
| 镜像 | 共享宿主镜像缓存 | `cluster up` 导入本地镜像；`kk image load` 导入用户镜像 |

### 14.1 节点容器

- 镜像 `mini-k8s-node:local`：Ubuntu 24.04 + containerd + runc + iptables + iproute2 + `mini`
  （含 `mini-cni` 插件入口）。首次构建需要访问 Docker Hub 与 Ubuntu 软件源。
- 入口 `mini node-init`：在 cgroup v2 下把自身移入 `init.scope` 并向子 cgroup 委派全部
  controller、`mount --make-rshared /`、开启 `ip_forward`，然后以子进程运行 containerd，
  等待其 socket 就绪后运行 kubelet；任一子进程退出则整体退出，由 Compose 重启节点。
- 运行参数：`--privileged --cgroupns=private`、seccomp/apparmor unconfined、`/run` 与 `/tmp`
  为 tmpfs；`/var` 为每个节点独立的 named volume（containerd 的 overlayfs snapshotter 需要
  非 overlay 的底层文件系统，且节点数据需跨重启保留）。
- containerd 配置（v3）：CNI 插件目录与配置目录、`sandbox` 镜像指向 `mini-k8s-pause:local`。

### 14.2 kubelet 与 CRI

- 依赖 `google.golang.org/grpc` 与 `k8s.io/cri-api`，经 `/run/containerd/containerd.sock` 调用
  RuntimeService 与 ImageService。
- kubelet 内部定义 CRI 形状的接口，生产实现转发到 containerd，单元测试使用进程内假实现。
- 节点地址取节点容器在 `mini-k8s` 网络上的 IP；容器运行时版本报告为 `containerd://<version>`。
- M2 保持 M1 的 Pod 能力（单容器、重启退避、镜像拉取错误、优雅终止、日志），只替换实现；
  多容器等扩展在 M3。

### 14.3 Pod 网络

- node IPAM 控制器：为没有 `spec.podCIDR` 的 Node 从 `10.244.0.0/16` 分配未使用的 `/24`，
  `spec.podCIDR` 一经设置不可修改。
- `mini-cni`：CNI 1.0 插件（ADD/DEL/CHECK/VERSION），从 stdin 读取网络配置、从环境变量读取
  容器 ID 与网络命名空间路径；创建 `cni0` 网桥与 veth 对、分配地址、设置默认路由，向 stdout
  返回 CNI 结果。kubelet 在得到 `podCIDR` 后写入 `/etc/cni/net.d/10-mini.conflist`；
  写入之前节点上报 `NetworkReady=false`，调度器不向其调度 Pod。
- host-gw 路由代理：kubelet 内的循环，按 Node 列表维护到其他节点 Pod 网段的路由，并确保
  MASQUERADE 规则存在；M4 之前的网关容器运行同一代理以访问 Pod IP。

### 14.4 集群生命周期与测试

- `kk cluster up`：构建三类本地镜像，Compose 启动控制面容器与节点容器，向每个节点导入
  `mini-k8s:local` 与 pause 镜像，等待节点 Ready（含 `podCIDR` 已分配、CNI 配置已写入）。
- `kk cluster down`：停止节点容器后 Compose down；节点 volume、etcd 数据与 token 保留。
- `kk image load IMAGE...`：`docker save` 后在每个节点 `ctr -n k8s.io images import`。
- e2e：断言“Pod 有真实运行的容器”改为在节点内经 CRI 查询；新增跨节点 Pod 互通、Pod 源 IP
  保留、Pod 出网（TCP）、停止节点容器后其 Pod 不可达、`kk image load` 等用例。

### 14.5 M2 落地记录

M2 按上述设计落地（[实施计划](../plans/2026-10-15-m2-kind-nodes.md)），e2e 42 个 spec 全部通过。
实现中发现并处理的问题：

- containerd 的 CRI 在为沙箱配置网络时总会额外调用名为 `loopback` 的 CNI 插件。没有引入
  开源 CNI 插件包，而是让 `mini-cni` 同时实现 `loopback` 类型（`/opt/cni/bin/loopback`
  同样是指向 `mini` 的符号链接，按 argv[0] 分发）。
- 沙箱创建失败时，错误信息里带有每次不同的沙箱 ID，导致“状态变化 → watch 事件 → 再次同步”
  的热循环。kuberuntime 为沙箱创建增加与容器重启相同的指数退避，退避期间原样返回上次错误，
  状态不再变化。
- CRI `RemoveImage` 会删除同一镜像 ID 的所有标签；e2e 清理用户导入的镜像时改用
  `ctr images rm` 只删除单个引用，避免误删 `mini-k8s:local`。
- 节点 DNS：Pod 网络命名空间访问不到节点容器内的 Docker 内置 DNS（127.0.0.11），因此
  kubelet 在节点地址的 53 端口运行 UDP 转发器，并把它写入沙箱的 DNS 配置；集群 DNS 在 M6。
- 网关仍是单 Service 的 HTTP 网关，改用节点镜像（需要 iproute2）并持有 NET_ADMIN，运行同一
  host-gw 路由代理访问 Pod IP；M4 迁入 kube-system，M6 由 kube-proxy/Ingress 取代。
- node IPAM 未单独写集成测试，由单元测试（分配算法）与 e2e（各节点网段互不重叠、Pod IP 落在
  所在节点网段内）覆盖。
- 代码审查发现并修复：containerd shim 退出后被过继给容器 PID 1，而 Go 写的 `node-init` 不回收
  非自身启动的子进程，会累积僵尸进程——节点容器改用 Docker 的 init（tini）作 PID 1；
  containerd 删除容器/沙箱时不删日志文件——kubelet 在删除 Pod、housekeeping 与容器重启时清理
  `/var/log/pods` 下对应的目录与旧实例日志；`logs -f` 读到文件末尾后不再读取新内容——改为在同一
  `bufio.Reader` 上轮询。三者均有单元测试与 e2e 覆盖。

## 15. 代码结构（2026-10-15 起）

目录与包名对齐 Kubernetes，熟悉 k8s 源码的读者可以直接对照：

| 路径 | 内容 | 对应的 Kubernetes 位置 |
| --- | --- | --- |
| `cmd/mini` | 多合一二进制，参数为组件名：`kube-apiserver`、`kube-controller-manager`、`kube-scheduler`、`kubelet`、`gateway`、`demo` | `cmd/kube-*`（及早期的 `hyperkube`） |
| `cmd/kk` | 命令行客户端与集群生命周期 | `kubectl`、`kind` |
| `pkg/apis/{meta,core,apps}/v1` | 资源类型，导入别名 `metav1`、`corev1`、`appsv1` | `k8s.io/api`、`apimachinery/pkg/apis/meta/v1` |
| `pkg/apis/{core,apps}/validation` | 资源校验 | `pkg/apis/*/validation` |
| `pkg/labels`、`pkg/fields`、`pkg/watch` | 选择器与 watch 事件类型 | `apimachinery/pkg/{labels,fields,watch}` |
| `pkg/scheme` | 资源注册表（REST 坐标、Kind、构造函数） | `runtime.Scheme`、`RESTMapper` |
| `pkg/apiserver` | 通用 API 服务：路由、存储读写、删除语义、watch、子资源 | `k8s.io/apiserver` |
| `pkg/registry/{core,apps}/<resource>` | 各资源的 Strategy（默认值、校验、可变性、字段选择器） | `pkg/registry/<group>/<resource>` |
| `pkg/storage/etcd3` | etcd 存储；`testing` 子包启动临时 etcd | `apiserver/pkg/storage/etcd3` |
| `pkg/client`、`pkg/client/{informers,workqueue,record}` | REST 与泛型客户端、Informer、WorkQueue、Event 记录器 | `k8s.io/client-go` |
| `pkg/controller` 与 `pkg/controller/<name>` | 共享工具（expectations）与各控制器 | `pkg/controller` |
| `pkg/scheduler` | 调度器 | `pkg/scheduler` |
| `pkg/kubelet`、`pkg/kubelet/{kuberuntime,dns}` | 节点代理；CRI 客户端（沙箱、容器、日志）；节点本地 DNS 转发 | `pkg/kubelet`、`pkg/kubelet/kuberuntime`；node-local-dns |
| `pkg/network/{cni,hostgw}` | CNI 插件 `mini-cni`（含 loopback）与 host-gw 路由代理 | CNI 插件（bridge/host-local/loopback）与 flannel host-gw |
| `pkg/nodeinit` | 节点容器入口：cgroup 委派、containerd 配置与启动 | kind 的节点入口脚本 |
| `pkg/gateway` | 单 Service 的 HTTP 网关（M6 由 kube-proxy 与 Ingress 取代） | 无直接对应 |
| `pkg/util/{ptr,deepcopy,validation}` | 通用小工具 | `k8s.io/utils/ptr`、`apimachinery/pkg/util/validation` |
| `test/e2e` | Ginkgo 端到端测试 | `test/e2e` |
| `test/integration` | 真实 etcd + 进程内 API Server 的集成测试与框架 | `test/integration` |

所有包都在 `pkg/` 下、可被外部模块导入（与 Kubernetes 的 `pkg/` 一样不承诺 API 稳定）；
`pkg/apis` 与 `pkg/client` 足以让外部程序编写自己的控制器。

