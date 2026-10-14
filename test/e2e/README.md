# mini-k8s e2e 测试

仿照 Kubernetes `test/e2e` 的 Ginkgo v2 + Gomega 端到端测试套件。它对**正在运行的本项目集群**
发起真实操作：通过 `kk` 和 API 创建资源，检查 API 状态、Docker 容器与 HTTP 流量。

> 只在可重建的本项目实验集群上运行。Disruptive 用例会重启控制面并执行 `kk cluster down/up`。

## 运行

```sh
make build
./bin/kk cluster up

make e2e                                   # 全部用例
make e2e-quick                             # 跳过 Disruptive 用例
make e2e E2E_FOCUS='sig-apps'              # 按名称正则筛选
make e2e E2E_LABELS='Disruptive'           # 按 Ginkgo 标签筛选
```

`go test ./...` 也会编译该套件，但没有 `MINI_E2E=1` 时直接跳过。

可选环境变量：`MINI_REPO_ROOT`（仓库根目录，默认自动向上查找）、`MINI_API`
（默认 `http://127.0.0.1:18000`）、`MINI_GATEWAY`（默认 `http://127.0.0.1:18080`）。

## 目录结构

```text
test/e2e/
├── e2e_test.go        # 套件入口：RunSpecs、BeforeSuite 等待集群就绪、空导入各领域包
├── framework/         # 共享框架
│   ├── config.go      # TestContext：仓库根、token、端点、超时
│   ├── framework.go   # Framework、SIGDescribe、标签、失败诊断钩子
│   ├── manifest.go    # 资源构造器、Apply/ApplyExpectError、DeferCleanup 清理
│   ├── wait.go        # 等待 Pod/容器状态、HTTP 工具
│   ├── exec.go        # kk 与 docker 命令执行
│   └── debug.go       # 失败时输出资源、容器与组件日志
├── apimachinery/      # [sig-api-machinery] 并发控制、校验、认证
├── apps/              # [sig-apps] 工作负载控制器
├── network/           # [sig-network] Service 与网关
├── node/              # [sig-node] Node 与 kubelet
└── lifecycle/         # [sig-cluster-lifecycle] 控制面重启、down/up（Disruptive）
```

## 编写约定

1. **每个功能都必须附带 e2e 用例**，覆盖正常路径以及至少一个失败或恢复路径。
2. 用 `framework.SIGDescribe("<area>", "<主题>", ...)` 声明容器，`It` 描述可观察行为，
   以 “should …” 开头。
3. 每个 `Describe` 调用一次 `framework.NewDefaultFramework("<base>")`，资源名使用
   `f.UniqueName(...)`，避免与用户资源或其他用例冲突。
4. 通过 `f.Apply` 创建资源。它注册 `DeferCleanup`，结束时删除资源，并等待 Pod 与容器
   真正消失，下一个用例因此总是从干净状态开始。
5. 断言同时检查 API 与实际运行时（例如 `WaitForRunningPods` 要求每个 Pod 恰好有一个
   运行中的容器），不要只相信 status 字段。
6. 异步行为一律用 `Eventually`/`WaitFor*`，不要 `time.Sleep`。
7. 会重启或停止组件的用例必须加 `framework.WithDisruptive()` 与 `Serial`。
8. 用例失败时框架自动输出资源列表、工作负载容器和各组件最近日志。
