# M6a ClusterIP/NodePort Service、Endpoints、kube-proxy 与 Ingress 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 取消“全集群只有一个 Service”的限制：Service 获得 ClusterIP（及 NodePort），Endpoints 控制器维护就绪地址，
每个节点上的 kube-proxy 用 iptables 实现转发；单 Service 网关被 Ingress 控制器取代。

**Architecture:** 与 Kubernetes 相同的分工：apiserver 在写入 Service 时从 `10.96.0.0/12` 与 `30000-32767`
分配 ClusterIP 与 NodePort（分配表存于 etcd，CAS 更新，repair 循环回收泄漏）；controller-manager 的 Endpoints 控制器
按选择器生成 Endpoints；kube-proxy（每个节点一个 static Pod，hostNetwork + privileged）watch Service 与 Endpoints，
用 `iptables-restore` 写入 `KUBE-SERVICES → KUBE-SVC-* → KUBE-SEP-*` 规则链；Ingress 控制器（control-plane 节点上的
static Pod，发布在 `127.0.0.1:18080`）按 host 与 path 把请求反向代理到 Endpoints。M6b 再做集群 DNS、Headless、
ExternalName 与 `kubernetes` Service。

**Tech Stack:** Go 1.26、iptables（nf_tables 后端，Ubuntu 24.04 包）、Ginkgo e2e。

## Global Constraints

- 只在 WSL（Ubuntu 24.04）中构建与测试；文档只出现 Linux 命令。
- 每个功能附带单元测试与 e2e；里程碑验收跑相关 e2e（全部里程碑完成后统一跑 full e2e）。
- 输出与报错文案以 AKS（v1.35）实测为准（2026-10-15）。

## 语义（AKS 实测）

| 项 | 规则 / 文案 |
| --- | --- |
| `get svc` | `NAME TYPE CLUSTER-IP EXTERNAL-IP PORT(S) AGE`，`-o wide` 加 `SELECTOR`；PORT(S) 形如 `80/TCP,81/UDP`、NodePort 为 `80:30875/TCP`；EXTERNAL-IP 为 `<none>` |
| `get endpoints` | `NAME ENDPOINTS AGE`；ENDPOINTS 为 `ip:port` 逗号连接，超过 3 个显示 `+ N more...`，无地址为 `<none>` |
| Endpoints 对象 | 标签 `endpoints.kubernetes.io/managed-by: endpoint-controller`；`subsets[].addresses[]` 含 `ip`、`nodeName`、`targetRef{kind: Pod, name, namespace, uid}`；端口 `name/port/protocol`（命名 targetPort 解析为容器端口）；未就绪 Pod 进 `notReadyAddresses` |
| ClusterIP 冲突 | `spec.clusterIP: Invalid value: "10.96.0.200": failed to allocate IP 10.96.0.200: provided IP is already allocated`（k8s 字段名为 `spec.clusterIPs`，mini 只有 `clusterIP`） |
| ClusterIP 越界 | `... failed to allocate IP 10.99.0.5: the provided network does not match the current range` |
| 修改 ClusterIP | `spec.clusterIP: Invalid value: "x": may not change once set` |
| NodePort 越界 | `spec.ports[0].nodePort: Invalid value: 40000: provided port is not in the valid range. The range of valid ports is 30000-32767` |
| NodePort 冲突 | `spec.ports[0].nodePort: Invalid value: 30875: provided port is already allocated` |
| ClusterIP 类型带 nodePort | ``spec.ports[0].nodePort: Forbidden: may not be used when `type` is 'ClusterIP'`` |
| 多端口未命名 | 每个端口 `spec.ports[i].name: Required value` |
| NodePort → ClusterIP | 未改动的 nodePort 自动清除并释放 |
| `get ingress` | `NAME CLASS HOSTS ADDRESS PORTS AGE`；CLASS 未设为 `<none>`，HOSTS 为各规则 host 逗号连接、全为空时 `*`，PORTS `80` |
| Ingress 校验 | `spec.rules[0].host: Invalid value: "Foo_bar": a lowercase RFC 1123 subdomain ...`；`spec.rules[0].http.paths[0].pathType: Required value: pathType must be specified`；`...path: Invalid value: "api": must be an absolute path`；`...backend: Invalid value: "": cannot set both port name & port number`；``spec: Invalid value: null: either `defaultBackend` or `rules` must be specified`` |

