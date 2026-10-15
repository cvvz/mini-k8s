# M9c StatefulSet 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 新增 apps/v1 StatefulSet：
- 稳定的 Pod 身份：`<sts>-<序号>`，hostname 加 subdomain，可经 headless Service 解析。
- 按序或并行的创建与缩容。
- 按 `volumeClaimTemplates` 为每个序号创建 PVC，PVC 的保留策略支持 `whenDeleted` 与 `whenScaled`。
- 两种更新策略：RollingUpdate（支持 partition，从最大序号往下逐个替换）和 OnDelete。
- 修订历史与回滚。

kk 支持 `get/describe statefulsets`、`scale statefulset`，以及 `rollout status|history|undo|restart statefulset`。

**Architecture:**

- **修订历史**：DaemonSet 中的 ControllerRevision 管理迁到共享包 `pkg/controller/history`（对应 k8s 的同名包），DaemonSet 与 StatefulSet 共用。
  - 共用的部分：找到或创建当前模板的修订、回滚时把旧修订提为最新、收养孤儿修订、名字碰撞时提高 `collisionCount`、清理超出上限的修订。
  - 两者的差异由参数传入。
    - 修订上存放 hash 的标签：DaemonSet 用 `controller-revision-hash`，StatefulSet 用 `controller.kubernetes.io/hash`。
    - Pod 标签记录的内容：DaemonSet 是 hash，StatefulSet 是修订名。
    - 修订的注解。
- **StatefulSet 控制器**：`pkg/controller/statefulset`，按 k8s `stateful_set_control.go` 的语义实现。
  - 把 Pod 按序号分成两组：`[start, start+replicas)` 内的为 replicas，之外的为 condemned。
  - **replicas 组**：
    - Failed 或 Succeeded 的 Pod 先删除，等它消失后重建。
    - 缺失的 Pod 先补 PVC，再建 Pod。
    - OrderedReady 模式下，遇到未 Running 且 Ready 的 Pod 就停止推进。
  - **condemned 组**：按序号从大到小删除。OrderedReady 模式下一次只删一个，并等待前面的 Pod 都健康。
  - **滚动更新**：从最大序号往下，删除修订不等于 updateRevision 的 Pod，删一个就返回，等新 Pod 健康后再继续，直到 partition。
  - **新建 Pod 用哪个修订**：RollingUpdate 时，序号小于 partition 的 Pod 用 currentRevision，其余用 updateRevision。
  - **status**：计算 status；所有 Pod 都已更新且就绪时，把 currentRevision 推进到 updateRevision。
  - 用 Expectations 避免缓存滞后导致重复创建或删除。
- **PVC 保留策略**：按 k8s `updateClaimOwnerRefForSetAndPod` 维护 PVC 的 ownerReferences。
  - `whenDeleted: Delete`：PVC 以 StatefulSet 为 controller。
  - `whenScaled: Delete`：缩容的 Pod 删除前，把它的 PVC 的属主改为该 Pod。Pod 消失后由 GC 删除 PVC。

## AKS 实测语义（2026-10-15，mini-k8s-probe-m9c 命名空间）

