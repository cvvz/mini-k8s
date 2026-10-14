# mini-k8s Implementation Plan

> **For agentic workers:** 若在后续任务中重建或扩展本计划，使用
> `superpowers:executing-plans` 逐任务实施；存在边界明确的独立任务时可使用
> `superpowers:subagent-driven-development`。本文件是事后补全，不是现在重新执行、
> 创建提交或修改运行中集群的授权。

**Goal:** 用 Go 自研精简控制面、Docker kubelet、HTTP 代理与 `kk`，在本机形成真实
容器的声明式部署、扩缩容、自愈和持久化恢复闭环。

**Architecture:** API Server 独占 etcd 访问，其余组件通过 API 轮询协调。
两个逻辑 worker 共享 Docker Engine，以 UID 和所有权标签管理业务容器。
Compose 承载组件，宿主机 `kk` 提供用户操作。

**Tech Stack:** Go 1.23+、标准库 HTTP/JSON、`gopkg.in/yaml.v3`、etcd v3.5.17、
Docker API v1.41、Docker Compose v2、make/curl/tar。

**Spec:** [mini-k8s 精简版设计规格](../specs/2026-10-14-mini-k8s-design.md)。

## 文档性质与执行状态

本计划还原已确认规格的实施拆分，并以现存代码和已运行测试记录完成状态。
勾选表示产物与验证结果已存在，不伪造“本文在编码前写好”或“每一步均按本文顺序
执行”的历史。

从零重建时，每个任务应按“先写以下测试契约 → 确认因功能缺失而失败 →
实现 → 运行该任务验证”的顺序执行。本文补写本身不重演 RED，也不需要重跑
会修改集群的 E2E。原工作目录并非 Git 仓库，没有提交记录可供引用。

初始预算约为 20 分钟编码、10 分钟联调；并行开发后先完成代码与二进制，
Docker 引擎问题和端到端验证在用户允许继续后处理，不能记为 30 分钟内全部完成。

## Global Constraints

- 范围以 spec 的最终精简版为准，不把早期完整功能设想悄悄加入实现。
- Go 1.23+；除 YAML 解析外优先使用标准库，Docker 不引入 SDK 依赖。
- etcd v3.5.17；worker Docker API v1.41；两个固定逻辑 worker。
- 仅本地 Unix Docker endpoint；拒绝 TCP/SSH/远程 endpoint。
- 仅 default namespace、单容器 Pod、0–20 Deployment 副本、一个 HTTP Service。
- 仅 Deployment replicas 可原地更新；其他 spec/labels 不可变。
- API 为 `127.0.0.1:18000`，Service 网关为 `127.0.0.1:18080`。
- 真正创建与检查容器，不模拟 Pod Running。
- 错误显式返回；并发更新使用版本前置条件。
- 只操作本项目拥有的 Docker 资源，不执行全局 prune、不删除 etcd 数据卷。
- 不擅自重启 Docker 或更改用户配置；涉及共享环境先获得授权。
- 文档使用通用路径和环境要求，不依赖某位开发者机器上的缓存或安装目录。

## Review Focus

| 高风险输入/条件 | 应有行为 | 对应任务与证据 |
| --- | --- | --- |
| 多文档中有非法字段、不可变更新 | 校验失败；非法批次不先写入前面文档 | T1/T5：`TestValidation`、`TestOnlyReplicaUpdatesAllowed`、`TestInvalidManifestBatchMakesNoAPIRequests` |
| 容器名称/标签相似但所有者不同 | 不接管、不读取日志、不删除 | T4：`TestDockerCleanupOwnership`、`TestDockerLogs` |
| 节点心跳过期、API 可达但 worker 未就绪 | 不调度到过期节点，不把 cluster up 标成成功 | T2/T6：`TestSchedulerIgnoresStaleNodesAndBalancesAssignedPods`、`TestReadinessRequiresAuthenticatedHealthAndBothFreshWorkers` |
| 更新/删除与协调并发发生 | 保留身份，拒绝旧版本，不强行覆盖 | T3/T5/T7：CLI 冲突测试和 `TestLiveCluster` |
| 停止失败、重启、镜像缓存缺失 | 不错误清理业务；恢复期望状态；确实准备镜像 | T4/T6/T7：停止、镜像准备测试与真实 down/up |

