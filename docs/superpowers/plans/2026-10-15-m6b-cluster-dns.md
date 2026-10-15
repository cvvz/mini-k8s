# M6b 集群 DNS、Headless/ExternalName Service 与 `kubernetes` Service 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Pod 能按名字发现 Service：自研集群 DNS（`kube-dns`，10.96.0.10）提供 `*.svc.cluster.local`、headless、SRV、
ExternalName 与 Pod 记录；kubelet 按 `dnsPolicy` 配置 Pod 的 resolv.conf、生成 `/etc/hosts`、注入 Service 环境变量；
apiserver 维护 `default/kubernetes` Service，Pod 内的组件可经它访问 API。

**Architecture:** 与 kubeadm 集群相同：DNS 服务器（`mini cluster-dns`，作用同 CoreDNS 的 kubernetes 插件）watch Service、Endpoints
与 Pod，在 kube-system 中以 Deployment + Service `kube-dns` 运行，由 `kk cluster up` 在节点就绪后像 kubeadm 的 addon
阶段一样 apply；集群外名字转发给上游（DNS Pod 的 `dnsPolicy: Default`，即节点的转发器）。

**Tech Stack:** Go 1.26（自写 DNS 报文编解码，不引入 DNS 库）、Ginkgo e2e。

## Global Constraints

- 只在 WSL（Ubuntu 24.04）中构建与测试；文档只出现 Linux 命令。
- 每个功能附带单元测试与 e2e；里程碑验收跑相关 e2e。
- 行为以 AKS（v1.35，CoreDNS）实测为准（2026-10-15）。

## 语义（AKS 实测）

| 项 | 规则 |
| --- | --- |
| resolv.conf（ClusterFirst） | `search <ns>.svc.cluster.local svc.cluster.local cluster.local <节点搜索域>`、`nameserver <DNS ClusterIP>`、`options ndots:5` |
| /etc/hosts | 首行 `# Kubernetes-managed hosts file.`，localhost 与 IPv6 条目，末行 `<PodIP>\t<hostname>[.<subdomain>.<ns>.svc.cluster.local]\t<hostname>`；`hostname -f` 得到 FQDN |
| A 记录 | `<svc>.<ns>.svc.cluster.local` → ClusterIP；headless → 每个就绪端点 IP；`<hostname>.<svc>.<ns>.svc.cluster.local` → 带 hostname 的端点；`<a-b-c-d>.<ns>.pod.cluster.local` → 该 IP |
| SRV | `_<port>._<proto>.<svc>.<ns>.svc.cluster.local` → `0 100 <port> <svc>.<ns>.svc.cluster.local`；headless 的目标为各端点 `<hostname 或 a-b-c-d>.<svc>.<ns>.svc.cluster.local` |
| ExternalName | CNAME 到 `externalName`（递归解析由上游完成） |
| 不存在 / 其他类型 | cluster.local 下不存在的名字 NXDOMAIN；存在但类型不符（如 AAAA）为 NOERROR 空应答 |
| 环境变量 | 同命名空间每个有 ClusterIP 的 Service：`WEB_SERVICE_HOST`、`WEB_SERVICE_PORT`（第一个端口）、`WEB_SERVICE_PORT_<NAME>`、`WEB_PORT=tcp://ip:port`、`WEB_PORT_<p>_<PROTO>`、`..._PROTO`、`..._PORT`、`..._ADDR`；另有 `KUBERNETES_*`（`KUBERNETES_SERVICE_PORT_HTTPS=443`） |
| Endpoints | headless 端点在 Pod `subdomain` 等于 Service 名时带 `hostname` |
| kubernetes Service | `default/kubernetes`，ClusterIP 为 Service 网段第一个地址，`443/TCP`（端口名 https），Endpoints 为 apiserver 地址 |

## 决策