| 项目 | 结果 |
| --- | --- |
| kubectl 列 | `NAME READY AGE`；`-o wide` 加 `CONTAINERS IMAGES` |
| 默认值 | replicas 1、podManagementPolicy OrderedReady、updateStrategy RollingUpdate + partition 0、revisionHistoryLimit 10、persistentVolumeClaimRetentionPolicy Retain/Retain；volumeClaimTemplates 补 volumeMode Filesystem |
| Pod | 名字 `<sts>-<序号>`，generateName `<sts>-`；标签：模板标签加 `controller-revision-hash: <修订名>`、`statefulset.kubernetes.io/pod-name`、`apps.kubernetes.io/pod-index`；`hostname` 为 Pod 名，`subdomain` 为 serviceName（不设 serviceName 时为空）；controller ownerRef |
| PVC | 名字 `<模板名>-<sts>-<序号>`；标签为模板标签加 selector 的 matchLabels（不含其余 Pod 模板标签）；Pod 卷名同模板名，`claimName` 指向该 PVC；`whenDeleted: Delete` 时 ownerRef 为 StatefulSet（controller、blockOwnerDeletion），Retain 时没有 ownerRef |
| 事件（statefulset-controller） | `SuccessfulCreate`：`Create Pod web-0 in StatefulSet web successful`、`Create Claim www-data-0 Pod data-0 in StatefulSet data success`；`SuccessfulDelete`：`Delete Pod web-2 in StatefulSet web successful` |
| 顺序 | OrderedReady：上一个 Pod Ready 后才建下一个；缩容从最大序号开始逐个删除；Parallel 同时创建 |
| 滚动更新 | partition 2、replicas 3 时只替换 web-2；status 为 currentReplicas 2、updatedReplicas 1、currentRevision 旧、updateRevision 新；partition 改回 0 后按 2、1、0 的顺序替换，完成后 currentRevision 等于 updateRevision |
| 修订 | `web-<hash>`，`kubectl get controllerrevisions` 的 CONTROLLER 列为 `statefulset.apps/web` |
| rollout status | `Waiting for partitioned roll out to finish: 0 out of 1 new pods have been updated...`、`Waiting for 1 pods to be ready...`、`partitioned roll out complete: 1 new pods have been updated...` |
| rollout history | `statefulset.apps/web ` 下列 `REVISION  CHANGE-CAUSE` |
| 缩容（whenScaled: Delete） | 被缩掉序号的 PVC 随 Pod 删除 |
| 删除 StatefulSet（whenDeleted: Delete） | PVC 一并删除 |
| 校验 | `spec.replicas: Invalid value: -1: must be greater than or equal to 0`；`spec.podManagementPolicy: Invalid value: "Sometimes": must be 'OrderedReady' or 'Parallel'`；`spec.updateStrategy: Invalid value: {"Type":"Never","RollingUpdate":null}: must be 'RollingUpdate' or 'OnDelete'` |
| 更新限制 | `spec: Forbidden: updates to statefulset spec for fields other than 'replicas', 'ordinals', 'template', 'updateStrategy', 'revisionHistoryLimit', 'persistentVolumeClaimRetentionPolicy' and 'minReadySeconds' are forbidden` |
| describe | `Replicas: 1 desired \| 1 total`、`Update Strategy: RollingUpdate`、`Partition: 0`、`Pods Status: 1 Running / 0 Waiting / 0 Succeeded / 0 Failed`、`Volume Claims:` |

## 决策

- 支持 `ordinals.start`。不实现 `maxUnavailable`（k8s 中为 alpha）。
- OnDelete 与 k8s 一样不推进 currentRevision。
- Pod 身份标签缺失时（例如收养的孤儿 Pod）补上标签，不修改 hostname 与 subdomain。
- PVC 正在删除时不建 Pod，等它消失（对应 k8s 的 PodClaimIsStale）。

## Tasks

- [ ] **Task 1 API**：`pkg/apis/apps/v1/statefulset.go`（类型、常量、`SetDefaultsStatefulSet`）；`validation` 中的
  `ValidateStatefulSet`/`ValidateStatefulSetUpdate`；`pkg/registry/apps/statefulset`；scheme（短名 `sts`）、typed client、registry。
  单测覆盖校验文案。
- [ ] **Task 2 history 包**：`pkg/controller/history`（`Sync`、`RevisionData`、`RevisionTemplate`、`ErrCollision`），DaemonSet 改用它，
  DaemonSet 的单测、集成测试保持通过。
- [ ] **Task 3 控制器**：`pkg/controller/statefulset`（`NewPod`、PVC 构造、保留策略 ownerRef、`sync`），接入 controller-manager。
  单测覆盖身份、PVC、ownerRef、修订选择；集成测试覆盖按序创建、按序缩容、partition 滚动、OnDelete、Parallel、PVC 与保留策略、收养。
- [ ] **Task 4 kk**：`get sts`（READY、wide 列）、`describe sts`、`scale statefulset`、`rollout status|history|undo|restart statefulset`
  （与 DaemonSet 共用基于修订的实现）。
- [ ] **Task 5 e2e**：`[sig-apps] StatefulSet`。
  - 按序创建、稳定网络身份（`web-0.web` DNS 与 /etc/hosts）。
  - 数据卷跨 Pod 重建保留。
  - 缩容与扩容，partition 滚动与回滚。
  - Parallel 与 PVC 保留策略。
  - 校验。
- [ ] **Task 6 文档**：README（中英）、v2 spec §12.13 与 §15、`examples/statefulset.yaml`。
