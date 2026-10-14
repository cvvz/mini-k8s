# M1 API 机制重构 Implementation Plan

> **状态：** 已完成并合入 main（2026-10-15）。
>
> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 用 K8s 风格的 API 机制替换 v1 的 `/api/{Kind}`：命名空间、通用元数据、Watch、
Informer/WorkQueue、ownerReferences 级联删除、finalizer、Pod 优雅删除、Events，并把全部
组件、`kk` 和 e2e 迁移过去，保持 v1 已有的端到端能力不退化。

**Architecture:** 分层为 `api`（类型、资源注册表、选择器）→ `storage`（etcd JSON gateway +
watch）→ `apiserver`（路由、策略、准入、删除语义、watch 推送、子资源）→ `client`（REST、
泛型资源客户端、watch）→ `informer`/`workqueue`/`record` → 各组件。组件仍是同一个
`mini` 二进制的不同角色。

**Tech Stack:** Go 1.25、etcd v3.5.17 JSON gateway、Docker Engine API v1.41、
Ginkgo v2 + Gomega（e2e）、gopkg.in/yaml.v3。

## Global Constraints

- 所有构建与测试在 Linux（WSL2）执行；文档只写 Linux 命令。
- 不引入 gRPC/client-go 等 Kubernetes 依赖；新依赖仅限 x/net、x/term 一类小库。
- 用户输入严格解码：未知字段返回 `Invalid`；不支持的操作明确报错。
- 只清理本集群拥有的容器；不执行全局 prune。
- 每个新行为都要有单元测试或组件测试，并附带 e2e spec。

## 文件结构

| 路径 | 职责 |
| --- | --- |
| `internal/api/meta.go` | TypeMeta、ObjectMeta、ListMeta、OwnerReference、Time、Status、DeleteOptions、Object 接口 |
| `internal/api/core.go` | Namespace、Node、Pod、Service、Event 类型 |
| `internal/api/apps.go` | Deployment 类型 |
| `internal/api/registry.go` | ResourceInfo 注册表、路径构造、名称/别名解析 |
| `internal/api/labels.go` | label selector（字符串解析与 LabelSelector 转换）、field selector |
| `internal/storage/etcd.go` | KV CRUD、CAS、lease、watch 流 |
| `internal/apiserver/server.go` | 路由与请求解析、认证、错误响应 |
| `internal/apiserver/handlers.go` | get/list/create/update/patch/delete |
| `internal/apiserver/watch.go` | watch 推送与 selector 过滤 |
| `internal/apiserver/strategy.go` | 各资源默认值、校验、可变性、字段选择器 |
| `internal/apiserver/subresources.go` | status、binding、log 代理 |
| `internal/client/client.go` | REST 客户端、API 错误判定 |
| `internal/client/typed.go` | 泛型资源客户端、Watcher |
| `internal/informer/informer.go` | list-watch、缓存、事件分发、resync |
| `internal/workqueue/queue.go` | 去重队列、延迟、指数退避 |
| `internal/record/recorder.go` | Event 记录与聚合 |
| `internal/controller/*.go` | deployment、gc、namespace 控制器 |
| `internal/scheduler/scheduler.go` | 事件驱动调度（M5 再插件化） |
| `internal/kubelet/*.go` | 节点注册/心跳、Pod 同步、Docker 运行时、日志服务、优雅终止 |
| `internal/proxy/gateway.go` | HTTP 网关（M4 前保持单 Service 语义） |
| `cmd/mini/main.go` | 角色入口 |
| `cmd/kk/*.go` | 新 CLI：-n/-A/-l/-o/-w、describe、events、namespace |
| `test/e2e/**` | 框架改为每个 spec 独立命名空间；新增 api-machinery/node spec |

## Tasks

### Task 1: api 包
- [x] 类型与 JSON 形状对齐 Kubernetes；`Object` 接口由内嵌 `*ObjectMeta`/`*TypeMeta` 方法提供。
- [x] 注册表：`Pods`、`Deployments` 等 `ResourceInfo`，`Lookup("po")`、`ForKind("Pod")`、`Path(ns,name,sub)`。
- [x] selector：`ParseSelector("a=b,c!=d,e in (x,y),!f")`、`LabelSelector.AsSelector()`；字段选择器仅 `=`/`==`/`!=`。
- [x] 单元测试：解析/匹配表驱动测试；JSON 往返测试。