## 决策

| 决策 | 选择 |
| --- | --- |
| 分配表 | 与 k8s RangeAllocation 相同思路：etcd 键 `<prefix>/ranges/serviceips`、`/ranges/servicenodeports` 存已分配集合，CAS 更新；动态分配优先取“静态段”之外的随机值（ClusterIP 前 256 个、NodePort 前 86 个留给显式指定，如 DNS 的 `10.96.0.10`） |
| 分配时机 | apiserver 的 Strategy 新增 `BeginCreate`/`BeginUpdate`（返回 finish 回调，写失败时回滚）与 `AfterDelete`，与 `genericregistry.Store` 同名同义 |
| repair | apiserver 每分钟对账：分配表中连续 3 轮无人使用的值释放；被 Service 使用却未登记的值补登；M6 之前存储的、没有 ClusterIP 的 Service 补分配（迁移） |
| Service 字段 | `type`（ClusterIP/NodePort）、`clusterIP`、`ports[].nodePort`、`targetPort`（数字或端口名）、`sessionAffinity`（None/ClientIP，`sessionAffinityConfig.clientIP.timeoutSeconds` 默认 10800）；不支持 `clusterIPs`/双栈/`externalTrafficPolicy`/LoadBalancer |
| Endpoints vs EndpointSlice | 只实现 core/v1 Endpoints（kube-proxy、Ingress 都消费它）；没有选择器的 Service 由用户自行维护 Endpoints |
| 节点故障 | Endpoints 控制器不看节点状态（与 k8s 相同）；节点失联时把 Pod 标为未就绪属于 M7 的节点生命周期控制器 |
| kube-proxy 部署 | 每个节点一个 static Pod（`kube-proxy-<node>`），清单在 `build/manifests/{control-plane,node}/kube-proxy.yaml`（单元测试保证两份一致）；DaemonSet（M9）之后改为 DaemonSet |
| kube-proxy 镜像 | `mini-k8s-kube-proxy:local`：Ubuntu 24.04 + iptables + conntrack + `mini`，与节点同为 nf_tables 后端，共享 `/run/xtables.lock` |
| 特权 | 容器新增 `securityContext.privileged` 与 `capabilities.add/drop`，经 CRI 传给 containerd |
| iptables 规则 | 与 kube-proxy iptables 模式一致：链名 `KUBE-SVC-`/`KUBE-SEP-`/`KUBE-EXT-` + base32(sha256)[:16]；`statistic --mode random --probability 1/(n-i)` 均分；`recent` 模块实现 ClientIP 亲和；`KUBE-MARK-MASQ`（0x4000）+ `KUBE-POSTROUTING` MASQUERADE；集群外源地址访问 ClusterIP、NodePort 与 hairpin 做 SNAT；无端点的 Service 在 filter 表 REJECT |
| hairpin | mini-cni 给宿主侧 veth 开启 `hairpin`，Pod 能经自己的 Service 访问到自己 |
| NodePort 访问 | 与 kind 在 Linux 上相同：从宿主机直接访问节点容器 IP（`kk get nodes -o wide`）的 NodePort，不额外发布端口 |
| Ingress | networking.k8s.io/v1 Ingress（不实现 IngressClass；处理 `ingressClassName` 为空或 `mini` 的对象）；精确 host > 通配 host（`*.example.com` 只匹配一级）> 无 host；Exact 优先于最长 Prefix（按路径段匹配），ImplementationSpecific 按 Prefix；同 host+path 冲突时较早创建者胜；无匹配走 `defaultBackend`，否则 404；无就绪端点 503；状态写 `loadBalancer.ingress[{hostname: localhost}]`（同 kind） |
| 网关 | 删除 `pkg/gateway` 与 gateway static Pod；Ingress 控制器沿用其位置（control-plane 节点、hostNetwork 8081、发布 18080） |

