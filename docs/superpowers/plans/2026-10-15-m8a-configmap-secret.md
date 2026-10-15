# M8a ConfigMap、Secret 与投射卷实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 新增 ConfigMap、Secret 资源与 `kk create configmap|secret`；容器 env 支持 `valueFrom`（configMapKeyRef、secretKeyRef、
fieldRef、resourceFieldRef）、`envFrom` 与 `$(VAR)` 展开；新增 configMap、secret、downwardAPI、projected 卷（原子写入、
只读挂载、内容变化后自动刷新）。

**Architecture:** API 侧是两个新资源及其 Strategy（校验、不可变、Secret `stringData` 合并），Pod 校验扩展 env 与卷。kubelet
增加 ConfigMap、Secret 的 informer：启动容器时解析 env，引用缺失时报 `CreateContainerConfigError`；每次同步 Pod 时由各卷的
payload 经原子写入器（`..data` 符号链接切换）落盘，内容未变则不写，引用缺失时 Pod 停在 `ContainerCreating` 并发
`FailedMount` 事件。Downward API 字段解析放在 `pkg/fieldpath`，`$(VAR)` 展开放在 `pkg/util/expansion`，原子写入与 payload
放在 `pkg/volume`（对应 Kubernetes 的 `pkg/volume/util` 与各卷插件）。

**Tech Stack:** Go 1.26、Ginkgo e2e。

## Global Constraints

- 只在 WSL（Ubuntu 24.04）中构建与测试；文档只出现 Linux 命令。
- 每个功能附带单元测试与 e2e；里程碑验收跑相关 e2e。
- 行为与文案以 AKS（v1.35）实测为准（2026-10-15，临时命名空间 `mini-k8s-probe-m8a*`，已删除）。

## 语义（AKS 实测）

| 项 | 规则 / 文案 |
| --- | --- |
| ConfigMap | `data`（字符串）与 `binaryData`（base64）；键匹配 `[-._a-zA-Z0-9]+`、不超过 253 字符、不能是 `.`/`..`：`data[a/b]: Invalid value: "a/b": a valid config key must consist of alphanumeric characters, '-', '_' or '.' (e.g. 'key.name',  or 'KEY_NAME',  or 'key-name', regex used for validation is '[-._a-zA-Z0-9]+')`；两者键重复：`data[k]: Invalid value: "k": duplicate of key present in binaryData`；总大小不超过 1MiB |
| Secret | `data`（base64）；`stringData` 写入时合并进 `data`（同名覆盖）且不保存；`type` 默认 `Opaque`、不可变（`type: Invalid value: "kubernetes.io/tls": field is immutable`）；`kubernetes.io/tls` 要求 `tls.crt`、`tls.key`，`kubernetes.io/basic-auth` 要求 `username` 或 `password`（缺两者时两条 Required），`kubernetes.io/ssh-auth` 要求 `ssh-privatekey`，`kubernetes.io/dockerconfigjson` 要求 `.dockerconfigjson` 且为合法 JSON（`Invalid value: "<secret contents redacted>": ...`）；其他自定义类型不检查 |
| immutable | 设为 true 后：`data: Forbidden: field is immutable when \`immutable\` is set`、`immutable: Forbidden: field is immutable when \`immutable\` is set`；metadata 仍可改 |
| kk get | ConfigMap：`NAME DATA AGE`；Secret：`NAME TYPE DATA AGE`（DATA = 键数） |
| kk describe | ConfigMap 列出 Data（`key:` / `----` / 值）与 BinaryData（`key: N bytes`）；Secret 列出 `Type:` 与 `key:  N bytes`，不显示值 |
| kk create | `configmap NAME --from-literal=k=v --from-file=[key=]path`（目录取其中的普通文件，非 UTF-8 内容进 binaryData）→ `configmap/NAME created`；`secret generic NAME [--type T] ...` 同理；`secret tls NAME --cert F --key F`；重复键：`error: cannot add key "a", another key by that name already exists in Data for ConfigMap "bad"` |
| env valueFrom 缺失 | 容器 waiting `CreateContainerConfigError`，消息 `configmap "nope" not found`、`couldn't find key zzz in ConfigMap ns/cm1`、`secret "x" not found`、`couldn't find key zzz in Secret ns/s1`；事件 `Warning Failed Error: <消息>`；`optional: true` 时跳过 |
| envFrom | 键加 `prefix` 后全部注入（1.35 放宽变量名：除 `=` 外的可打印 ASCII，`1bad`、`a-b` 均可）；后出现的覆盖先出现的，`env` 覆盖 `envFrom` |
| `$(VAR)` | env 的 value、command、args 中引用已定义变量（含先前的 env、envFrom、Service 变量）；未定义保持原样，`$$` 转义为 `$` |
| fieldRef（env） | `metadata.name/namespace/uid`、`metadata.labels['k']`、`metadata.annotations['k']`、`spec.nodeName`、`spec.serviceAccountName`、`status.hostIP(s)`、`status.podIP(s)`；不支持时：`Unsupported value: "metadata.labels": supported values: ...`；未知字段：`Invalid value: "metadata.bogus": error converting fieldPath: field label not supported: metadata.bogus` |
| resourceFieldRef | `limits/requests.cpu/memory/ephemeral-storage`；值 = ceil(量/divisor)，cpu divisor 只能 `1m`、`1`，memory 为 `1`、`1k`…`1Ei`；未设 limit 时取节点 allocatable；250m 的 `limits.cpu` 默认 divisor 为 `1` |
| 卷布局 | `<dir>/..<时间戳>/` 存放文件，`..data` → 该目录，顶层每个路径首段一个符号链接（`a -> ..data/a`、`sub -> ..data/sub`）；更新时写新时间戳目录、原子替换 `..data`、删旧目录；文件默认 0644（`defaultMode`、`items[].mode` 可改） |
| 卷介质 | configMap 在磁盘；secret、downwardAPI、projected 是 tmpfs；全部只读挂载（写入报 Read-only file system） |
| 卷缺失 | Pod 停在 `ContainerCreating`，事件 `Warning FailedMount MountVolume.SetUp failed for volume "cfg" : configmap "nope" not found`（secret 为 `secret "x" not found`；items 引用不存在的键：`configmap references non-existent config key: zzz`）；`optional: true` 时为空卷、缺失的键跳过 |
| downwardAPI 卷 | 字段：`metadata.annotations/labels/name/namespace/uid`（含 `['k']`），resourceFieldRef 需 `containerName`；labels/annotations 渲染为排序后的 `k="v"` 行（Go `%q`），无结尾换行；标签变化后文件随之更新 |
| 更新 | ConfigMap、Secret、Pod 标签变化后卷内容更新（AKS 约 5s；mini 每秒同步）；env 不随之更新 |
| Pod 校验 | `valueFrom: Invalid value: "": may not be specified when \`value\` is not empty`；`valueFrom: Invalid value: "": must specify one of: \`fieldRef\`, \`resourceFieldRef\`, \`configMapKeyRef\` or \`secretKeyRef\``；`envFrom: Invalid value: "": must specify one of: \`configMapRef\` or \`secretRef\``；`configMap.name: Required value`；`secret.secretName: Required value`；`items[0].key: Required value`；`items[0].path: Invalid value: "../x": must not contain '..'`；`defaultMode: Invalid value: 512: must be a number between 0 and 0777 (octal), both inclusive`；`downwardAPI.path: Invalid value: "/abs": must be a relative path`；`downwardAPI: Required value: one of fieldRef and resourceFieldRef is required`；`downwardAPI.resourceFieldRef.containerName: Required value`；`projected: Invalid value: "b": conflicting duplicate paths`；`configMap: Forbidden: may not specify more than 1 volume type`；env 名：`Invalid value: "a=b": a valid environment variable name must consist only of printable ASCII characters other than '='` |