这里列的是容易遗漏的审查重点，不代表所有故障都已有独立单元测试。
特别是 etcd CRUD/CAS 与组件组合依赖真实 E2E 证据，不把少量 API 单测写成完整存储测试套件。

## 文件与责任

| 文件/目录 | 责任 |
| --- | --- |
| [model.go](../../../internal/mini/model.go) | 资源类型、严格校验、不可变更新规则、调度选择与 ownership 辅助函数 |
| [store.go](../../../internal/mini/store.go) | etcd HTTP gateway、序列化、revision 与条件写入 |
| [api.go](../../../internal/mini/api.go) | 认证 HTTP API、状态子接口、日志转发、共享 Client |
| [controllers.go](../../../internal/mini/controllers.go) | 调度、Deployment 协调、节点心跳与容器协调 |
| [docker.go](../../../internal/mini/docker.go) | Unix socket Docker 适配、容器/镜像/日志/回收 |
| [proxy.go](../../../internal/mini/proxy.go) | 后端刷新和 HTTP 轮询代理 |
| [cmd/mini/main.go](../../../cmd/mini/main.go) | 组件角色、循环、HTTP 服务及退出信号 |
| [cmd/kk/](../../../cmd/kk/) | CLI、manifest 解析、集群生命周期、凭据和可选 Docker 配置 |
| [compose.yaml](../../../compose.yaml) | 服务拓扑、网络、卷、loopback 端口 |
| [Makefile](../../../Makefile) / [Dockerfile](../../../Dockerfile) / [Dockerfile.etcd](../../../Dockerfile.etcd) | 二进制、etcd 准备与本地镜像构建 |
| [examples/demo.yaml](../../../examples/demo.yaml) | 两副本 Web Deployment 与单 Service |
| [integration_test.go](../../../internal/mini/integration_test.go) | 真实集群验收；默认跳过，显式启用才运行 |
| [README.md](../../../README.md) / [README.zh-CN.md](../../../README.zh-CN.md) | 英文/中文项目说明与可移植 Quick Start |

## 依赖与并行边界

```text
T1 资源模型 --> T2 纯协调算法 --------------------+
      |                                        |
      +-----> T3 存储/API/Client -----> T5 CLI ---+--> T6 部署 --> T7 验收
      |              |                         |
      +-----> T4 Docker 适配 + 组件运行 ---------+
```

- T2 的纯算法只依赖模型；运行中的控制循环同时依赖 T3/T4。
- T4 的 Docker 适配与 T5 的 CLI 在接口冻结后可以并行。
- T6 消费全部可执行程序和 CLI 生命周期逻辑；T7 依赖完整部署路径。
- 原实施中由主线程处理 API/控制循环，两个子代理分别处理 Docker 适配和 CLI/部署；
  文件所有权不重叠，统一构建与 E2E 负责集成。
- API Client、Resource、Docker 方法签名先固定；接口调整须通知其消费者，而不是
  让多个实现者同时改共享文件。

## T1. 资源契约与校验

**Files:** [model.go](../../../internal/mini/model.go)、
[model_test.go](../../../internal/mini/model_test.go)、[go.mod](../../../go.mod)。

**Interfaces:**

```go
func Strict(data []byte, target any) error
func Decode(data []byte) (Resource, error)
func Validate(r *Resource) error
func CheckUpdate(old, next Resource) error
```

输出 `Resource`、`Metadata`、`Status`、`PodSpec`、`DeploymentSpec`、`ServiceSpec`
给后续任务。Spec 为 `json.RawMessage`，按 Kind 严格解析。

- [x] 固化校验测试：合法单容器 Pod 通过；未知字段、非法名称、非 default
  namespace、负副本数、错误 apiVersion 失败。
