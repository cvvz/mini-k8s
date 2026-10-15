# M7b 调度框架实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 调度器从“选 Pod 最少的节点”改为 kube-scheduler 式的插件框架：Filter 过滤（不可调度、污点、nodeSelector/节点亲和、
资源适配、Pod 亲和/反亲和），Score 打分（资源最少分配、同属主打散、偏好亲和、PreferNoSchedule），带退避的调度队列；
`kk label` 维护节点标签。

**Architecture:** `pkg/scheduler/framework` 定义插件接口与 NodeInfo 快照（节点及其 Pod、已请求资源，含“假定已绑定”的 Pod），
插件在 `pkg/scheduler/framework/plugins/<name>`（与 Kubernetes 同名）；调度一轮：快照 → 全部 Filter → 对可行节点打分加权
（权重同 kube-scheduler 默认配置）→ 取最高分（并列随机）→ 经 binding 子资源绑定。失败写 `PodScheduled=False` 与
`FailedScheduling` 事件，并按 1s 起翻倍、上限 10s 退避，集群事件（节点或 Pod 变化）提前重试。

**Tech Stack:** Go 1.26、Ginkgo e2e。

## Global Constraints

- 只在 WSL（Ubuntu 24.04）中构建与测试；文档只出现 Linux 命令。
- 每个功能附带单元测试与 e2e；里程碑验收跑相关 e2e。
- 失败信息以 AKS（v1.35）实测为准（2026-10-15）。

## 语义（AKS 实测与 Kubernetes 源码）

| 项 | 规则 / 文案 |
| --- | --- |
| 失败信息 | `0/3 nodes are available: 1 node(s) had untolerated taint {node-role.kubernetes.io/control-plane: }, 2 Insufficient cpu.`（原因按字母序；mini 不实现抢占，不带 `preemption:` 后缀） |
| NodeUnschedulable | `node(s) were unschedulable`；容忍 `node.kubernetes.io/unschedulable:NoSchedule` 的 Pod 可以调度 |
| NodeAffinity | nodeSelector 与 required nodeAffinity（`In`、`NotIn`、`Exists`、`DoesNotExist`、`Gt`、`Lt`，terms 之间 OR、expressions 之间 AND；`matchFields` 支持 `metadata.name`）：`node(s) didn't match Pod's node affinity/selector`；preferred 按权重打分 |
| NodeResourcesFit | 有效请求 = max(各容器之和, 最大 init 容器)；`Insufficient cpu`、`Insufficient memory`、`Too many pods`；打分 LeastAllocated（cpu、memory 平均，未设请求的容器按 100m/200Mi 计） |
| InterPodAffinity | required podAffinity/podAntiAffinity（`labelSelector`、`topologyKey`、`namespaces`，默认 Pod 自己的命名空间）：`node(s) didn't match pod affinity rules`、`node(s) didn't match pod anti-affinity rules`；已运行 Pod 的 required 反亲和同样阻止新 Pod；preferred 按权重打分 |
| 默认打散 | 同一 controller（ReplicaSet 等）的 Pod 越少得分越高（SelectorSpread/默认 PodTopologySpread 的效果） |
| TaintToleration 打分 | 不被容忍的 PreferNoSchedule 污点越少越好 |
| 权重 | TaintToleration 3、NodeAffinity 2、PodTopologySpread 2、InterPodAffinity 2、NodeResourcesFit 1（kube-scheduler 默认） |
| 节点注册 | kubelet 注册节点时带 `node.kubernetes.io/not-ready:NoSchedule`，Ready 后由节点生命周期控制器移除；调度器不再单独判断就绪（与 Kubernetes 相同） |
| kk label | `kk label nodes N k=v ...` → `node/N labeled`；`k-` → `node/N unlabeled`；已有不同值且无 `--overwrite`：`error: 'k' already has a value (v), and --overwrite is false` |

## 决策

| 决策 | 选择 |
| --- | --- |
| 抢占、优先级、DRA | 不实现（README 说明） |
| topologySpreadConstraints | 不实现显式约束；默认按属主打散 |
| 并发 | 单个调度循环；绑定同步进行，绑定成功前把 Pod 记为“假定已绑定”以便连续调度时资源与打散计算正确 |
| 并列 | 最高分并列时随机选择（同 kube-scheduler） |

## Tasks

### Task 1: API
- [x] PodSpec `nodeSelector`、`affinity`（nodeAffinity、podAffinity、podAntiAffinity）类型、校验；单元测试。

### Task 2: 框架与插件
- [x] `pkg/scheduler/framework`（NodeInfo、插件接口、调度一轮）、插件 NodeUnschedulable、TaintToleration、NodeAffinity、NodeResourcesFit、InterPodAffinity、PodTopologySpread（默认打散）；队列退避；kubelet 注册污点；单元测试（每个插件、失败信息、打分加权）与集成测试。

### Task 3: kk 与 e2e
- [x] `kk label`；e2e：nodeSelector 与必需/偏好节点亲和、资源不足与释放后调度、默认打散、Pod 反亲和、cordon 后待调度 Pod 在 uncordon 时被调度、失败信息格式。
- [x] README（中英）、v2 规格、本计划勾选。

## 结果

全部任务完成：单元测试（每个插件、失败信息、打分）、集成测试（`-race`）与相关 e2e（scheduling 5 个，apps/node/network 回归）通过。
e2e 中反亲和用例的问题在测试本身（`WaitForRunningPods` 要求的数量包含了待调度的 Pod），调度器无误。

代码审查后的 critical 修复：

- 退避时长 `1s << (尝试次数-1)` 没有上限，Pod 长期不可调度时会溢出为负数，`AddAfter` 立即重新入队，调度器空转——移位次数
  加上限，并加单元测试。
- 值为空的 nodeSelector（如 `node-role.kubernetes.io/control-plane: ""`）会匹配没有该标签的节点——与 Kubernetes 一样要求标签存在；
  顺带修复 `kk label` 无法添加空值标签。
