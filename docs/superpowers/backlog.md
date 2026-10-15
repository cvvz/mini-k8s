# 待办与已知问题

代码审查中发现、但不属于 critical 级别而未在当期修复的问题，以及计划外的改进想法。每一项完成后从这里删除，
并在对应 PR 中说明。

## 已知问题

| 问题 | 位置 | 影响 | 修法 |
| --- | --- | --- | --- |
| Service repair 先 List Service、再读分配表：两步之间被删除的 Service 释放的 IP/端口会被重新登记为已分配 | `pkg/registry/core/service/repair.go`（M6a 审查） | 该值要等 3 轮（约 3 分钟）后才被回收，期间显式申请它会得到 already allocated；不会重复分配 | 与 Kubernetes 的 repair 相同：先取分配表快照及其版本，再 List，最后用 CAS 整体写回重建的集合 |

## 改进想法

| 想法 | 现状 | 做法 | 收益 |
| --- | --- | --- | --- |
| API 改用明文 HTTP/2（h2c） | API Server 与 kk、各组件之间是明文 HTTP/1.1，每个 informer 的 watch 是一条 chunked JSON 长连接，各占一条 TCP 连接 | 服务端用 Go 1.24+ 的 `http.Server.Protocols.SetUnencryptedHTTP2`，客户端 Transport 同样启用；watch 的 JSON 行流格式不变 | 一个进程的所有 watch 复用一条连接，更接近 Kubernetes 客户端经 TLS 默认使用 HTTP/2 的行为 |

## 高负载下偶发失败的 e2e（2026-10-15，M8b 发现）

- `[sig-node] Probes should hold off liveness checks until the startup probe passes`：长时间运行时，kubelet 的 TCP 启动探针连接 Pod IP 出现 `i/o timeout`，容器被重启；单独运行可通过（3/3）。在提速 e2e 框架时一并处理（探针超时与节点负载）。
