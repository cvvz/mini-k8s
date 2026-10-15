# M7a 节点租约、节点生命周期、污点驱逐与 cordon/drain 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 节点失联能被发现并处理：kubelet 以 Lease 续约，节点生命周期控制器把失联节点标为 Unknown、打上
`unreachable`/`not-ready` 污点并把其上的 Pod 标为未就绪；污点驱逐控制器按 `tolerationSeconds` 驱逐 Pod；
`kk cordon/uncordon/drain/taint` 维护节点。

**Architecture:** 与 Kubernetes 相同的分工：kubelet 每 2 秒续约 `kube-node-lease` 中的 Lease，状态只在变化时或每 10 秒写一次；
controller-manager 中的 node-lifecycle 控制器按 Lease 判断存活（替代此前“Ready 心跳 20 秒新鲜度”的判断），
taint-eviction 控制器负责 NoExecute 污点；apiserver 提供 `pods/eviction` 子资源、准入时加默认容忍。

**Tech Stack:** Go 1.26、Ginkgo e2e。

## Global Constraints

- 只在 WSL（Ubuntu 24.04）中构建与测试；文档只出现 Linux 命令。
- 每个功能附带单元测试与 e2e；里程碑验收跑相关 e2e。
- 文案与 Kubernetes v1.35 源码一致（node lifecycle、taint eviction、kubectl drain）。

## 语义（与 Kubernetes 一致）

| 项 | 规则 / 文案 |
| --- | --- |
| Lease | `coordination.k8s.io/v1` Lease `kube-node-lease/<node>`：`holderIdentity` 为节点名，`leaseDurationSeconds`，kubelet 续约 `renewTime` |
| 失联 | 最近一次续约（或 Ready 心跳）超过宽限期：所有条件置 `Unknown`，reason `NodeStatusUnknown`，message `Kubelet stopped posting node status.`；Node 事件 `NodeNotReady` `Node <n> status is now: NodeNotReady` |
| 条件污点 | Ready=Unknown → `node.kubernetes.io/unreachable` 的 NoSchedule 与 NoExecute；Ready=False → `node.kubernetes.io/not-ready` 的两种；Ready=True 时移除；NoExecute 污点带 `timeAdded` |
| Pod 未就绪 | 节点变为非 Ready 时，其上 Pod 的 Ready 条件置 False（MarkPodsNotReady），Endpoints 随之摘除 |
| 默认容忍 | 准入为每个 Pod 加 `node.kubernetes.io/not-ready:NoExecute op=Exists for 300s` 与 `unreachable` 同样一条（已有同 key 的 NoExecute 容忍时不加） |
| 驱逐 | 不容忍的 NoExecute 污点立即驱逐；容忍且有 `tolerationSeconds` 的在 `timeAdded + seconds` 驱逐；事件 `TaintManagerEviction` `Marking for deletion Pod <ns>/<name>` |
| Eviction | `POST pods/<name>/eviction`（`policy/v1` Eviction，可带 deleteOptions）→ 优雅删除，返回 201 Status Success |
| cordon | `node/<n> cordoned`、`node/<n> already cordoned`；uncordon 同理 `uncordoned` |
| drain | 先 cordon；跳过 mirror Pod；没有 controller 的 Pod 需 `--force`，带 emptyDir 的 Pod 需 `--delete-emptydir-data`，否则报 `cannot delete Pods declare no controller (use --force to override): ns/name`、`cannot delete Pods with local storage (use --delete-emptydir-data to override): ns/name`；逐个 `evicting pod ns/name`，消失后 `pod/name evicted`，最后 `node/<n> drained`；`--grace-period`、`--timeout` |
| taint | `kk taint nodes N key[=value]:Effect` → `node/N tainted`；`key[:Effect]-` → `node/N untainted`；同 key 同 effect 已存在且无 `--overwrite`：`error: node N already has key taint(s) with same effect(s) and --overwrite is false` |

## 决策

| 决策 | 选择 |
| --- | --- |
| 时间参数 | 为了笔记本上的集群反应快：Lease 时长 15 秒、续约间隔 2 秒，节点状态每 10 秒或变化时写，生命周期控制器每 2 秒检查、宽限期 15 秒（Kubernetes 默认 40 秒/10 秒/5 分钟/5 秒/50 秒） |
| 就绪判断 | `IsNodeReady` 只看 Ready 条件的 status（与 Kubernetes 相同），新鲜度由生命周期控制器负责 |
| 驱逐控制器 | 独立的 taint-eviction 控制器（同 Kubernetes 1.29+ 的 `taint-eviction-controller`），工作队列按 Pod 截止时间 `AddAfter` |
| PDB | 不实现 PodDisruptionBudget，Eviction 直接优雅删除（README 说明） |
| DaemonSet | M9 才有；drain 的 `--ignore-daemonsets` 先接受但无作用 |

## Tasks

### Task 1: API
- [x] Lease 类型、注册、`kube-node-lease` 命名空间；Taint `timeAdded`；`pods/eviction` 子资源；默认容忍准入；`IsNodeReady` 改为只看 status；单元测试。

### Task 2: kubelet 与控制器
- [x] kubelet 续约 Lease、状态按变化/周期写；`pkg/controller/nodelifecycle`（失联判断、条件污点、MarkPodsNotReady）；`pkg/controller/tainteviction`；集成测试（假 kubelet 停止续约 → Unknown、污点、Pod 未就绪、按期驱逐、恢复后移除污点）。

### Task 3: kk
- [x] `cordon/uncordon/drain/taint`、`get leases`；单元测试。

### Task 4: e2e 与文档
- [x] e2e：Lease 续约；停止 worker 节点容器 → NotReady、污点、Endpoints 摘除、按 `tolerationSeconds` 驱逐并在另一节点重建、恢复后污点消失；`kk taint` NoExecute 驱逐与容忍；cordon/drain（迁移、`--force`、mirror Pod 不受影响）；默认容忍。
- [x] README（中英）、v2 规格、本计划勾选。

## 结果

全部任务完成：单元测试、集成测试（`-race`）与相关 e2e（节点生命周期 5 个用例含真实停止 worker 节点容器，sig-node 与
cluster-lifecycle 共 44 个；修复后再跑 13 个）通过。e2e 中发现调度器只按节点 Pod 总数选择节点，“每个 worker 一个副本”不可靠，
用例改为创建工作负载时临时 cordon worker-1；按属主打散留给 M7b。

代码审查后的 critical 修复：

- 多个 NoExecute 污点时用最早的 `timeAdded` 计时，被永久容忍的旧污点会让新污点的容忍期“提前用完”——改为每个污点按自己的
  `timeAdded + tolerationSeconds` 计算再取最早者，起点不早于控制器启动时间（`cluster down/up` 后残留污点不会立即驱逐）。
- 升级前创建的 Pod 没有默认容忍，节点短暂 NotReady 就被立即驱逐——驱逐计算前为容忍列表补默认值。
- 带 `tolerations` 的 Pod 清单再次 `kk apply` 时，merge patch 替换了数组、丢掉默认容忍而触发“spec 不可变”——默认容忍改在
  Pod 的 Defaults 中补（更新时同样），更新校验也先给旧 spec 补上。
- apiserver/etcd 中断超过宽限期后节点可能被误标 Unknown——生命周期控制器每轮先探测 API 健康，中断恢复后为所有节点重新计时。
