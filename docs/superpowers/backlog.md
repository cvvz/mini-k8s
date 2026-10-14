# 待办与已知问题

代码审查中发现、但不属于 critical 级别而未在当期修复的问题，以及计划外的改进想法。每一项完成后从这里删除，
并在对应 PR 中说明。

## 已知问题

| 来源 | 问题 | 影响 | 建议修复 |
| --- | --- | --- | --- |
| M3b 审查 | `restartPolicy: OnFailure` 的容器被 liveness/startup 探针停止后若以 0 退出，会被当作成功而不再重启 | 卡死后被探针杀掉的任务类 Pod 可能被误判为 Succeeded | kuberuntime 记录因探针失败而停止的容器 ID；除 Never 外一律重启（同 kubelet `computePodActions`） |
| M3b 审查 | 探针 worker 在上报 liveness/startup 失败后退出；若 `StopContainer` 失败，该实例不再被探测 | containerd 停止容器失败时，容器继续运行且失去 liveness 保护 | worker 上报后清零计数继续探测，实例退出后由 `Sync` 停止 worker |
| M3b 审查 | CPU shares 未截断到 262144；超大的 CPU limit 计算 quota 时可能溢出 | 请求数百核的 Pod 会一直创建失败 | shares 截断到 [2, 262144]；quota 防溢出或在校验中限制上限 |
| M3b 审查 | 已被替换或移除的探针 worker 可能仍上报一次失败 | 产生一条误导性的 `Killing` 事件（对已退出的实例调用 StopContainer 无副作用） | 上报前确认 worker 仍是当前 worker |
