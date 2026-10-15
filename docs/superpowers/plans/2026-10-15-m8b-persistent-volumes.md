# M8b PersistentVolume、PVC 与动态供给实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 新增 PersistentVolume、PersistentVolumeClaim、StorageClass；PV 控制器负责绑定与回收，PVC/PV 保护 finalizer 防止删除仍在用的对象；
调度器的 VolumeBinding 插件处理 `WaitForFirstConsumer` 与卷的节点亲和；kind 式的 local-path 供给器作为插件运行，提供默认 StorageClass
`standard`；kubelet 挂载 `persistentVolumeClaim` 卷。

**Architecture:**

- **API 服务器**：创建 PVC 时，若未写 `storageClassName`，填入默认 StorageClass（相当于 DefaultStorageClass 准入插件），并加上
  `kubernetes.io/pvc-protection`；创建 PV 时加上 `kubernetes.io/pv-protection`。
- **kube-controller-manager**：
  - `persistentvolume` 控制器把 PVC 与合适的 PV 绑定（两边互指，phase 为 Bound）；需要供给时给 PVC 打上
    `volume.kubernetes.io/storage-provisioner`；PVC 被删除后，PV 变为 Released，按回收策略处理。
  - `pvcprotection`、`pvprotection` 控制器在对象不再被使用时移除 finalizer。
- **调度器**：
  - PreFilter 检查 PVC 是否存在，以及 Immediate 类型的 PVC 是否已绑定。
  - Filter 检查已绑定 PV 的节点亲和，以及未绑定的 PVC 能否在该节点上绑定或供给。
  - 选定节点后，给待供给的 PVC 写 `volume.kubernetes.io/selected-node`，或把静态 PV 预绑定到 PVC；然后异步等待 PVC 绑定完成，再绑定 Pod。
- **local-path 供给器**（`mini local-path-provisioner`，以插件 Deployment 运行）：
  - 为选定节点创建 PV `pvc-<uid>`，类型为 hostPath（`/var/local-path-provisioner/<pv>_<ns>_<pvc>`，`DirectoryOrCreate`），带该节点的
    nodeAffinity。
  - 回收时在该节点上运行辅助 Pod 删除目录，然后删除 PV。
- **kubelet**：通过 PVC → PV 找到 hostPath，未绑定时报 FailedMount。

**Tech Stack:** Go 1.26、Ginkgo e2e。

## Global Constraints

- 只在 WSL（Ubuntu 24.04）中构建与测试；文档只出现 Linux 命令。
- 每个功能附带单元测试与 e2e；里程碑验收跑相关 e2e。
- 行为与文案以 AKS（v1.35）实测与 Kubernetes 源码为准（2026-10-15，临时命名空间 `mini-k8s-probe-m8b`，只建 PVC，PV 由 AKS 自身供给、随删除回收）。

## 语义