## Tasks

### Task 1: API
- [x] Service 字段、默认值、校验；Endpoints 类型与校验；Container `securityContext`；networking/v1 Ingress 类型、默认值与校验；scheme 注册（`endpoints`/`ep`、`ingresses`/`ing`）、registry、客户端；单元测试。

### Task 2: 分配器与 apiserver
- [x] `pkg/registry/core/service/{allocator,ipallocator,portallocator}`；Service REST 钩子（创建/更新/删除时分配与释放、patchAllocatedValues、类型切换清理）；repair 循环；删除单 Service 准入；集成测试（并发分配不重复、释放、冲突与越界报错、迁移）。

### Task 3: Endpoints 控制器
- [x] `pkg/controller/endpoint`：Service/Pod 事件入队，按端口集合分组 subsets，命名端口解析，就绪/未就绪，删除 Service 时删除 Endpoints；集成测试。

### Task 4: 节点侧
- [x] kubelet 映射 `securityContext` 到 CRI；mini-cni 开启 hairpin；`pkg/proxy/iptables`（规则生成纯函数 + 同步循环）与 `mini kube-proxy`；镜像、static Pod 清单、Compose（token 挂载到所有节点、worker 清单目录）、`kk cluster up` 构建与导入镜像、所有节点上重启镜像过期的 static Pod；单元测试（规则文本快照、链清理、亲和）。

### Task 5: Ingress 控制器与 kk
- [x] `pkg/ingress`（路由表纯函数 + 反向代理 + 状态回写）与 `mini ingress-controller`，替换 gateway；`kk get svc/ep/ing` 列；`examples/demo.yaml` 增加 Ingress；单元测试。

### Task 6: e2e 与文档
- [x] e2e：ClusterIP 跨节点访问与负载均衡、端口映射与命名端口、多端口、hairpin、无端点立即拒绝、ClientIP 亲和、Endpoints 就绪变化、NodePort 从宿主机经每个节点访问、分配冲突/越界/不可变、类型切换释放端口、kube-proxy static Pod 与规则自愈；Ingress 路径/host/默认后端/404/503、滚动更新不中断、就绪门控；更新控制面 e2e（组件列表）。
- [x] README（中英）、v2 规格（§9、§12、§15）、本计划勾选。

## 结果

全部任务完成：单元测试、集成测试（`-race`）与相关 e2e（network、控制面、apimachinery、apps、生命周期，共 62 个 spec）通过。
实现与验证中的发现：

- 链名哈希与真实 kube-proxy 一致（单元测试用 kube-proxy 测试中的 `KUBE-SVC-XPGD46QRK7WJZT7O`、`KUBE-SEP-SXIVWICOYRO3J4NJ` 校验）。
- 从旧集群升级时，新的 ingress-controller 清单先于新镜像到达节点，会短暂 CrashLoop，镜像导入后自愈；`kk cluster up`
  现在对所有节点（不只 control-plane）重启镜像过期的 static Pod。
- README 架构图改为 Mermaid（GitHub 直接渲染，中英文不再有字符画对齐问题）。

代码审查后的 critical 修复：

- 同一 Service 中，先处理的端口动态分配到的 nodePort 可能正好是后面端口显式指定的值，导致重复——改为与 Kubernetes
  `initNodePorts` 相同的两遍分配（先显式、后动态），并加回归测试。
- UDP 后端被移除后旧的 conntrack 条目持续把流量 DNAT 到已消失的 Pod——kube-proxy 在每次同步后删除被移除端点的 UDP
  流，Service 从无端点变为有端点时删除被拒绝的流（同 kube-proxy）。

非 critical 的 repair 竞态记入 [backlog](../backlog.md)。
