# M3b 探针与资源限制实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 容器支持 liveness/readiness/startup 探针（httpGet、tcpSocket、exec）以及 CPU/内存的 requests/limits（落到 cgroup，内存超限 OOMKilled），Pod 有 QoS 等级，节点上报容量。

**Architecture:** kubelet 新增 `pkg/kubelet/prober`：每个“运行中的容器实例 × 探针类型”一个 worker 按周期探测，结果供 kuberuntime 计算 `ready`/`started`；liveness 或 startup 失败达到阈值时由 kubelet 停止该容器，随后按重启策略重启。资源经 CRI `LinuxContainerResources` 交给 runc 写入 cgroup v2。数量（Quantity）按 Kubernetes 的写法解析与序列化。

**Tech Stack:** Go 1.26、CRI v1、cgroup v2、Ginkgo e2e。

## Global Constraints

- 只在 WSL（Ubuntu 24.04）中构建与测试；文档只出现 Linux 命令。
- 每个功能附带单元测试与 e2e；字段名、默认值、事件原因与消息格式与 Kubernetes 一致。

## 语义（Kubernetes 文档与源码）

| 项 | 规则 |
| --- | --- |
| 探针字段默认值 | `initialDelaySeconds` 0、`periodSeconds` 10、`timeoutSeconds` 1、`successThreshold` 1、`failureThreshold` 3；liveness/startup 的 `successThreshold` 必须为 1 |
| 结果判定 | httpGet：200 ≤ 状态码 < 400；tcpSocket：能建立连接；exec：退出码 0 |
| readiness | 首次成功前为未就绪；连续 `failureThreshold` 次失败变为未就绪，连续 `successThreshold` 次成功恢复；只影响 `ready` 与 Pod Ready 条件，不重启 |
| liveness | 连续 `failureThreshold` 次失败后停止容器（宽限期为 Pod 的 `terminationGracePeriodSeconds`），按重启策略重启；事件 `Unhealthy`（`Liveness probe failed: ...`）与 `Killing`（`Container web failed liveness probe, will be restarted`） |
| startup | 成功前 liveness/readiness 不生效，`started: false`；失败达到阈值同 liveness 处理；成功后不再探测 |
| 资源 | `cpu`、`memory`；只写 limits 时 requests 默认等于 limits；requests ≤ limits |
| cgroup 映射 | `cpu.shares = requests.cpu(milli) × 1024 / 1000`（最少 2）；`cpu quota = limits.cpu(milli) × 100000 / 1000`、period 100000；`memory.max = limits.memory`，禁止 swap |
| QoS | 所有容器 cpu 与 memory 都有 limits 且 requests == limits → Guaranteed；都没有 → BestEffort；否则 Burstable。创建时写入 `status.qosClass` |
| OOM | 超出内存限制的容器被内核杀死，containerd 报告原因 `OOMKilled`，退出码 137 |
| 节点容量 | `status.capacity`/`allocatable`：`cpu`（核数）、`memory`（`/proc/meminfo`）、`pods`（110） |

## 决策

| 决策 | 选择 | 理由 |
| --- | --- | --- |
| 拆分 | M3b：探针与资源；M3c：emptyDir/hostPath 卷与 hostNetwork | 控制 PR 规模 |
| Quantity | 自研 `pkg/api/resource`：支持十进制与 `m`、`k/M/G/T`、`Ki/Mi/Gi/Ti` 后缀，内部以毫单位 int64 保存，JSON 中保留原字符串 | 不引入 apimachinery；覆盖常见写法 |
| 探测发起方 | kubelet（节点容器网络命名空间）直接访问 Pod IP；exec 走 CRI `ExecSync` | 与 kubelet 相同 |
| 停止容器 | 探针 worker 判定失败后回调 kubelet：记录事件、`StopContainer`、唤醒 Pod worker | 重启沿用 kuberuntime 的退避与重启策略 |
| 资源感知调度 | 不在本里程碑（M7） | 路线图既定 |
| 测试辅助 | demo 增加 `/healthz`、`/ready` 及开关（`/fail-live`、`/fail-ready`、`/recover`）与 `DEMO_START_DELAY`；`mini cat`、`mini alloc` | scratch 镜像中没有 shell |

## 文件

| 文件 | 内容 |
| --- | --- |
| `pkg/api/resource` | Quantity 解析、比较、JSON |
| `pkg/apis/core/v1` | `Probe`、`ProbeHandler`、`HTTPGetAction`、`TCPSocketAction`、`ExecAction`、`ResourceRequirements`、`ResourceList`、`ContainerStatus.Started`、`PodStatus.QOSClass`、`NodeStatus.Capacity/Allocatable`；默认值；`GetPodQOS` |
| `pkg/apis/core/validation` | 探针（恰好一个处理器、阈值、端口）、资源名与 requests ≤ limits |
| `pkg/registry/core/pod` | 创建时写 QoS |
| `pkg/kubelet/prober` | 探测执行器与 worker 管理 |
| `pkg/kubelet/kuberuntime` | `ready`/`started` 计算；资源映射；`StopContainer` 包装 |
| `pkg/kubelet/kubelet.go` | 接入 prober；节点容量 |
| `cmd/mini` | demo 健康检查端点；`cat`、`alloc` |
| `cmd/kk` | `describe` 已显示 status；无需改动之外补充 QoS |
| `test/e2e/node/probes.go`、`resources.go` | 新用例 |

## Tasks

### Task 1: Quantity 与 API
- [x] `pkg/api/resource` 与单元测试（解析、毫单位、二进制后缀、比较、非法输入）。
- [x] 类型、默认值、校验、QoS 与单元测试。

### Task 2: 资源落地
- [x] `containerConfig` 填 `LinuxContainerResources`；单元测试检查 shares/quota/memory/swap。
- [x] 节点容量上报。

### Task 3: prober
- [x] 执行器（http/tcp/exec，超时）与 worker（初始延迟、周期、阈值、按容器实例重建）。
- [x] kuberuntime 用探针结果计算 `ready`/`started`；startup 未通过时 readiness/liveness 不生效。
- [x] liveness/startup 失败：事件、停止容器、重启；单元测试使用假执行器与假时钟。

### Task 4: e2e 与文档
- [x] 探针：readiness 切换影响网关（503/200）、liveness 失败重启并有事件、startup 保护慢启动、tcpSocket 与 exec 探针。
- [x] 资源：cgroup 中的 `memory.max` 与 `cpu.max`、OOMKilled、QoS 三种等级、节点容量。
- [x] README（中英）、v2 规格、本计划勾选。

## 结果

全部任务完成：e2e 59 个 spec 通过（新增 8 个，约 300 秒），单元测试与 `-race` 通过。快速开始的示例
`examples/demo.yaml` 增加了 requests/limits 与 readiness/liveness 探针，README 演示 readiness 失败后网关
只转发给另一个 Pod。

代码审查未发现 critical 问题；4 条非 critical 问题记入 [待办](../backlog.md)。