## 决策

| 决策 | 选择 |
| --- | --- |
| kubelet 获取对象 | 集群范围的 ConfigMap、Secret informer（Kubernetes 默认按 Pod 引用逐个 watch，效果相同，实现更简单） |
| 卷刷新 | 每次 Pod 同步（约每秒）重算 payload；与磁盘内容相同则不写 |
| 卷失败的状态 | 由 `CreateContainerConfigError` 改为与 Kubernetes 一致的 `ContainerCreating` + `FailedMount` 事件（同样适用于已有的 hostPath 检查失败） |
| 不做 | `subPath`、`fileKeyRef`、serviceAccountToken 投射、`--from-env-file`、kube-root-ca.crt |

### Task 1: API

- [ ] `corev1.ConfigMap`、`corev1.Secret`；scheme、client、registry（Strategy：校验、不可变、`stringData` 合并、type 默认值）。
- [ ] `kk get`/`describe` 列与格式；`kk create configmap`、`kk create secret generic|tls`。
- [ ] 单元测试与集成测试：校验文案、`stringData`、不可变。

### Task 2: env

- [ ] `EnvVar.ValueFrom`、`Container.EnvFrom` 与校验；`pkg/util/expansion`；`pkg/fieldpath`（字段与资源值）。
- [ ] kubelet：ConfigMap、Secret informer；启动容器时解析 env 与 command/args，失败报 `CreateContainerConfigError`。

### Task 3: 卷

- [ ] `VolumeSource` 增加 configMap、secret、downwardAPI、projected 与校验；`pkg/volume` 原子写入器与 payload。
- [ ] kubelet：tmpfs、只读挂载、每次同步刷新、`ContainerCreating` + `FailedMount`。

### Task 4: e2e 与文档

- [ ] e2e（`[sig-storage]`）：ConfigMap env/envFrom/卷与更新、Secret env/卷（tmpfs、只读、更新）、downward API env/卷（标签更新）、
  projected、缺失与 optional、immutable、`kk create`。
- [ ] README、spec；审查；PR。
