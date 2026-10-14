# 待办与已知问题

代码审查中发现、但不属于 critical 级别而未在当期修复的问题，以及计划外的改进想法。每一项完成后从这里删除，
并在对应 PR 中说明。

## 已知问题

暂无。

## 改进想法

| 想法 | 现状 | 做法 | 收益 |
| --- | --- | --- | --- |
| API 改用明文 HTTP/2（h2c） | API Server 与 kk、各组件之间是明文 HTTP/1.1，每个 informer 的 watch 是一条 chunked JSON 长连接，各占一条 TCP 连接 | 服务端用 Go 1.24+ 的 `http.Server.Protocols.SetUnencryptedHTTP2`，客户端 Transport 同样启用；watch 的 JSON 行流格式不变 | 一个进程的所有 watch 复用一条连接，更接近 Kubernetes 客户端经 TLS 默认使用 HTTP/2 的行为 |