| 项 | 规则 / 文案 |
| --- | --- |
| kk get | sc：`NAME PROVISIONER RECLAIMPOLICY VOLUMEBINDINGMODE ALLOWVOLUMEEXPANSION AGE`，默认类显示 `standard (default)`；pvc：`NAME STATUS VOLUME CAPACITY ACCESS MODES STORAGECLASS VOLUMEATTRIBUTESCLASS AGE`（`<unset>`）；pv：`NAME CAPACITY ACCESS MODES RECLAIM POLICY STATUS CLAIM STORAGECLASS VOLUMEATTRIBUTESCLASS REASON AGE`；访问模式缩写 RWO/ROX/RWX/RWOP |
| PVC 校验 | `spec.accessModes: Required value: at least 1 access mode is required`；`spec.accessModes: Unsupported value: "X": supported values: "ReadOnlyMany", "ReadWriteMany", "ReadWriteOnce", "ReadWriteOncePod"`；`spec.resources[storage]: Required value`；更新：`spec: Forbidden: spec is immutable after creation except resources.requests and volumeAttributesClassName for bound claims`（控制器首次写入 `volumeName` 例外） |
| PVC 默认值 | `volumeMode: Filesystem`；未写 `storageClassName` 时用带 `storageclass.kubernetes.io/is-default-class: "true"` 的类；`""` 表示不用类 |
| 绑定 | PV 与 PVC 匹配：类相同、访问模式包含请求的全部、容量不小于请求、selector 匹配、volumeMode 相同，取最小的；PV `spec.claimRef` 指向 PVC（含 uid）且注解 `pv.kubernetes.io/bound-by-controller: "yes"`，`status.phase: Bound`；PVC `spec.volumeName`，注解 `pv.kubernetes.io/bind-completed: "yes"`、`pv.kubernetes.io/bound-by-controller: "yes"`，`status` 带 phase、accessModes、capacity |
| WaitForFirstConsumer | 无消费者时事件 `Normal WaitForFirstConsumer waiting for first consumer to be created before binding`（persistentvolume-controller）；调度器选定节点后在 PVC 上写 `volume.kubernetes.io/selected-node` |
| 供给 | 控制器给 PVC 打 `volume.kubernetes.io/storage-provisioner` 与 `volume.beta.kubernetes.io/storage-provisioner`，事件 `Normal ExternalProvisioning Waiting for a volume to be created either by the external provisioner '<p>' or manually by the system administrator. If volume creation is delayed, please verify that the provisioner is running and correctly registered.`；供给器事件 `Normal Provisioning External provisioner is provisioning volume for claim "ns/name"`、`Normal ProvisioningSucceeded Successfully provisioned volume pvc-<uid>`；PV 注解 `pv.kubernetes.io/provisioned-by` |
| 类不存在 | `Warning ProvisioningFailed storageclass.storage.k8s.io "nonexistent" not found` |
| 调度 | PVC 不存在：`0/3 nodes are available: persistentvolumeclaim "nope" not found.`；未绑定的 Immediate PVC：`... pod has unbound immediate PersistentVolumeClaims.`；PV 节点亲和不符：`node(s) had volume node affinity conflict`；静态 PV 无可用且无供给器：`node(s) didn't find available persistent volumes to bind` |
| 保护 | PVC 被 Pod 使用时删除，变为 `Terminating`（finalizer `kubernetes.io/pvc-protection`），describe 显示 `Used By: <pod>`；Pod 删除后 PVC 消失；PV 绑定期间删除同理（`kubernetes.io/pv-protection`） |
| 回收 | PVC 删除后 PV 变为 `Released`；`Delete` 策略的供给 PV 由供给器删除存储与 PV（AKS 约 20s）；`Retain` 保持 Released；非供给的 `Delete` PV：`Failed`（`error getting deleter volume plugin for volume "x": no deletable volume plugin matched`） |
| 节点 | PV 目录在节点容器的 `/var/local-path-provisioner`（节点磁盘卷上，跨重启保留） |

## 决策

| 决策 | 选择 |
| --- | --- |
| 供给器 | 仿 kind 的 rancher local-path-provisioner：独立组件，以插件部署在 `local-path-storage` 命名空间，供给器名 `mini-k8s.io/local-path`，默认类 `standard`（`WaitForFirstConsumer`、`Delete`） |
| PV 卷类型 | hostPath + `DirectoryOrCreate` + 节点亲和（与 local-path-provisioner 相同），kubelet 无需新卷插件 |
| 调度绑定 | 需要绑定卷的 Pod 在 goroutine 中完成 PreBind 与绑定（kube-scheduler 的 binding cycle），等待期间该 Pod 不重复调度；超时（2 分钟）或失败则撤销 selected-node 后退避重试 |
| 不做 | 卷扩容、VolumeAttributesClass、CSI、Recycle（视为不支持，PV 变 Failed）、块设备 volumeMode |

### Task 1: API

- [ ] 类型、校验、默认值、registry（默认类、保护 finalizer）、scheme、client、kk 列与 describe。

### Task 2: 控制器

- [ ] persistentvolume 绑定/回收控制器；pvcprotection、pvprotection；集成测试。

### Task 3: 供给器、调度器、kubelet

- [ ] `pkg/volume/localpath` 供给器与 `mini local-path-provisioner`、`mini rm`；插件清单（Namespace、Deployment、StorageClass）。
- [ ] VolumeBinding 插件与异步绑定；kubelet PVC 卷。

### Task 4: e2e 与文档

- [ ] 动态供给（WFFC、节点亲和、数据持久、保护、回收删除目录）、静态 PV（Retain、预绑定 volumeName）、PVC 缺失与类缺失、校验。
- [ ] README、spec；审查；PR。