- [x] 固化更新测试：同模板 replicas 2 → 3 允许，修改 image 拒绝。
- [x] 实现四种 Kind、版本对应关系、显式副本上限、Service 单端口和服务器字段结构。
- [x] 运行验证：

```sh
go test ./internal/mini -run 'TestValidation|TestOnlyReplicaUpdatesAllowed' -count=1
```

**Expected:** PASS；不能通过放宽未知字段解析来使测试通过。

## T2. 调度、副本所有权与 HTTP 代理算法

**Files:** [model.go](../../../internal/mini/model.go)、
[controllers.go](../../../internal/mini/controllers.go)、
[proxy.go](../../../internal/mini/proxy.go)、
[controllers_test.go](../../../internal/mini/controllers_test.go)。

**Consumes:** T1 的模型；接入实际轮询时使用 T3 的 Client。

**Produces:**

```go
func ChooseNode(nodes, pods []Resource, now time.Time) string
func OwnedPods(d Resource, pods []Resource) []Resource
func Schedule(ctx context.Context, c *Client) error
func ReconcileDeployments(ctx context.Context, c *Client) error
func NewProxy() *Proxy
func (p *Proxy) Refresh(ctx context.Context, c *Client) error
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request)
```

- [x] 验证 15 秒之外的节点不入选；已分配数较少者优先，批次内分配计入下一次选择。
- [x] 验证 ownerUID 不同的同标签 Pod 不属于当前 Deployment。
- [x] 验证无后端返回 503，两次请求访问两个后端且保留路径与查询。
- [x] 实现创建、缩容、失去 owner 后回收；无可用节点保持 Pending 并写原因。
- [x] 实现 Service 快照刷新、标签筛选、节点心跳过滤与 HTTP 轮询代理。
- [x] 接入 T3 后运行：

```sh
go test ./internal/mini -run 'TestScheduler|TestDeploymentOwnership|TestProxy' -count=1
```

**Expected:** PASS。真实副本创建/删除的结果留给 T7 验证，不将模拟 API 测试当成真实容器证据。

## T3. etcd 存储、HTTP API 与共享 Client

**Files:** [store.go](../../../internal/mini/store.go)、
[api.go](../../../internal/mini/api.go)、[api_test.go](../../../internal/mini/api_test.go)。

**Consumes:** T1 的资源模型和校验；运行时连接真实 etcd。

**Produces:**

```go
func NewStore(url string) *Store
func (s *Store) List(ctx context.Context, kind string) ([]Resource, error)
func (s *Store) Get(ctx context.Context, kind, name string) (Resource, error)
func (s *Store) Write(ctx context.Context, r Resource, revision int64, del bool) (Resource, error)
func APIHandler(store *Store, token string) http.Handler
func NewClient(url, token string) *Client
```

Client 提供 `List`、`Get`、`Create`、`Update(ctx, r, status)`、
`Delete`、`Logs`、`Health`；所有资源操作传播 context、认证和错误。
共享 `ErrNotFound`、`ErrConflict` 支持调用方区分失败类别。

- [x] 验证未认证请求在触及存储前返回 401；资源别名只映射受支持 Kind。
- [x] 实现 etcd range/txn、Base64 编码、ModRevision 映射和 Service singleton key。
- [x] 实现资源/状态分离更新与 UID、resourceVersion 检查。
- [x] 实现日志经 API 转发到所属 kubelet，不给 API Server 挂 Docker socket。
- [x] 运行：

```sh
go test ./internal/mini -run 'TestAPI|TestCanonical' -count=1
```

**Expected:** PASS。真实 etcd CAS、重启持久化由 T7 验证；不要凭此命令单独宣布
存储层已通过所有行为测试。

## T4. Docker 运行时与组件运行入口

**Files:** [docker.go](../../../internal/mini/docker.go)、
[docker_test.go](../../../internal/mini/docker_test.go)、
[controllers.go](../../../internal/mini/controllers.go)、
[cmd/mini/main.go](../../../cmd/mini/main.go)。

**Consumes:** T1 PodSpec/Status；节点协调消费 T3 Client，入口接入 T2 控制器和代理。

