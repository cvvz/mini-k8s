# M2 kind 式节点 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 节点从“共享宿主 Docker 的逻辑节点”换成 kind 式节点：每个节点是一个 privileged
容器，内含 containerd 与 kubelet；kubelet 经 CRI 管理 Pod；Pod 网络由自研 `mini-cni`、
node IPAM 控制器和 host-gw 路由代理提供。保持 M1 全部行为（35 个 e2e 用例）不退化。

**Architecture:** 设计见 [v2 规格 §14](../specs/2026-10-14-mini-k8s-v2-design.md) 与
[可行性验证报告](../spikes/2026-10-15-kind-node-spike.md)。`mini` 多合一二进制新增
`node-init`、`pause`、`fetch` 三个入口，并在以 `mini-cni` 名字被调用时充当 CNI 插件。

**Tech Stack:** Go 1.26、containerd 2.x + runc（Ubuntu 24.04 软件包）、CRI v1（`k8s.io/cri-api`
+ `google.golang.org/grpc`）、iproute2/iptables/nsenter、Docker Engine + Compose（运行节点容器）。

## Global Constraints

- 测试只在 WSL（Linux）执行；文档只写 Linux 命令。
- 新依赖仅限 `k8s.io/cri-api`、`google.golang.org/grpc` 及其传递依赖。
- 每个新行为都有单元测试，并有 e2e 用例覆盖。
- kubelet 只管理本节点 containerd 中带 Pod 元数据的沙箱和容器。

## 关键决策

| 议题 | 决定 | 理由 |
| --- | --- | --- |
| 节点入口 | `mini node-init`（Go）：cgroup v2 委派、`mount --make-rshared /`、`ip_forward`，生成 containerd 配置并以子进程运行 containerd，socket 就绪后在进程内运行 kubelet | 可测试、无 shell 脚本；任一部分退出则整个节点退出，由 Compose 重启 |
| CNI 插件形态 | `/opt/cni/bin/mini-cni` 是指向 `/usr/local/bin/mini` 的符号链接，`mini` 按 argv[0] 进入插件模式（busybox 式） | 一个二进制、一个镜像层 |
| 网络操作 | 调用节点镜像中的 `ip`、`nsenter`、`iptables`，不引入 netlink 库 | 依赖最少；命令即文档，易读易排障 |
| IPAM 状态 | `/run/mini-cni/ipam/<ip>` 文件 + flock，节点重启即清空 | 节点重启后所有沙箱都会重建，持久化分配只会泄漏地址 |
| loopback | `mini-cni` 自己把 Pod 的 `lo` 拉起 | 无需参考插件 |
| pause 镜像 | `mini-k8s-pause:local`（`FROM scratch`，`/mini pause`），在 containerd 配置的 `pinned_images.sandbox` 中指定 | 不依赖 registry.k8s.io |
| 重启语义 | 容器退出后按退避创建 `attempt+1` 的新容器，保留上一个（`logs --previous`），删除更早的；restartCount 即 attempt | 与 kubelet 一致；CRI 不支持重启已退出的容器 |
| 沙箱失效 | 最新沙箱非 READY（如节点重启）时停止并删除，再以 `attempt+1` 新建 | 与 kubelet 一致 |
| 日志 | 由 containerd 写入 `/var/log/pods/{ns}_{pod}_{uid}/{container}/{attempt}.log`（CRI 格式），kubelet 读文件实现 tail/follow/previous | CRI 标准行为 |
| 节点就绪 | Ready 需同时满足：CRI RuntimeReady、NetworkReady（CNI 配置已写入）、pause 镜像存在；否则 Ready=False 并给出原因 | 镜像导入或 podCIDR 分配之前不调度 Pod |
| Pod DNS | M2 不配置集群 DNS；若节点 `/etc/resolv.conf` 指向 Docker 内置 DNS（Pod 中不可达），改用其 `ExtServers` 上游 | 集群 DNS 在 M6 |
| 网关 | M4 之前仍是 Compose 容器，但改用节点镜像并运行同一套 host-gw 路由代理（`NET_ADMIN`） | 需要到 Pod 网段的路由 |
| 镜像构建 | `build/images/{mini,etcd,node,pause}/Dockerfile`，仓库根目录为构建上下文，`.dockerignore` 只放行所需文件 | 用户确认；缩小构建上下文 |
| 镜像导入 | `cluster up` 把 `mini-k8s:local` 与 pause 镜像导入每个节点；`kk image load IMAGE...` 导入用户镜像 | 同 `kind load docker-image` |
| 数据保留 | 每个节点 `/var` 是独立 named volume（containerd 数据、镜像、日志），`cluster down` 保留；`cluster down --volumes` 连同 etcd 一起删除 | 重启快；提供明确的重置手段 |

