# M9b Job 与 CronJob 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 新增 batch/v1 的 Job 与 CronJob。Job 控制器支持 completions/parallelism、NonIndexed 与 Indexed、backoffLimit 与失败退避、
activeDeadlineSeconds、suspend；ttlSecondsAfterFinished 控制器清理结束的 Job。CronJob 控制器按 cron 表达式（含时区）创建 Job，支持
concurrencyPolicy、startingDeadlineSeconds、suspend 与历史上限。kk 支持 `get/describe jobs|cronjobs`、`create job NAME --image IMG -- CMD`
与 `create job NAME --from=cronjob/NAME`。

**Architecture:**

- **API**：Job 的 Strategy 在创建时生成 selector `batch.kubernetes.io/controller-uid=<uid>`（未设 manualSelector 时），并给模板与 Job 加
  标签。
- **Job 控制器**（`pkg/controller/job`）：按 Pod phase 计数，沿用旧版的 legacy tracking，不加 job-tracking finalizer。
  - 根据 Pod 数量创建或删除 Pod，判定 Complete 或 Failed。
  - 写入 status 与条件。
- **TTL 控制器**（`pkg/controller/ttlafterfinished`）：在 Job 结束满 TTL 后删除 Job（后台级联删除其 Pod）。
- **CronJob 控制器**（`pkg/controller/cronjob`）：
  - 用自写的 cron 解析器计算最近一次未执行的调度时间，按并发策略处理后创建 Job。
  - 更新 `status.active`、`lastScheduleTime`、`lastSuccessfulTime`，清理超过历史上限的 Job。
  - 在下次调度时间重新入队。

**Tech Stack:** Go 1.26（`time/tzdata` 内嵌时区）、Ginkgo e2e。

## Global Constraints

- 只在 WSL（Ubuntu 24.04）中构建与测试；文档只出现 Linux 命令。
- 每个功能附带单元测试与 e2e；里程碑验收跑相关 e2e。
- 行为与文案以 AKS（v1.35）实测为准（2026-10-15，临时命名空间 `mini-k8s-probe-m9b`）。

## 语义（AKS 实测）

| 项 | 规则 / 文案 |
| --- | --- |
| kk get jobs | `NAME STATUS COMPLETIONS DURATION AGE`（STATUS：Running、Complete、Failed、Suspended；COMPLETIONS `2/3`，无 completions 时 `n/1 of m`）；wide 加 `CONTAINERS IMAGES SELECTOR` |
| kk get cronjobs | `NAME SCHEDULE TIMEZONE SUSPEND ACTIVE LAST SCHEDULE AGE`（`<none>`、`False`）；wide 加 `CONTAINERS IMAGES SELECTOR` |
| Job 默认值 | parallelism 1；completions 1（两者都未设时）；backoffLimit 6；completionMode NonIndexed；suspend false；manualSelector false；podReplacementPolicy TerminatingOrFailed |
| 标签 | Job 与 Pod：`batch.kubernetes.io/job-name`、`job-name`；Pod 另有 `batch.kubernetes.io/controller-uid`、`controller-uid`；selector 为 `batch.kubernetes.io/controller-uid` |
| Pod | `generateName: <job>-`（Indexed 为 `<job>-<index>-`，并带标签与注解 `batch.kubernetes.io/job-completion-index`、环境变量 `JOB_COMPLETION_INDEX`（fieldRef 该标签）、hostname `<job>-<index>`） |
| 条件 | 完成：`SuccessCriteriaMet` 与 `Complete`（reason `CompletionsReached`，message `Reached expected number of succeeded pods`），并写 completionTime；失败：`FailureTarget` 与 `Failed`（`BackoffLimitExceeded` / `Job has reached the specified backoff limit`，`DeadlineExceeded` / `Job was active longer than specified deadline`） |
| 失败重建 | 第 n 次失败后等 10s×2^(n-1)（上限 6 分钟）再建新 Pod；OnFailure 时容器重启次数计入 backoffLimit |
| 事件 | `Normal SuccessfulCreate Created pod: x`、`Normal Completed Job completed`、`Warning BackoffLimitExceeded …`、`Warning DeadlineExceeded …`、`Normal SuccessfulDelete Deleted pod: x` |
| Job 校验 | `spec.completions: Invalid value: -1: must be greater than or equal to 0`、`spec.backoffLimit: …`、`spec.template.spec.restartPolicy: Required value: valid values: "OnFailure", "Never"`；更新 `spec.completions: Invalid value: 5: field is immutable`；selector、template 不可变 |
| CronJob | Job 名 `<cj>-<调度时间的 Unix 分钟数>`，注解 `batch.kubernetes.io/cronjob-scheduled-timestamp`（RFC3339），属主 CronJob；`status.lastScheduleTime`、`lastSuccessfulTime`、`active`；默认 concurrencyPolicy Allow、suspend false、successfulJobsHistoryLimit 3、failedJobsHistoryLimit 1 |
| CronJob 事件 | `Normal SuccessfulCreate Created job x`、`Normal SawCompletedJob Saw completed job: x, condition: Complete`、`Normal SuccessfulDelete Deleted job x` |
| CronJob 校验 | `spec.schedule: Invalid value: "every minute": expected exactly 5 fields, found 2: [every minute]`；`spec.concurrencyPolicy: Unsupported value: "Sometimes": supported values: "Allow", "Forbid", "Replace"` |
| kk create job --from | 注解 `cronjob.kubernetes.io/instantiate: manual`，属主为该 CronJob；`job.batch/NAME created` |

## 决策

| 决策 | 选择 |
| --- | --- |
| 计数 | legacy tracking（以现存 Pod 为准），不实现 job-tracking finalizer 与 uncountedTerminatedPods；`status.uncountedTerminatedPods` 恒为空 |
| cron | 自写解析：5 字段（数字、`*`、范围、步长、列表、月份与星期名称）与 `@yearly/@annually/@monthly/@weekly/@daily/@midnight/@hourly`；`timeZone` 用 `time.LoadLocation`（内嵌 tzdata） |
| 错过的调度 | 只补最近一次；超过 startingDeadlineSeconds 则跳过（事件 `Warning MissSchedule`） |
| 不做 | podFailurePolicy、successPolicy、backoffLimitPerIndex、elastic Indexed Job、podReplacementPolicy 的 Failed 模式 |

### Task 1: API 与 kk

- [ ] batch/v1 类型、默认值、校验、registry（selector 生成）、scheme、client；kk 列、describe、create job。

### Task 2: 控制器

- [ ] Job 控制器（单元测试：计数、状态、退避；集成测试：完成、失败、Indexed、deadline、suspend）；TTL 控制器。
- [ ] CronJob 控制器与 cron 解析（单元测试：解析、next；集成测试用假时钟：创建、Forbid/Replace、历史清理）。

### Task 3: e2e 与文档

- [ ] `[sig-apps] Job`：完成、失败、Indexed、deadline、TTL；`[sig-apps] CronJob`：按分钟创建并完成、`create job --from`。
- [ ] README、spec；审查；PR。
