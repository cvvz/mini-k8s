# M10c Leader Election 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** kube-controller-manager 与 kube-scheduler 像 Kubernetes 默认（`--leader-elect=true`）那样，通过 `kube-system` 中同名的 Lease
选主。多个副本同时运行时只有 leader 工作；leader 退出时释放租约，其他副本立即接管；leader 续约失败时退出进程，由 kubelet 重启后重新参选。

**Architecture:**

- **`pkg/client/leaderelection`**：照搬 client-go `tools/leaderelection` 的 LeaseLock 语义。
  - **tryAcquireOrRenew**：Lease 不存在时创建。被他人持有时，用本地"观察到记录变化的时间 + leaseDuration"判断是否过期，避免各机器之间的时钟偏差。过期或自己持有时用 resourceVersion 乐观更新：易主时 `leaseTransitions` 加一、更新 acquireTime；续约时保留它们。
  - **acquire**：每个 RetryPeriod（带 1.2 倍抖动）尝试一次。
  - **renew**：每个 RetryPeriod 续约一次，在 RenewDeadline 内续约不成功即停止领导。
  - **ReleaseOnCancel**：退出时把 holderIdentity 清空、leaseDurationSeconds 设为 1。
  - **回调与事件**：回调为 OnStartedLeading、OnStoppedLeading（总会被调用）、OnNewLeader；在 Lease 上记录 `LeaderElection` 事件，内容为 `<id> became leader` 或 `<id> stopped leading`。
- **组件**（`cmd/mini/leader.go`）：
  - identity 为 `<hostname>_<uuid v4>`，参数用 Kubernetes 默认值：15s、10s、2s。
  - 只有当选后才启动 informer 与控制器（调度器同理）。
  - 失去领导权时 `leaderelection lost` 退出进程；收到 SIGTERM 时释放租约后正常退出。
  - `MINI_LEADER_ELECT=false` 时关闭选主。

## 观察

- **apiserver 不可用时会丢主**：apiserver 停机超过 RenewDeadline（10s）时，controller-manager 与 scheduler 无法续约而退出，与 Kubernetes 相同。kubelet 重启它们后，新实例最多等 15s 让旧租约过期再当选（旧进程未释放租约）。
- **重启后的静态 Pod 会参与竞争**：用 `crictl stop` 停掉 leader 时，kubelet 会立即重启静态 Pod，重启后的实例与 standby 同时竞争空出的租约，哪个赢都是合法结果。因此 e2e 改为移走静态 Pod 清单：kubelet 优雅停止 leader（释放租约）且不再重启，接管方因此是确定的。

## Tasks

- [x] `pkg/client/leaderelection`，单测用测试 API Server 覆盖三种情况：
  - 释放后立即切换。
  - 不释放时等过期再切换。
  - 被他人占用后续约失败、停止领导。
- [x] controller-manager 与 scheduler 接入选主。
- [x] e2e `[sig-cluster-lifecycle] Leader election`：
  - 两个租约有人持有，且在持续续约。
  - Disruptive：在 control-plane 节点上启动一个 standby controller-manager。移走静态清单后，standby 立即接管，控制器照常工作，并记录了 LeaderElection 事件；恢复清单后静态 Pod 作为 standby 等待；删除 standby 后静态 Pod 重新当选。
- [x] 文档：README（中英）、spec §12.16 与 §15。
