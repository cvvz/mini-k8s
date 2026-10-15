# e2e 并行执行与跨功能场景用例 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:**
- 让 e2e 套件在用例增多后仍然跑得快。
- 补充跨功能的复杂场景用例。

**背景（M10d 合入前的 full e2e）：**
- 146 个 spec 用时 1390 秒，各 spec 耗时之和为 1391 秒，说明套件完全串行。
- 最慢的用例主要在等真实事件：
  - `cluster up --workers` 约 97 秒；
  - HPA 扩缩约 77 秒；
  - PodGC 约 62 秒；
  - CronJob 等整分钟约 37 秒。
- 命名空间清理每个 spec 只占约 1 秒，不是瓶颈。
- 因此提速的关键是并行，而不是缩短单个用例。

**Architecture:**

- **并行执行**：
  - `make e2e` 改用 Ginkgo CLI，通过 go.mod 的 `tool` 指令固定版本，用 `go tool ginkgo` 运行。
  - 默认 `--procs=4`（`E2E_PROCS` 可调），Serial 用例在其他进程全部结束后单独运行。
  - 用例本来就各用独立命名空间、唯一的 Ingress host 和唯一的节点标签键；固定的 ClusterIP/NodePort 取自静态段。大部分用例无需改动即可并行。
- **Serial 约束**：
  - 审查所有触及集群级状态的用例：cordon/taint、停节点或组件、metrics-server 缩到 0、PodGC 的假节点、资源请求记账等。
  - 唯一漏标的是"metrics-server 停机"用例：它是 Disruptive 却没标 Serial，并行时会让 HPA 和 kk top 用例失败。
  - 框架在 BeforeEach 中强制要求带 Disruptive 标签的 spec 必须是 Serial，以后不会再漏。
- **场景用例**：
  - 新增 `test/e2e/scenarios` 包（`[sig-scenarios]`），每个 spec 串起多项功能。
  - Ingress 辅助函数移入 framework 供复用。

| 场景 | 覆盖的组合 | 并行性 |
| --- | --- | --- |
| Application stack | ConfigMap/Secret（envFrom、secretKeyRef）+ StatefulSet（每副本 PVC、headless DNS）+ Deployment + Service + Ingress 负载均衡；store Pod 重建后数据与节点不变、DNS 指向新 IP；改 ConfigMap 后 `rollout restart` 期间请求零失败；删除命名空间后容器与动态供给的 PV 全部回收 | 并行 |
| Batch pipeline | Indexed Job（并行 3）经 WaitForFirstConsumer 的共享 PVC 写分片 → 所有 Pod 被调度到卷所在节点 → TTL 删除 Job 与 Pod 后数据仍在 → 第二个 Job 汇总并通过 `kk logs` 校验 | 并行 |
| Multi-tenancy | SA + RoleBinding(edit) 以 `--as` 部署和扩缩；跨命名空间与集群级资源被拒（文案对齐 Kubernetes）；Pod 内 token 权限一致；ClusterRole/ClusterRoleBinding 放开再收回；删除 SA 后 token 失效（401），已部署的负载继续运行 | 并行（集群级对象名唯一） |
| Safe rollouts | maxUnavailable=0 时发布一个永不就绪的版本：Ingress 流量只落在旧 Pod、`rollout status` 报 progress deadline、Progressing=False；`rollout undo` 后旧 Pod 原样保留（UID 不变），全程请求零失败；history 中恢复的模板成为 revision 3 | 并行 |
| Scheduled work | CronJob 运行重叠：Forbid 时跳过并记录 JobAlreadyActive，改为 Replace 后下一轮删除并替换活跃 Job（及其 Pod） | 并行（约 3 分钟） |
| Node failure | worker-2 失联：无状态 Pod 在容忍期后迁到 worker-1，Service 持续可用；StatefulSet Pod 被驱逐但不替换（at-most-one），强删后新 Pod 因本地卷节点亲和而 Unschedulable；节点恢复后 db-0 带着原数据在原节点启动，扩容后 API 重新分布到 worker-2 | Disruptive + Serial |

## 任务

- [x] go.mod 增加 `tool github.com/onsi/ginkgo/v2/ginkgo`；Makefile 的 `e2e` 改用 `go tool ginkgo -v --procs=$(E2E_PROCS)`，`--label-filter`、`--focus` 照旧映射。
- [x] metrics-server 停机用例加 Serial；framework 在 BeforeEach 中检查 Disruptive ⇒ Serial。
- [x] `framework.NewIngress`/`IngressPath` 取代 network 包内的私有函数。
- [x] 新建 scenarios 包与上表六个 spec，并在 e2e_test.go 中空导入。
- [x] 跑 full e2e（并行）：152/152 通过，772 秒（基线 1390 秒）；只有新场景中的一条断言需要改（kk describe deployment 不输出 Conditions 表格，改为直接读 API）。
- [x] 更新 test/e2e/README.md（并行、Serial 约定、scenarios 目录）、README（en/zh）中的 e2e 说明，以及 spec §11、§12。
