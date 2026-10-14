# M3a 多容器 Pod、init 容器、重启策略与 exec 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Pod 支持多个容器与 init 容器、`restartPolicy` 三种策略和终态（Succeeded/Failed），并提供 `kk exec`。

**Architecture:** 在 M2 的 CRI kubelet 上把单容器 SyncPod 改写为 Kubernetes 式的“先 init 容器顺序执行、再并行维护业务容器”的状态机；Pod 相位与条件按 kubelet `getPhase` 的规则计算。exec 走 API Server → kubelet → CRI `ExecSync`（非交互）。

**Tech Stack:** Go 1.26、CRI v1（containerd 2.2）、Ginkgo e2e。

## Global Constraints

- 只在 WSL（Ubuntu 24.04）中构建与测试；文档只出现 Linux 命令。
- 每个功能附带单元测试与 e2e；不确定的语义以 AKS（`kubectl --context testaks`，临时命名空间）或 k8s 源码为准。
- 字段名、状态原因、条件原因与消息与 Kubernetes 保持一致。

## 语义（2026-10-15 在 AKS 上核对）

| 场景 | Pod phase | `kubectl get` STATUS | 要点 |
| --- | --- | --- | --- |
| init 容器运行中 | Pending | `Init:0/2` | 未开始的 init 与业务容器 waiting `PodInitializing`；Initialized=False/`ContainersNotInitialized` |
| init 失败，`restartPolicy: Never` | Failed | `Init:Error` | 业务容器不启动；Ready=False/`PodFailed` |
| init 失败，Always/OnFailure | Pending | `Init:CrashLoopBackOff` | init 容器带退避重试 |
| init 全部完成 | — | — | init 状态 terminated `Completed`，`ready: true` |
| Never：容器退出 0 与非 0 | Failed | `Error` | 全部退出且有失败 → Failed；全部 0 → Succeeded/`Completed` |
| OnFailure：一个退出 0、一个运行 | Running | `NotReady` | 退出 0 的不重启；ContainersReady=False，消息 `containers with unready status: [ok]` |
| Always：一个退出 0 | Running | `NotReady`/`CrashLoopBackOff` | 退出 0 也重启 |
| exec 已结束的 Pod | — | — | 拒绝：`cannot exec into a container in a completed pod; current phase is Failed` |
| 多容器 `logs` 未指定 `-c` | — | — | API 要求指定容器；kubectl 客户端默认第一个并提示 `Defaulted container "a" out of: a, b` |

## 决策

| 决策 | 选择 | 理由 |
| --- | --- | --- |
| 拆分 | M3a（本计划）：多容器、init、重启策略、exec；M3b：探针、资源限制、卷、hostNetwork | 控制单个 PR 规模，M3b 依赖 M3a 的多容器状态机 |
| exec 协议 | `POST /api/v1/namespaces/{ns}/pods/{name}/exec?container=&command=...`，响应 JSON `{stdout, stderr, exitCode}`；kubelet 用 CRI `ExecSync` | Kubernetes 用 SPDY/WebSocket 流式协议；本项目只做非交互命令（无 stdin/tty），实现量小一个数量级，README 明确说明差异 |
| kk exec 退出码 | 透传命令退出码，并在 stderr 打印 `command terminated with exit code N` | 与 kubectl 一致 |
| 默认容器 | API 要求多容器 Pod 指定容器；kk 的 `logs`/`exec` 未给 `-c` 时取第一个容器并提示 | 与 API Server/kubectl 的分工一致 |
| 终态 Pod | kubelet 停止其沙箱（释放 IP），保留容器与日志直到 Pod 被删除；之后不再同步 | 与 kubelet 对终态 Pod 的处理一致 |
| 沙箱重建 | 新沙箱中 init 容器重新执行 | Kubernetes 语义 |
| 删除时停止容器 | 所有容器并行 StopContainer | 总耗时等于宽限期而非宽限期×容器数 |
| 测试辅助 | `mini sleep DURATION`（成功退出的短任务） | scratch 镜像中没有 shell/sleep |

## 文件

