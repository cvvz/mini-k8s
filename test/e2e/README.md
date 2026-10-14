# mini-k8s e2e 测试

仿照 Kubernetes `test/e2e` 的 Ginkgo v2 + Gomega 端到端测试套件。它对**正在运行的本项目集群**
发起真实操作：通过 `kk` 和 API 创建资源，检查 API 状态、节点内的 CRI 容器（经 `crictl`）与 HTTP 流量。

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

Service 与 Ingress 用例使用各自的命名空间和 Ingress host，可以与 Quick Start 部署的示例共存。

可选环境变量：`MINI_REPO_ROOT`（仓库根目录，默认自动向上查找）、`MINI_API`
（默认 `http://127.0.0.1:18000`）、`MINI_INGRESS`（Ingress 控制器，默认 `http://127.0.0.1:18080`）。

## 目录结构

```text
test/e2e/
├── e2e_test.go        # 套件入口：RunSpecs、BeforeSuite 等待集群就绪、空导入各领域包
├── framework/         # 共享框架
│   ├── config.go      # TestContext：仓库根、token、端点、超时
│   ├── framework.go   # Framework（每个 spec 一个命名空间）、SIGDescribe、标签、失败诊断钩子
│   ├── manifest.go    # 资源构造器、Apply/ApplyExpectError/KK（自动带 -n）
│   ├── wait.go        # 等待 Pod/容器状态、Pod 消失、Events、HTTP 工具
│   ├── exec.go        # kk、docker 与节点内 crictl 命令执行
│   └── debug.go       # 失败时输出资源、容器与组件日志
├── apimachinery/      # [sig-api-machinery] 并发控制、校验、认证
├── apps/              # [sig-apps] 工作负载控制器
├── network/           # [sig-network] Pod 网络、Service、kube-proxy 与 Ingress
├── node/              # [sig-node] Node 与 kubelet
└── lifecycle/         # [sig-cluster-lifecycle] 控制面重启、down/up（Disruptive）
```

## 编写约定

1. **每个功能都必须附带 e2e 用例**，覆盖正常路径以及至少一个失败或恢复路径。
2. 用 `framework.SIGDescribe("<area>", "<主题>", ...)` 声明容器，`It` 描述可观察行为，
   以 “should …” 开头。
3. 每个 `Describe` 调用一次 `framework.NewDefaultFramework("<base>")`。框架在每个 spec 前
   创建唯一命名空间 `f.Namespace`（`e2e-<base>-xxxxxx`），结束时删除并等待命名空间控制器
   清空其中所有资源，下一个用例因此总是从干净状态开始。需要额外命名空间时用
   `f.CreateNamespace(ctx, "suffix")`，同样会自动清理。
4. 用 `f.Apply`/`f.KK` 操作资源，它们自动带上 `-n f.Namespace`；直接调用 API 时使用
   `client.Pods(f.Client, f.Namespace)` 等类型化客户端。
5. 断言同时检查 API 与实际运行时（例如 `WaitForRunningPods` 要求 Pod Running 且 Ready，并且
   恰好有一个运行中的容器；`WaitForPodGone` 同时检查 API 与节点内的 CRI 容器），不要只相信 status 字段。
6. 异步行为一律用 `Eventually`/`WaitFor*`，不要 `time.Sleep`；不要在被 `Eventually` 轮询的
   函数里再嵌套 `Eventually`。
7. 会重启或停止组件的用例必须加 `framework.WithDisruptive()` 与 `Serial`；依赖集群级单例
   （例如当前唯一的网关 Service）的用例加 `Serial`。
8. 读取会被控制器并发修改的对象时，先等控制器稳定（例如 `observedGeneration == generation`），
   避免测试自身与控制器产生版本冲突。
9. 用例失败时框架自动输出节点、命名空间内的资源与事件、工作负载容器和各组件最近日志。