**Produces:**

```go
func NewDocker(socket, cluster, node, network string) *Docker
func (d *Docker) Ensure(ctx context.Context, p Resource) (Status, error)
func (d *Docker) Cleanup(ctx context.Context, active []Resource) error
func (d *Docker) Logs(ctx context.Context, containerID string, w io.Writer) error
func Heartbeat(ctx context.Context, c *Client, node string) error
func ReconcileNode(ctx context.Context, c *Client, d *Docker, node string) error
```

- [x] 先覆盖 Docker 边界：创建请求字段、所有权、缓存镜像、退出重启及退避。
- [x] 覆盖镜像拉取流错误、非法配置、context 取消、回收失败和日志帧错误。
- [x] 实现 Unix socket HTTP 适配；不使用 Docker SDK、宿主机挂载或 privileged 业务容器。
- [x] 实现独立心跳循环与 Pod 协调循环，提供所有权检查后的日志接口。
- [x] 接入所有角色的启动与信号退出，以及真正提供 HTTP 响应的 demo。
- [x] 运行：

```sh
go test -race ./internal/mini -run '^TestDocker' -count=1
go build ./cmd/mini
```

**Expected:** PASS / exit 0。fake Engine 的 Unix socket 测试不是宿主机 Docker 健康检查。

## T5. kk 资源命令与 manifest

**Files:** [cli.go](../../../cmd/kk/cli.go)、[manifest.go](../../../cmd/kk/manifest.go)、
[main.go](../../../cmd/kk/main.go)，以及对应 CLI/manifest/delete 测试。

**Consumes:** T1 校验、T3 Client、T6 提供的项目定位与 token 读取。

**Produces:**

```go
func readManifests(reader io.Reader) ([]mini.Resource, error)
func (a app) run(ctx context.Context, args []string) error
func (a app) apply(ctx context.Context, resource mini.Resource) (string, error)
func (a app) remove(ctx context.Context, kind, name string) error
```

- [x] 测试多文档、非法类型、服务器字段和 Node manifest 拒绝规则。
- [x] 测试非法批次不会先发送 API 请求；合法批次的实际写入仍是非事务。
- [x] 测试 apply、scale、delete 保留正确 UID/版本，冲突明确传播。
- [x] 测试 `get -o json` 列表为数组、单资源为对象；describe/logs 使用正确 API。
- [x] 实现 apply/get/describe/delete/logs/scale 和非零错误退出。
- [x] 运行：

```sh
go test ./cmd/kk -run 'TestApply|TestGet|TestScale|TestCreateDelete|TestDelete|TestInvalidManifest|TestReadManifests|TestDemoManifest' -count=1
```

**Expected:** PASS；不能把一次模拟 Client 调用视为运行中的集群已部署成功。

## T6. 构建、集群生命周期与用户文档

**Files:** [cluster.go](../../../cmd/kk/cluster.go)、
[docker_config.go](../../../cmd/kk/docker_config.go)、相应 cluster/config/health/etcd 测试、
Makefile、两个 Dockerfile、compose.yaml、examples/demo.yaml、双语 README。

**Consumes:** 已可构建的 `cmd/mini` 和 `cmd/kk`；真实本地 Docker Engine。

**Produces:**

```go
func projectRoot() (string, error)
func loadToken(root string, create bool) (string, error)
func newCluster(ctx context.Context, root, token string, out io.Writer) (cluster, error)
func (c cluster) up(ctx context.Context) error
func (c cluster) waitReady(ctx context.Context) error
func (c cluster) down(ctx context.Context) error
```

- [x] 测试 token 创建/复用和不覆盖无效 token，本地 endpoint 与项目根目录识别。
- [x] 测试 API 与两个新鲜 worker 心跳全部满足才算启动就绪。
- [x] 测试停止协调器必须先于删除容器；停止失败时不能继续删除。
- [x] 测试 etcd 下载只提取目标二进制、缓存可复用，up 构建两个本地镜像并禁止 Compose 拉取。
- [x] 测试可选匿名 Docker 配置隔离，不修改用户配置；预检与构建使用一致命令环境。
- [x] 实现七个 Compose 服务、专用网络、etcd 卷和两个 loopback 端口。
- [x] 编写可移植双语 README，环境要求、等待就绪、部署、扩缩容、停止/恢复均明确。
- [x] 验证代码与构建：