## 文件结构

| 路径 | 职责 |
| --- | --- |
| `build/images/{mini,etcd,node,pause}/Dockerfile`、`.dockerignore` | 四个本地镜像 |
| `cmd/mini/{nodeinit,pause,fetch,cni}.go` | 新入口 |
| `pkg/nodeinit` | cgroup 委派、containerd 配置生成与进程管理 |
| `pkg/network/cni` | `mini-cni` 插件（ADD/DEL/CHECK/VERSION）与 IPAM |
| `pkg/network/hostgw` | host-gw 路由与 MASQUERADE 代理 |
| `pkg/controller/nodeipam` | 分配 `Node.spec.podCIDR` |
| `pkg/kubelet/kuberuntime` | CRI 运行时管理：沙箱、容器、退避、状态、日志、清理 |
| `pkg/kubelet`（修改） | 运行时接口、网络就绪、CNI 配置、路由代理、Pod DNS |
| `pkg/kubelet/dockershim` | 删除 |
| `cmd/kk`（修改） | 构建与导入镜像、`image load`、`logs -p`、`cluster down --volumes` |
| `compose.yaml`（修改） | 节点容器、网关容器 |
| `test/e2e/**` | 容器检查改为经节点内 CRI；新增节点网络、节点故障、镜像导入、previous 日志用例 |

## Tasks

### Task 1: 镜像与入口
- [x] `build/images/*`、`.dockerignore`；Makefile/`kk cluster up` 改用新路径。
- [x] `mini pause`（等待信号退出）、`mini fetch URL`（测试用 HTTP 客户端）、`mini node-init`。
- [x] 单元测试：containerd 配置生成、cgroup 控制器字符串转换。

### Task 2: Node podCIDR 与 node IPAM
- [x] `corev1.NodeSpec.PodCIDR`；节点策略：设置后不可修改。
- [x] `nodeipam` 控制器：为无网段的节点分配未使用的 `/24`，冲突重试。
- [x] 单元测试：分配算法；多节点网段互不重叠由 e2e 覆盖（见规格 §14.5）。

### Task 3: mini-cni 与 host-gw
- [x] 插件：读取 stdin 配置与 CNI_* 环境变量，创建网桥/veth、分配地址、设置路由，输出 CNI 1.0 结果；DEL 幂等。
- [x] IPAM：文件锁、首个空闲地址、按容器 ID 回收。
- [x] host-gw：按 Node 列表计算期望路由，与 `ip -j route` 对比后增删；确保 MASQUERADE 规则。
- [x] 单元测试：IPAM、配置解析、结果格式、路由差异计算（假命令执行器）。

### Task 4: kuberuntime（CRI）
- [x] gRPC 连接 containerd；RunPodSandbox/CreateContainer/StartContainer/StopContainer/
  StopPodSandbox/RemovePodSandbox/ListPodSandbox/ListContainers/ContainerStatus/PodSandboxStatus/
  ImageStatus/PullImage/Status/Version。
- [x] SyncPod 状态计算沿用 M1 语义（Pending/Running、waiting 原因、lastState、Ready 条件）。
- [x] 日志：CRI 日志格式解析、tail、follow、previous。
- [x] 单元测试：假 CRI 客户端覆盖创建、重启退避、拉取失败、沙箱重建、清理、日志解析。

### Task 5: kubelet 集成与集群生命周期
- [x] kubelet 改用运行时接口；Ready 条件；CNI 配置写入；路由代理；Pod DNS。
- [x] compose 与 `kk`：节点容器、网关容器、镜像导入、`image load`、`logs -p`、`down --volumes`。
- [x] 删除 dockershim。

### Task 6: e2e 与文档
- [x] 框架：`PodContainers` 经 `docker exec <节点> crictl` 查询；`ExecInPod`。
- [x] 新用例：节点网段分配、跨节点 Pod 互通且保留源 IP、Pod 访问集群外地址、节点容器停止与恢复、
  `kk image load`、`kk logs --previous`。
- [x] README（中英）、v2 规格状态与 §14 落地记录、本计划勾选。

## 结果

全部任务完成：e2e 42 个 spec 通过（约 210 秒），单元测试与 `-race` 通过。实现中的偏差与问题
（`loopback` 插件、沙箱创建退避、CRI 删除镜像的语义）记录在 v2 规格 §14.5。