### Task 2: storage 包
- [x] `Get/List/Create/Update/Delete`，`Create` 比较 `create_revision=0`，更新/删除比较 `mod_revision`。
- [x] `GrantLease(ttl)`、`CreateWithLease`。
- [x] `Watch(ctx, prefix, rev)`：解析 gateway 流，PUT/DELETE + prev_kv，压缩时返回 `ErrCompacted`。
- [x] 组件测试：启动真实 `bin/etcd`（测试辅助 `storage/etcdtest`，缺失时 Skip）。

### Task 3: apiserver
- [x] 路由：`/api/v1`、`/apis/{g}/{v}`、命名空间/集群资源、子资源；`/healthz`、`/version`、`/apis` 发现。
- [x] 创建：严格解码 → 命名空间准入 → generateName → 默认值 → 校验 → 写入；`dryRun=All`。
- [x] 更新：资源版本前置条件；主资源保留 status，status 子资源保留 spec；spec 变化递增 generation。
- [x] PATCH：JSON merge patch，支持 `metadata.resourceVersion` 前置条件。
- [x] 删除：preconditions、propagationPolicy（Orphan/Foreground finalizer）、Pod 优雅期、finalizer 未清空时只置 deletionTimestamp。
- [x] Watch：无 RV 时先发送现有对象的 ADDED；selector 过滤与“离开匹配集合”转 DELETED；超时关闭。
- [x] 子资源：`pods/binding`、`pods/log`（代理 kubelet）、`*/status`。
- [x] 启动时创建 `default`、`kube-system` 命名空间；Event 使用 1 小时 etcd lease。
- [x] 组件测试：真实 etcd + httptest，覆盖 CRUD、冲突、watch、删除语义、selector、命名空间准入。

### Task 4: client / informer / workqueue / record
- [x] 泛型客户端与 Watcher；`IsNotFound/IsConflict/IsAlreadyExists/IsInvalid`。
- [x] Informer：list → watch → 410 重新 list；`Lister.List(selector)`、`Get(ns,name)`；handler 分发；resync。
- [x] WorkQueue：client-go 语义（dirty/processing、AddAfter、AddRateLimited、Forget）。
- [x] Recorder：同（对象、reason、message）聚合 count。
- [x] 单元测试：队列语义、informer 对 watch 事件的缓存与回调（基于 apiserver 组件测试环境）。

### Task 5: 控制器与调度器
- [x] Deployment 控制器：ownerReferences(controller=true)、generateName、补建/缩容（优先删除未调度/未就绪 Pod）、status 与 observedGeneration。
- [x] GC 控制器：后台删除无 owner 的依赖对象；处理 `orphan` 与 `foregroundDeletion` finalizer。
- [x] Namespace 控制器：清空 Terminating 命名空间后移除 `kubernetes` finalizer。
- [x] Scheduler：informer + 队列，按 Pod 数最少的 Ready 节点绑定（binding 子资源），失败写 PodScheduled 条件与事件。

### Task 6: kubelet 与网关
- [x] 节点注册（地址、标签、nodeInfo）、Ready 条件心跳。
- [x] 只 watch 本节点 Pod；单容器 Docker 运行时迁移到新类型；containerStatuses；只在变化时写 status。
- [x] 优雅终止：deletionTimestamp → `docker stop -t grace` → 删除容器 → `DELETE gracePeriodSeconds=0`（UID 前置条件）。
- [x] kubelet 服务 `:10250/containerLogs/{ns}/{pod}/{container}`（follow、tailLines）。
- [x] 网关迁移到 informer；Service 仍全局唯一（准入保证），M4 替换。

### Task 7: kk
- [x] 资源别名、`-n/-A/-l/-o wide|json|yaml|name/-w`；表格列对齐 kubectl。
- [x] `apply/create/delete -f`（文件、目录、`-`），apply 先整体 dry-run 再写入；`delete --grace-period/--cascade`。
- [x] `describe`（对象 + 相关 Events）、`logs -f --tail`、`scale`、`create namespace`、`api-resources`、`version`。
- [x] 单元测试：参数解析、manifest 解析、表格输出。

### Task 8: e2e 与文档
- [x] 框架：每个 spec 创建 `e2e-<base>-xxxx` 命名空间，结束删除并等待清空；客户端换成新 client。
- [x] 迁移已有 22 个 spec；新增：watch 事件、selector、命名空间隔离与删除、generateName、finalizer、
  GC 三种传播策略、优雅终止（SIGTERM 日志 + Terminating 状态）、强制删除、Events、Deployment status。
- [x] README（中英）、v2 规格状态、本计划勾选；WSL 中 `make vet test e2e` 全绿后合入。
