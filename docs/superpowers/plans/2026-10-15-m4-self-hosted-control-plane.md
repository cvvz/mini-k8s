# M4 控制面自托管实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 像 kubeadm 一样，etcd、kube-apiserver、kube-controller-manager、kube-scheduler 以 static Pod 运行在 `control-plane` 节点上，在 `kube-system` 中以 mirror Pod 可见；control-plane 节点带污点，业务 Pod 不调度上去。

**Architecture:** Compose 只定义节点容器（`control-plane`、`worker-1`、`worker-2`）。control-plane 节点的 kubelet 读取清单目录（只读挂载仓库中的 `build/manifests/control-plane/`）运行 static Pod，不依赖 API；API 可用后为每个 static Pod 创建 mirror Pod 并把状态写到 mirror Pod 上。组件经 hostPath 挂载的 token 文件访问 API，清单中不含任何密钥。

**Tech Stack:** Go 1.26、CRI、Ginkgo e2e。

## Global Constraints

- 只在 WSL（Ubuntu 24.04）中构建与测试；文档只出现 Linux 命令。
- 每个功能附带单元测试与 e2e；行为以 kubeadm/kubelet 为准（static Pod 命名、mirror 注解、污点消息格式）。

## 设计（相对 v2 规格 §12.2 的落地选择）

| 项 | 选择 |
| --- | --- |
| 清单来源 | 仓库内 `build/manifests/control-plane/*.yaml`，只读挂载到 control-plane 节点的 `/etc/kubernetes/manifests`；不再由 `kk cluster up` 生成 |
| 凭据 | `.mini-k8s/token` 只读挂载到 control-plane 节点 `/etc/mini-k8s/token`，static Pod 经 hostPath 挂载并通过 `MINI_TOKEN_FILE` 读取；清单与 mirror Pod 中不出现 token |
| static Pod 命名与身份 | 名称 `<metadata.name>-<节点名>`，命名空间默认 `default`；UID 由节点名与清单内容的哈希得出，内容变化即视为新 Pod（先停旧的再起新的） |
| mirror Pod | 注解 `kubernetes.io/config.mirror`（=哈希）、`kubernetes.io/config.source: file`，ownerReference 指向 Node；kubelet 不把 mirror Pod 当作普通 Pod 运行；被删除时 kubelet 立即完成删除并重建，static Pod 不受影响 |
| 自举 | static Pod 在 API 可用前即运行；节点注册、informer、mirror Pod 失败后重试；控制面组件崩溃由 kubelet 重启 |
| 镜像拉取策略 | 新增 `imagePullPolicy`（Always、IfNotPresent、Never）；static Pod 使用 Never，镜像导入后下一轮同步即启动（`ErrImageNeverPull` 不退避） |
| 健康检查 | `/healthz`、`/livez`、`/readyz` 允许匿名访问（与 Kubernetes 默认一致），apiserver 与 etcd 的 static Pod 带 liveness 探针 |
| 网关 | 也作为 control-plane 节点上的 static Pod（hostNetwork，监听 8081，经节点容器发布到 `127.0.0.1:18080`），依靠该节点 kubelet 的 host-gw 路由访问 Pod；M6 由 kube-proxy/Ingress 取代 |
| 污点 | `Node.spec.taints`、`Pod.spec.tolerations`（Equal/Exists，NoSchedule/PreferNoSchedule/NoExecute）；调度器过滤不容忍 NoSchedule/NoExecute 的节点，失败消息如 `0/3 nodes are available: 1 node(s) had untolerated taint {node-role.kubernetes.io/control-plane: }, 2 node(s) were unschedulable.`；kubelet 以 `MINI_NODE_ROLE=control-plane` 注册时带污点与 `node-role.kubernetes.io/control-plane` 标签，worker 带 `node-role.kubernetes.io/worker` 标签；NoExecute 驱逐留到 M7 |
| etcd 数据 | hostPath `/var/lib/etcd`（control-plane 节点的持久 `/var` 卷）；旧版本的 `mini-k8s_etcd-data` 卷不迁移，升级时用 `kk cluster down --volumes` 重建 |
| kk | `cluster up` 等待三个节点 Ready；control-plane 节点额外导入 etcd 镜像；`cluster down` 先停 worker 再停 control-plane |

## Tasks

### Task 1: API 与调度
- [x] 污点/容忍类型、校验、匹配函数与单元测试；调度器过滤与失败消息；kk ROLES 列取自 `node-role.kubernetes.io/*` 标签。
- [x] `imagePullPolicy` 字段、默认值（`:latest` 或无 tag 为 Always，否则 IfNotPresent）与 kubelet 行为。
- [x] 健康检查端点匿名访问。

### Task 2: kubelet static Pod
- [x] 清单目录轮询、解析、默认值与校验、命名与 UID；static Pod 与 API Pod 合并的查找；清单删除或变化时停止旧 Pod。
- [x] mirror Pod 创建、状态写入、被删后重建；忽略 mirror Pod 的运行；日志/exec 能找到 static Pod。
- [x] 节点角色、污点与标签注册；`MINI_TOKEN_FILE`。
- [x] 单元测试：清单解析、UID 稳定性、mirror 生命周期（假 API）。

### Task 3: 部署与 kk
- [x] `build/manifests/control-plane/{etcd,kube-apiserver,kube-controller-manager,kube-scheduler,gateway}.yaml`。
- [x] compose：只有节点容器；control-plane 发布 18000/18080；workers 经 `http://control-plane:8080` 访问 API。
- [x] kk：镜像导入按节点区分、等待三个节点、`cluster down` 顺序；单元测试更新。

### Task 4: e2e 与文档
- [x] 框架：节点列表含 control-plane；调试输出改为节点日志与 kube-system Pod 日志。
- [x] 用例：kube-system 中四个控制面组件与网关 Running 且为 mirror Pod；`kk logs` 读取控制面日志；删除 mirror Pod 后重建且组件不重启；杀死 apiserver 容器后被 kubelet 拉起；业务 Pod 不调度到 control-plane、带容忍的 Pod 可以；重启 control-plane 节点后状态完整。
- [x] README（中英）：架构图、组件表、Quick Start 输出、排障；v2 规格；本计划勾选。

## 结果

全部任务完成：e2e 68 个 spec 通过（新增 4 个控制面用例，约 375 秒），单元测试与 `-race` 通过；
`kk cluster up` 约 25 秒完成（镜像已构建时）。实现中发现的问题与差异记录在 v2 规格 §12.3。

代码审查修复：static Pod 停止后删除 mirror Pod 失败（例如停掉的正是 API Server）时不再卡住，记录照样移除，
同一清单放回后能重新运行，孤儿 mirror 由 housekeeping 清理；同名清单只运行第一个并报错；清单目录读取失败时
本轮不改动。新增“移走清单再放回”的单测与 e2e（69 个 spec 全部通过）。