| 文件 | 内容 |
| --- | --- |
| `pkg/apis/core/v1/types.go`、`defaults.go` | `PodSpec.InitContainers`、`RestartPolicy`；`PodStatus.InitContainerStatuses`；`PodExecResult`；默认 Always |
| `pkg/apis/core/validation`、`pkg/apis/apps/validation` | 容器名在 init 与业务容器间唯一、`restartPolicy` 枚举；Deployment 模板只允许 Always |
| `pkg/kubelet/kuberuntime/sync.go`、`status.go` | 多容器状态机；phase/conditions 计算 |
| `pkg/kubelet/kuberuntime/kill.go`、`exec.go` | 并行停止；终态 Pod 停沙箱；`Exec`（ExecSync） |
| `pkg/kubelet/kubelet.go` | init 容器事件；`/exec` 接口 |
| `pkg/apiserver/subresources.go` | `exec` 子资源；日志容器名包含 init 容器 |
| `pkg/client/client.go` | `PodExec` |
| `cmd/kk` | `kk exec`；logs/exec 默认容器提示；STATUS/READY/RESTARTS 按 kubectl 规则 |
| `cmd/mini/sleep.go` | `mini sleep DURATION` |
| `test/e2e/framework`、`test/e2e/node/pods.go` | 多容器构造器；新用例 |

## Tasks

### Task 1: API
- [x] 类型、默认值、校验；单元测试覆盖唯一性、枚举、Deployment 限制。

### Task 2: kuberuntime 状态机
- [x] init 顺序执行、失败按策略重试或终止；业务容器按策略重启（Always 一律、OnFailure 仅非 0、Never 不重启）。
- [x] phase：init 未完成 → Pending（Never 且 init 失败 → Failed）；有从未运行的 waiting 容器 → Pending；有运行中 → Running；
  全部停止时 Always → Running、全部成功 → Succeeded、Never → Failed、OnFailure → Running。
- [x] 条件 Initialized/ContainersReady/Ready 的原因与消息；PodScheduled 保留。
- [x] 终态 Pod 停止沙箱且不再变化；删除时并行停止容器。
- [x] 单元测试（假 CRI）：上表每一行。

### Task 3: exec
- [x] kuberuntime `Exec`、kubelet `/exec`、API 子资源（终态 Pod、未知容器、未调度 Pod 的错误）、客户端、`kk exec`。
- [x] 单元/集成测试：API 子资源错误路径与代理。

### Task 4: kk 与 kubelet 展示
- [x] `kk get pods` 的 STATUS（`Init:N/M`、`Init:<reason>`、`NotReady`、`Completed`、`Error`）、READY、RESTARTS；logs/exec 默认容器提示。
- [x] kubelet 为 init 容器记录事件。

### Task 5: e2e 与文档
- [x] 用例：多容器共享网络与按容器取日志；init 顺序执行；init 失败（Never/Always）；Never/OnFailure 终态；exec 输出与退出码、拒绝已结束 Pod；
  Deployment 拒绝非 Always；两个忽略 SIGTERM 的容器并行在宽限期内被杀。
- [x] README（中英）支持范围、限制、kk 命令；v2 规格状态；本计划勾选。

## 结果

全部任务完成：e2e 51 个 spec 通过（新增 9 个，约 260 秒），单元测试与 `-race` 通过。kubectl STATUS 列的
细节（容器倒序遍历、非 0 退出时 `Completed` 改为失败原因、终态 Pod 不显示 Terminating）按 Kubernetes
`printPod` 源码实现。新增示例 `examples/init-and-sidecar.yaml`。

代码审查后修复（均有单元测试，第一条另有 e2e）：沙箱丢失（节点重启）后，`restartPolicy: Never` 且已运行过容器的
Pod 不再重建沙箱，未结束的容器记为 `ContainerStatusUnknown`（退出码 137）、Pod 进入 Failed，OnFailure 下已成功的
容器不再重跑（同 kubelet `computePodActions`）；当前沙箱中已有业务容器时视为已初始化，不因运行时的瞬时错误回到
初始化状态；CrashLoopBackOff 消息写整个退避时长，退避期间状态不变，不再每秒写一次；kubelet 的 token 比较改为常数时间，
exec 超时上限 1 小时。
