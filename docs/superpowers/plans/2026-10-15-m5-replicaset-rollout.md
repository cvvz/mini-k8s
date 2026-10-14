# M5 ReplicaSet、滚动更新与回滚实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 新增 ReplicaSet 资源与控制器；Deployment 改为管理 ReplicaSet，支持 RollingUpdate（maxSurge/maxUnavailable）与 Recreate、修订历史、回滚、暂停/恢复；提供 `kk rollout status|history|undo|pause|resume|restart`。

**Architecture:** 与 Kubernetes 相同的两层控制器：ReplicaSet 控制器让 Pod 数等于 `spec.replicas`；Deployment 控制器按模板哈希（`pod-template-hash`）找到或创建新 ReplicaSet，按策略在新旧 ReplicaSet 之间调整副本数，维护 `deployment.kubernetes.io/revision` 注解、状态与 Available/Progressing 条件。回滚由 kk 在客户端把旧 ReplicaSet 的模板写回 Deployment。

**Tech Stack:** Go 1.26、Ginkgo e2e。

## Global Constraints

- 只在 WSL（Ubuntu 24.04）中构建与测试；文档只出现 Linux 命令。
- 每个功能附带单元测试与 e2e；输出与事件文案以 AKS 实测为准（2026-10-15）。

## 语义（AKS 实测）

| 项 | 规则 / 文案 |
| --- | --- |
| 默认策略 | RollingUpdate，maxSurge 25%（向上取整）、maxUnavailable 25%（向下取整），两者都为 0 时 maxUnavailable 取 1；`revisionHistoryLimit` 10；`progressDeadlineSeconds` 600 |
| 新 ReplicaSet | 名称 `<deployment>-<hash>`，选择器与模板标签加 `pod-template-hash`；注解 `deployment.kubernetes.io/revision`（最大旧版本 + 1；回滚复用旧 RS 时也递增）；复制 Deployment 的 `kubernetes.io/change-cause` |
| 滚动算法 | 同 `rolloutRolling`：新 RS 扩容不超过 `replicas + maxSurge`；旧 RS 先缩不可用的 Pod，再在保持 `replicas - maxUnavailable` 个可用的前提下缩容 |
| Recreate | 先把旧 RS 缩到 0，旧 Pod 全部消失后再扩新 RS |
| 事件 | Deployment：`ScalingReplicaSet` `Scaled up replica set web-56c4574cd from 0 to 3` / `Scaled down replica set ... from 3 to 2`；ReplicaSet：`SuccessfulCreate` `Created pod: web-56c4574cd-s45cq`、`SuccessfulDelete` |
| 条件 | Available：`MinimumReplicasAvailable` "Deployment has minimum availability." / `MinimumReplicasUnavailable` "Deployment does not have minimum availability."；Progressing：`NewReplicaSetCreated` `Created new replica set "x"`、`ReplicaSetUpdated` `ReplicaSet "x" is progressing.`、`NewReplicaSetAvailable` `ReplicaSet "x" has successfully progressed.`、`ProgressDeadlineExceeded` `ReplicaSet "x" has timed out progressing.`；暂停时 `DeploymentPaused` "Deployment is paused"，恢复时 `DeploymentResumed` "Deployment is resumed" |
| rollout status | `Waiting for deployment "web" rollout to finish: 1 out of 3 new replicas have been updated...`、`... 1 old replicas are pending termination...`、`... 2 of 3 updated replicas are available...`、`deployment "web" successfully rolled out` |
| rollout history | `deployment.apps/web` 后接 `REVISION  CHANGE-CAUSE` 表，未设置为 `<none>` |
| rollout undo | `deployment.apps/web rolled back`；`--to-revision` 不存在：`error: unable to find specified revision 9 in history` |
| pause/resume | `deployment.apps/web paused` / `resumed` |

## 决策

| 决策 | 选择 |
| --- | --- |
| ReplicaSet 的 Pod 管理 | 沿用现有 Deployment 控制器的创建/删除、Expectations 与删除排序（迁到 `pkg/controller/replicaset`）；`minReadySeconds` 计算可用副本 |
| 孤儿 Pod 收养 | 不实现（与 Kubernetes 不同）；控制器只管理 controllerRef 指向自己的 Pod |
| 比例扩缩 | 不实现 `desired-replicas`/`max-replicas` 注解；滚动过程中修改副本数由滚动算法自然收敛 |
| 旧集群 | M5 之前创建的 Deployment 直接拥有 Pod：新 RS 可用后删除这些遗留 Pod，自动迁移（见“结果”） |

## Tasks

### Task 1: API
- [x] `ReplicaSet` 类型、注册、strategy、客户端；Deployment `strategy`、`minReadySeconds`、`revisionHistoryLimit`、`paused`、`progressDeadlineSeconds`、`conditions`；默认值与校验；单元测试。

### Task 2: 控制器
- [x] ReplicaSet 控制器与单元测试（假 API）。
- [x] Deployment 控制器重写：新/旧 RS、RollingUpdate 与 Recreate、修订号、历史清理、状态与条件、暂停；单元测试覆盖滚动算法与 fencepost 计算。

### Task 3: kk
- [x] `get rs`、`scale rs`、`rollout status|history|undo|pause|resume|restart`；单元测试。

### Task 4: e2e 与文档
- [x] 用例：滚动更新期间始终有可用副本（maxUnavailable 0）且网关不中断；Recreate；历史与回滚（含 change-cause、不存在的版本）；暂停期间修改模板不滚动、恢复后滚动；ReplicaSet 自愈与 `kk get rs`；GC 级联（Deployment → RS → Pod）；更新现有 Deployment 用例。
- [x] README（中英）、v2 规格、本计划勾选。

## 结果

全部任务完成：e2e 76 个 spec 通过（新增 7 个，含滚动更新期间网关不中断），单元测试、集成测试与 `-race` 通过。
实现中发现并修复：

- 控制器在缩放时改写了 informer 缓存中的 ReplicaSet（数据竞争）——同步开始时先深拷贝。
- control-plane 节点的磁盘保留旧镜像，节点重建后 static Pod 会先用旧镜像启动；`kk cluster up` 现在比较
  每个 kube-system 容器正在用的镜像与镜像名当前指向的镜像，不一致就重启（相当于一次 kubeadm 式升级）。
- `kk cluster up` 只接受启动之后的节点心跳，否则 etcd 中重建前的心跳会让它过早返回。

代码审查后的 critical 修复：

- M5 之前存入 etcd 的 Deployment 没有 `strategy` 等默认值——控制器同步时对副本补默认值，不再解引用空指针。
- M5 之前 Deployment 直接拥有的 Pod 不会被管理——新 RS 完全可用后删除这些遗留 Pod（Recreate 则先删）。
  因此旧集群升级后自动迁移，无需删除重建。
- `--cascade=orphan` 留下的 ReplicaSet 在重建同名 Deployment 时不会被收养——选择器匹配且无 controller 的
  ReplicaSet 现在被收养（`AdoptedReplicaSet`，与 Kubernetes 一致），Pod 不重建；集成测试与 e2e 均覆盖。
