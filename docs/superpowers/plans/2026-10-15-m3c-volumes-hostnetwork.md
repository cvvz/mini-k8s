# M3c 卷（emptyDir、hostPath）与 hostNetwork 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Pod 可以声明 `emptyDir`（磁盘或内存）与 `hostPath` 卷并挂载到容器；`hostNetwork: true` 的 Pod 使用节点（节点容器）的网络命名空间。

**Architecture:** kubelet 在启动容器前为 Pod 准备卷目录（`/var/lib/kubelet/pods/<uid>/volumes/kubernetes.io~empty-dir/<name>`，内存介质挂 tmpfs；hostPath 按 type 检查或创建），经 CRI `Mount` 交给 containerd；Pod 删除时卸载并删除目录。hostNetwork 通过 CRI `NamespaceOption{Network: NODE}` 实现，Pod IP 即节点地址，不调用 CNI。

**Tech Stack:** Go 1.26、CRI v1、Ginkgo e2e。

## Global Constraints

- 只在 WSL（Ubuntu 24.04）中构建与测试；文档只出现 Linux 命令。
- 每个功能附带单元测试与 e2e；字段、默认值与错误信息与 Kubernetes 一致。
- 非 Linux 主机仍需能编译 `go vet ./...`（挂载相关代码按平台拆分）。

## 语义

| 项 | 规则 |
| --- | --- |
| emptyDir | Pod 首次在节点上运行时创建，容器重启与节点重启后保留，Pod 删除时删除；`medium: Memory` 为 tmpfs（`sizeLimit` 作为 tmpfs 大小） |
| hostPath | 节点（节点容器）上的路径；`type`：空（不检查）、`DirectoryOrCreate`、`Directory`、`FileOrCreate`、`File`；检查失败时容器不创建，报 `CreateContainerConfigError`，消息 `hostPath type check failed: <path> is not a directory` |
| volumeMounts | `name` 必须引用 Pod 的卷；`mountPath` 为绝对路径且同一容器内不重复；`readOnly` 只读挂载 |
| hostNetwork | 共享节点网络命名空间；`status.podIP` 等于 `hostIP`；不分配 Pod 网段地址；DNS 使用节点的解析配置；主机名为节点名 |

## 决策

| 决策 | 选择 | 理由 |
| --- | --- | --- |
| 卷目录 | `/var/lib/kubelet/pods/<uid>/volumes/kubernetes.io~empty-dir/<name>`（节点容器内，位于持久的 `/var` 卷） | 与 kubelet 路径一致，节点重启后保留 |
| 错误呈现 | 卷准备失败时所有待启动容器 waiting `CreateContainerConfigError` 并记录 `Failed` 事件 | 让 `kk get pods`/`describe` 直接看到原因 |
| 不做 | `subPath`、`sizeLimit` 的驱逐、hostPort、端口冲突调度 | 超出本里程碑；hostPort 与端口冲突检查在 M7 调度框架中考虑 |
| 测试辅助 | `mini write FILE TEXT` | scratch 镜像中没有 shell |

## Tasks

### Task 1: API
- [x] `Volume`、`EmptyDirVolumeSource`、`HostPathVolumeSource`、`VolumeMount`、`PodSpec.HostNetwork`；校验与单元测试。

### Task 2: kubelet
- [x] 卷准备（目录、tmpfs、hostPath 类型检查）、CRI `Mounts`、删除 Pod 时清理（含孤儿清理）；单元测试（临时目录、假挂载）。
- [x] hostNetwork：沙箱与容器的网络命名空间选项、Pod IP、DNS、主机名；单元测试检查 CRI 配置。

### Task 3: e2e 与文档
- [x] emptyDir：init 容器写入、业务容器读取；内存介质为 tmpfs；Pod 删除后目录被清理。
- [x] hostPath：写入后在节点上可见；只读挂载拒绝写入；`type: Directory` 指向不存在的路径时报错。
- [x] hostNetwork：Pod IP 等于节点地址，另一节点上的 Pod 能经节点地址访问它。
- [x] README（中英）、v2 规格、本计划勾选。

## 结果

全部任务完成：e2e 64 个 spec 通过（新增 4 个，约 325 秒），单元测试与 `-race` 通过。实现时一并把容器的
PID 命名空间改为每容器独立（`Pid: CONTAINER`，与 Kubernetes 默认一致；此前 CRI 未指定时 containerd 让容器
共享沙箱的 PID 命名空间）。示例 `examples/init-and-sidecar.yaml` 增加了两个容器共享的 emptyDir。

代码审查修复：housekeeping 原先先取“保留的 Pod”快照、再列出沙箱与目录，期间新建的 Pod 可能被当作孤儿删除
沙箱与 emptyDir（数据丢失）；现在先列出、后取快照，并把仍有 worker 的 Pod 计入保留集合（有单元测试）。