| 决策 | 选择 |
| --- | --- |
| DNS 实现 | `pkg/dns`：报文编解码（无压缩写出，读取支持压缩指针）、记录生成（纯函数）、UDP/TCP 服务；TTL 30 秒；UDP 应答超过 512 字节（或 EDNS 声明的大小）时置 TC 位，客户端改用 TCP；转发上游时原样转发报文 |
| 部署 | Deployment `kube-dns`（2 副本，镜像 `mini-k8s:local`，`dnsPolicy: Default`，就绪探针 `/ready`）与 Service `kube-dns`（10.96.0.10，`dns` 53/UDP、`dns-tcp` 53/TCP），清单在 `build/manifests/addons/`；API 凭据经 hostPath 挂载节点上的 token（mini 没有 ServiceAccount） |
| DNS Pod 访问 API | 经 `kubernetes` Service（in-cluster 配置：未设 `MINI_API` 时使用 `KUBERNETES_SERVICE_HOST/PORT`，协议 http） |
| kubernetes Service | apiserver 启动后每 10 秒确保 Service（10.96.0.1，port 443 name https，targetPort 为 apiserver 端口）与 Endpoints（apiserver 地址：`MINI_ADVERTISE_ADDRESS`，否则取 eth0 地址）；mini 没有 TLS，443 端口上是明文 HTTP |
| kubelet DNS | `dnsPolicy` ClusterFirst（默认）、ClusterFirstWithHostNet、Default、None 与 `dnsConfig`（nameservers/searches/options 合并），hostNetwork 的 ClusterFirst 按 Default；集群 DNS 地址与域名来自 `MINI_CLUSTER_DNS`（10.96.0.10）与 `MINI_CLUSTER_DOMAIN`（cluster.local） |
| /etc/hosts | 非 hostNetwork Pod 由 kubelet 在 Pod 目录生成并挂载到每个容器（与 kubelet 相同） |
| Service 字段 | `clusterIP: None`（headless，不分配、kube-proxy 忽略）、`type: ExternalName` + `externalName`（无 ClusterIP、端口可省略）、`publishNotReadyAddresses`；类型在 ExternalName 与其他类型之间切换时分配或释放 ClusterIP |
| Pod 字段 | `hostname`、`subdomain`、`dnsPolicy`、`dnsConfig`、`enableServiceLinks`（默认 true） |
| e2e 工具 | `mini lookup [-t A|SRV|CNAME] NAME`：Go 纯解析器，遵循 Pod 的 resolv.conf（search、ndots），输出排序后的结果 |

## Tasks

### Task 1: API 与控制器
- [x] Service/Pod 新字段的默认值、校验、分配与类型切换；Endpoints 控制器的 hostname 与 publishNotReadyAddresses；kube-proxy 与 kk 对 headless/ExternalName 的处理；单元与集成测试。

### Task 2: DNS 服务器
- [x] `pkg/dns` 报文、记录与服务；`mini cluster-dns` 组件与 `mini lookup` 工具；单元测试（编解码、各类记录、NXDOMAIN/NODATA、截断、转发）。

### Task 3: kubelet 与 apiserver
- [x] Pod DNS 配置、/etc/hosts、hostname、Service 环境变量；`kubernetes` Service 与 Endpoints 维护；组件 in-cluster 配置；单元测试。

### Task 4: 部署与 e2e
- [x] addon 清单与 `kk cluster up` 的 addon 阶段；e2e：Service 与 headless/hostname/SRV/ExternalName/Pod 记录解析、短名搜索、NXDOMAIN、外部名字、resolv.conf 与 dnsPolicy、/etc/hosts、环境变量、Pod 内经 `kubernetes.default` 访问 API、删除一个 DNS Pod 后仍可解析。
- [x] README（中英）、v2 规格、本计划勾选。

## 结果

全部任务完成：单元测试、集成测试（`-race`）与相关 e2e（network 27 个含 5 个 DNS 用例，node/生命周期/apimachinery 53 个，
修复后 DNS 与 apps 18 个）通过。实现中确认：DNS Pod 经 `kubernetes` Service 访问 API，就绪探针要求 informer 同步，
因此 `kube-dns` 就绪即证明 ClusterIP、kube-proxy 与 in-cluster 配置都工作。

代码审查后的 critical 修复：

- 恶意 UDP 包（一段很长的标签链加数千个指向它的问题）能让 DNS 服务器分配数百 MB 内存——名字限制为 255 字节、标签 63 字节，
  只接受恰好一个问题的查询（其余直接 FORMERR，不回显问题），并限制同时处理的 UDP 查询数；附回归测试。
- 新增 Pod 默认字段（`dnsPolicy`、`enableServiceLinks`）会改变模板哈希，升级后所有 Deployment 都会被滚动一次——与 Kubernetes
  的 `FindNewReplicaSet`/`EqualIgnoreHash` 相同，按“补齐默认值、忽略哈希标签后的模板内容”认定新 ReplicaSet；附集成测试。
