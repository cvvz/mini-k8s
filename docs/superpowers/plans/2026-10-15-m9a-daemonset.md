# M9a DaemonSet 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 新增 DaemonSet 与 ControllerRevision。DaemonSet 控制器在每个符合条件的节点上运行一个 Pod，支持 RollingUpdate（maxUnavailable、maxSurge）
与 OnDelete 两种更新策略，并维护修订历史。`kk rollout status|history|undo|restart` 支持 daemonset。kube-proxy 像 kubeadm 一样改为 `kube-system`
中的 DaemonSet 插件，不再是各节点的 static Pod。

**Architecture:**

- **API**：`apps/v1` 新增 DaemonSet 与 ControllerRevision。
  - Strategy 负责默认值：RollingUpdate、maxUnavailable 1、maxSurge 0、revisionHistoryLimit 10。
  - 模板变化时递增注解 `deprecated.daemonset.template.generation`。
- **控制器**（`pkg/controller/daemon`）：
  - **修订历史**：每个模板版本对应一个 ControllerRevision，名称为 `<ds>-<hash>`，数据为 `{"spec":{"template":{…,"$patch":"replace"}}}`。回滚到旧模板时把该修订的号提升为最大值加一，超出历史上限的旧修订会被清理。
  - **判断节点是否运行**：按 nodeSelector、节点亲和、污点（NoSchedule 不容忍就不新建，NoExecute 不容忍就删除）。
  - **创建 Pod**：带 nodeAffinity `matchFields metadata.name In [node]`、默认容忍，以及标签 `controller-revision-hash`、`pod-template-generation`，由默认调度器绑定。
  - **其他维护**：删除多余的、失败的、以及落在不该运行节点上的 Pod，然后按策略滚动更新。
  - **状态**：更新 status 的八个计数字段。

**Tech Stack:** Go 1.26、Ginkgo e2e。

## Global Constraints

- 只在 WSL（Ubuntu 24.04）中构建与测试；文档只出现 Linux 命令。
- 每个功能附带单元测试与 e2e；里程碑验收跑相关 e2e。
- 行为与文案以 AKS（v1.35）实测为准（2026-10-15，临时命名空间 `mini-k8s-probe-m9a`）。

## 语义（AKS 实测）

| 项 | 规则 / 文案 |
| --- | --- |
| kk get ds | `NAME DESIRED CURRENT READY UP-TO-DATE AVAILABLE NODE SELECTOR AGE`（nodeSelector 为空时 `<none>`）；wide 加 `CONTAINERS IMAGES SELECTOR` |
| kk get controllerrevisions | `NAME CONTROLLER REVISION AGE`，CONTROLLER 形如 `daemonset.apps/probe` |
| Pod | `generateName: <ds>-`；标签 `controller-revision-hash`、`pod-template-generation`；属主 DaemonSet；`affinity.nodeAffinity.required…matchFields metadata.name In [<node>]`；默认容忍 not-ready/unreachable:NoExecute、disk-pressure/memory-pressure/pid-pressure/unschedulable:NoSchedule（hostNetwork 另加 network-unavailable:NoSchedule） |
| 事件 | `Normal SuccessfulCreate Created pod: <pod>`、`Normal SuccessfulDelete Deleted pod: <pod>` |
| rollout status | `Waiting for daemon set "x" rollout to finish: 1 out of 2 new pods have been updated...`、`Waiting for daemon set "x" rollout to finish: 1 of 2 updated pods are available...`、`daemon set "x" successfully rolled out`；未观察到新 spec：`Waiting for daemon set spec update to be observed...` |
| rollout history | `daemonset.apps/x` + `REVISION  CHANGE-CAUSE` 表；`--revision=N` 显示该修订的 Pod 模板 |
| rollout undo | `daemonset.apps/x rolled back`；被恢复的修订号变为最新（1,2 → 2,3） |
| 校验 | `spec.template.spec.restartPolicy: Unsupported value: "OnFailure": supported values: "Always"`；updateStrategy 类型只能为 `RollingUpdate`、`OnDelete`；selector 必须匹配模板标签、不可变 |
| nodeSelector 无匹配 | DESIRED 0，已有 Pod 被删除 |

## 决策

| 决策 | 选择 |
| --- | --- |
| 调度 | 与 Kubernetes 1.12+ 相同：控制器只设节点亲和，由默认调度器绑定（资源、端口等仍由调度器判断） |
| kube-proxy | 改为 `build/manifests/addons/kube-proxy.yaml` 的 DaemonSet（`tolerations: [{operator: Exists}]`）；删除两份 static Pod 清单与 worker 的清单目录挂载 |
| 失败 Pod | 删除并由下一轮重建；退避 1s 起翻倍，上限 5 分钟（Kubernetes 上限 15 分钟） |
| 不做 | 收养孤儿 Pod、`collisionCount` 冲突处理（哈希冲突时追加计数重算）以外的修订合并 |

### Task 1: API 与 kk

- [ ] 类型、默认值、校验、registry、scheme、client；`kk get ds|controllerrevisions`、describe；`kk rollout` 支持 daemonset。

### Task 2: 控制器

- [ ] `pkg/controller/daemon`：修订、节点判定、Pod 增删、滚动更新（含 surge）、状态；单元测试（节点判定、滚动算法）与集成测试。

### Task 3: kube-proxy 迁移、e2e 与文档

- [ ] kube-proxy DaemonSet 插件，删除 static 清单；e2e 中按标签查找 kube-proxy Pod。
- [ ] e2e（`[sig-apps] DaemonSet`）：按节点标签增减、删除后重建、滚动更新与回滚、kube-proxy 为 DaemonSet。
- [ ] README、spec；审查；PR。