```sh
go test ./cmd/kk -run 'TestToken|TestDocker|TestCluster|TestProjectRoot|TestReadiness|TestAnonymous|TestPrepareEtcd' -count=1
make build
```

**Expected:** PASS；生成本机 `bin/kk` 与 Linux `bin/mini-linux`。
`make prepare-etcd` 需要下载能力或预置匹配缓存，不能假设仓库自带二进制。

手动验证 Compose 语法可使用占位 token，不打印真实凭据：

```sh
MINI_TOKEN=syntax-check MINI_DOCKER_SOCKET=/var/run/docker.sock docker compose config --quiet
```

该命令只验证配置；Linux 的实际启动仍应使用对应本地 socket。

## T7. 集成、自愈与持久化验收

**Files:** [integration_test.go](../../../internal/mini/integration_test.go)。

**Consumes:** T1–T6 完整产物；已启动、本项目专用的可重建实验集群。

**Produces:** `TestLiveCluster`，通过 `MINI_E2E=1` 显式启用。

- [x] 使用真实 `kk apply/get/logs/scale/delete` 与 Docker 命令，不替换成模拟状态。
- [x] 两个初始 Pod 分布到两个逻辑节点，HTTP 响应来自真实容器。
- [x] 在 2 → 3 → 1 副本变更中同时检查资源数和实际 Docker 容器数。
- [x] 终止容器后验证重启计数增加，删除 Pod 后验证替代 UID 不同。
- [x] 检查旧 resourceVersion 被拒绝，控制面重启后 Deployment UID 不变。
- [x] 执行完整 down/up：清理业务容器，保留 UID 和期望副本，重新运行两个真实容器。
- [x] 全局检查：

```sh
go test -race -count=1 ./...
go vet ./...
```

- [x] 真实环境检查：

```sh
./bin/kk cluster up
MINI_E2E=1 go test ./internal/mini -run '^TestLiveCluster$' -count=1 -v -timeout 5m
./bin/kk cluster status
./bin/kk get pods
curl --fail http://127.0.0.1:18080/
```

**Expected:** 所有命令成功；测试日志列出每项行为通过；最终两个 worker Ready、
两个 Pod Running，网关返回 `hello from ...`。

实时测试会修改示例、终止受管理容器、重启项目控制面、执行 down/up；
不能在未经确认的共享实验状态上自动运行。

## 集成审查与实际偏差记录

1. **镜像准备链路**：审查发现仅构建组件镜像不足以满足自定义 etcd 镜像依赖。
   已补齐 `prepare-etcd`、第二个本地镜像构建及对应测试，真实启动通过。
2. **Compose 插值**：socket 的 flow-sequence 字符串缺少引号曾导致解析失败；
   已修正，并通过 Compose 配置解析和真实启动。
3. **Docker 可用性**：曾遇到 Desktop 引擎停止但 API 缓存回答 info/version。
   `kk` 的预检不是完整诊断；用户授权重启后恢复，随后真实验收通过。
   没有自动重置 Docker 数据或清理其他项目。
4. **部署模式**：本机二进制控制面曾作为排障替代被讨论，但最终未实施。
   当前已验证部署仍为 Compose + 共享 Docker Engine。
5. **验证范围**：2026-10-14 的单元/race/vet 和真实 E2E 已通过，
   不能推广为跨平台认证或完整 Kubernetes 兼容。

## 完成定义与后续工作

本轮完成定义是 spec 验收矩阵通过、源码/测试/文档持久化、用户可使用 `kk` 交互，
不是“组件名称齐全”或“编译通过”。

本计划对应的实现已完成。原始更大范围的能力位于 spec 的未来方向中，需作为新需求
重新设计，不在本计划中留下暗示已经实现的勾选项。
