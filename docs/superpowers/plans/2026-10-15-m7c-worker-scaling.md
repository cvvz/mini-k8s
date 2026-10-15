# M7c worker 扩缩实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `kk cluster up --workers N` 把集群扩到或缩到 N 个 worker（1–10），像 kind 的多节点配置；缩容先 drain 再删节点；
新增 Pod 垃圾回收控制器（PodGC），删除绑定在已不存在节点上的 Pod。

**Architecture:** `compose.yaml` 仍定义 control-plane、worker-1、worker-2。`kk cluster up` 把 worker 数记在
`.mini-k8s/workers`，并据此生成 `.mini-k8s/compose.workers.yaml`：N > 2 时用 `extends` 复制 worker-1 为
worker-3…worker-N（改 hostname、`MINI_NODE`、`/var` 卷）；N = 1 时把 worker-2 的副本数设为 0；N = 2 时为空。之后每次
compose 调用（up、stop、down）都带 `-f compose.yaml -f .mini-k8s/compose.workers.yaml`。缩容时，若 API 可用先
`drain` 要移除的 worker，再 `compose up --remove-orphans` 删除其容器、删除其 `/var` 卷，集群就绪后删除编号超过 N 的
`worker-K` Node 对象；集群停着时缩容无法 drain，留在这些节点上的 Pod 由 PodGC 清理。

**Tech Stack:** Go 1.26、Docker Compose（`extends`、`deploy.replicas`）、Ginkgo e2e。

## Global Constraints

- 只在 WSL（Ubuntu 24.04）中构建与测试；文档只出现 Linux 命令。
- 每个功能附带单元测试与 e2e；里程碑验收跑相关 e2e。
- 不破坏默认拓扑：不带 `--workers` 时沿用上次的 worker 数（首次为 2）；e2e 套件要求默认的 3 个节点。

## 语义（Kubernetes 源码）

| 项 | 规则 |
| --- | --- |
| PodGC 周期 | 每 20s 检查一次（`gcCheckPeriod`） |
| 孤儿 Pod | `spec.nodeName` 指向的节点不在 informer 中：先隔离 40s（`quarantineTime`），到期后用 GET 确认节点仍不存在，才处理 |
| 处理方式 | 未终止的 Pod 先写状态：`phase: Failed`，条件 `DisruptionTarget=True`、reason `DeletionByPodGC`、message `PodGC: node no longer exists`；然后以 `gracePeriodSeconds: 0` 删除 |
| 节点删除的连带清理 | Lease、镜像 Pod 由属主引用经垃圾回收器删除；Pod CIDR 由 nodeipam 释放；host-gw 路由在各节点按 Node 列表收敛 |
| 缩容 drain | 同 `kk drain NODE --force --delete-emptydir-data --ignore-daemonsets --timeout 2m`：节点要被移除，无控制器的 Pod 也只能删除 |

## 决策

| 决策 | 选择 |
| --- | --- |
| 新 worker 的定义 | 生成的 compose 覆盖文件 + `extends: {file: compose.yaml, service: worker-1}`；只覆盖 hostname、`MINI_NODE` 与 `/var` 卷（同一目标路径的卷覆盖合并），挂载与 `depends_on` 沿用 worker-1 |
| 数量的记录 | `.mini-k8s/workers`（纯文本整数）；覆盖文件是它的派生物，每次 `kk cluster up/down`、`kk image load` 都重新生成 |
| N = 1 | worker-2 设 `deploy.replicas: 0`，compose 删除其容器；不改 compose.yaml |
| 上限 | 10（笔记本资源；Pod CIDR 本身允许 256 个节点） |
| 缩容时集群停着 | 不 drain；就绪后删 Node，PodGC 清理其 Pod，控制器在别处重建 |
| 卷 | 被移除 worker 的 `mini-k8s_worker-K-var` 卷随之删除，再加回来时是全新磁盘 |
| e2e 拓扑 | 套件开始时检查节点恰为 control-plane、worker-1、worker-2；扩缩用例结束时缩回 2 |

## 文件

- Modify `cmd/kk/cluster.go`：`workers` 数量、覆盖文件生成、compose 参数、缩容（drain、删卷、删 Node）。
- Modify `cmd/kk/args.go`、`cmd/kk/main.go`：`--workers` 标志与用法。
- Create `pkg/controller/podgc/gc_controller.go` + 测试；Modify `cmd/mini/controller_manager.go`。
- Modify `pkg/apis/core/v1/types.go`：`DisruptionTarget` 条件类型。
- e2e：`test/e2e/lifecycle/cluster.go`（扩到 3 再缩回 2）、`test/e2e/node/podgc.go`（孤儿 Pod）、
  `test/e2e/framework`（默认拓扑检查）。
- 文档：README（中英）、v2 spec 实施记录与代码映射、本计划。

### Task 1: PodGC 控制器

- [ ] `pkg/controller/podgc`：`NewController(c, pods, nodes)`，`Run` 每 `Period` 调 `gc`；`gc` 记录每个缺失节点首次发现的
  时间，满 `Quarantine` 后 GET 确认 NotFound，再对其上 Pod 写 Failed + DisruptionTarget 并 grace 0 删除；节点重新出现
  则清除记录。
- [ ] 测试（集成测试服务器）：缺失节点上的 Pod 在隔离期内保留、到期后被删除且删除前状态为 Failed；存在节点与未调度的
  Pod 不受影响；隔离期内节点又出现则不删除。
- [ ] 接入 kube-controller-manager。

### Task 2: `kk cluster up --workers N`

- [ ] `loadWorkers(root)`、`saveWorkers(root, n)`、`workersOverride(n)`、`workerNames(n)`；`cluster` 增加 `root`、`workers`
  字段，`compose(args...)` 返回带两个 `-f` 的命令；`up/down/image load/waitReady` 用动态节点列表。
- [ ] 缩容：`drainRemoved`（API 健康时）→ 写新数量与覆盖文件 → compose up → `docker volume rm -f` → 就绪后
  `deleteRemovedNodes`。
- [ ] 单元测试：覆盖文件内容（N=1、2、3）、数量读写与校验、down 的命令序列、`deleteRemovedNodes` 只删编号超出的 worker。

### Task 3: e2e 与文档

- [ ] lifecycle：扩到 3 → worker-3 Ready、有 Pod CIDR、偏好它的 Pod 在其上运行且跨节点可达；缩回 2 → Pod 被驱逐并在
  其他 worker 重建、Node/容器/卷都消失。
- [ ] node：PodGC 孤儿 Pod（API 创建的不可调度节点 + 绑定其上的带 finalizer 的 Pod，删除节点后 Pod 被标 Failed 并删除）。
- [ ] BeforeSuite 默认拓扑检查。
- [ ] README、spec 更新；跑相关 e2e；审查；PR 合入。
