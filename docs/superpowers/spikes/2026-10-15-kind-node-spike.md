# kind 式节点可行性验证（2026-10-15）

目的：在正式实施 kind 式节点（spec §14）之前，验证风险最大的环节在本地环境中可行：
privileged 节点容器内运行 containerd、嵌套 cgroup v2、CRI 创建 Pod 沙箱并调用 CNI、
跨节点 host-gw 路由。

环境：Ubuntu 24.04（WSL2，内核 6.18，cgroup v2，controllers 齐全，`br_netfilter` 已加载）、
Docker Engine 29.9。

## 结论

| 验证项 | 结果 |
| --- | --- |
| privileged 容器内运行 containerd 2.2.1 + runc 1.3.4 | 通过；`crictl info` 中 RuntimeReady、NetworkReady 均为 True |
| 嵌套 cgroup v2 委派 | 通过；入口脚本把进程移入 `init.scope` 后开启全部 controller |
| 节点内运行容器（`ctr run`） | 通过 |
| CRI：RunPodSandbox → CNI 分配 IP → CreateContainer/StartContainer | 通过；创建沙箱约 2 秒；Pod IP 10.244.1.2 / 10.244.2.2 |
| CRI 日志与资源限制 | 通过；`crictl logs` 可读；`memory_limit_in_bytes` 真实写入 cgroup `memory.max` |
| host-gw 跨节点 Pod 通信 | 通过；仅在两个节点上各加一条到对方 Pod 网段的路由，ping 与 TCP 都通 |
| Pod 源 IP 保留 | 通过；对端看到的来源是 Pod IP（10.244.1.2），没有被 NAT |
| Pod 访问外网 | 通过（TCP）；需要在节点内对 `10.244.0.0/16 → 非集群地址` 做 MASQUERADE。注意 WSL 环境本身禁止出站 ICMP，不能用 ping 判断 |
| 真实节点故障 | 通过；停止节点容器后其上的 Pod 立即不可达 |
| 资源开销 | 每个空闲节点容器约 45 MiB 内存，CPU 约 0.2% |

结论：没有原理性障碍，可按 spec §14 实施。

## 已验证的配方

### 节点镜像

```dockerfile
FROM ubuntu:24.04
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      containerd runc iptables iproute2 iputils-ping containernetworking-plugins ca-certificates curl procps \
    && rm -rf /var/lib/apt/lists/*
RUN curl -fsSL https://github.com/kubernetes-sigs/cri-tools/releases/download/v1.34.0/crictl-v1.34.0-linux-amd64.tar.gz \
      | tar -xz -C /usr/local/bin
COPY entrypoint.sh /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
```

正式实现时 crictl 只用于排障；CNI 插件换成自研 `mini-cni`，参考插件可保留作对照。

### 入口脚本（cgroup v2 嵌套的关键）

```bash
#!/bin/bash
set -euo pipefail
if [ -f /sys/fs/cgroup/cgroup.controllers ]; then
  # cgroup v2 "no internal processes"：先把所有进程移入叶子 cgroup，再向子 cgroup 开启全部 controller
  mkdir -p /sys/fs/cgroup/init.scope
  while read -r pid; do echo "$pid" > /sys/fs/cgroup/init.scope/cgroup.procs 2>/dev/null || true; done < /sys/fs/cgroup/cgroup.procs
  sed -e 's/ / +/g' -e 's/^/+/' < /sys/fs/cgroup/cgroup.controllers > /sys/fs/cgroup/cgroup.subtree_control
fi
mount --make-rshared /
sysctl -qw net.ipv4.ip_forward=1
exec containerd
```

正式实现中由 `mini node-init`（Go）完成同样的步骤，再以子进程方式管理 containerd 与 kubelet。

### 节点容器参数

```sh
docker run -d --name NODE --hostname NODE --network CLUSTER_NET \
  --privileged --cgroupns=private \
  --security-opt seccomp=unconfined --security-opt apparmor=unconfined \
  --tmpfs /run --tmpfs /tmp \
  -v /var/lib/containerd \
  -v CONTAINERD_CONFIG_DIR:/etc/containerd:ro -v CNI_CONF_DIR:/etc/cni/net.d:ro \
  NODE_IMAGE
```

- `/var/lib/containerd` 必须是 volume：overlayfs snapshotter 不能叠加在容器自身的 overlay 根文件系统上。
- `/run`、`/tmp` 是 tmpfs，`docker cp` 写不进去；向节点内写文件要通过 `docker exec -i … sh -c 'cat > PATH'` 或 bind mount。

### containerd 配置（v3 格式）

```toml
version = 3
[plugins.'io.containerd.cri.v1.runtime'.cni]
  bin_dirs = ['/usr/lib/cni']
  conf_dir = '/etc/cni/net.d'
```

沙箱镜像默认会从 `registry.k8s.io` 拉取 `pause:3.10.1`。正式实现改为本地构建的 pause 镜像
（`/mini pause`），通过 `[plugins.'io.containerd.cri.v1.images'.pinned_images] sandbox = …` 指定，
并在 `cluster up` 时导入每个节点。

### 本次使用的 CNI 配置（参考插件）

```json
{"cniVersion":"1.0.0","name":"spike","plugins":[
 {"type":"bridge","bridge":"cni0","isGateway":true,"ipMasq":false,
  "ipam":{"type":"host-local","ranges":[[{"subnet":"10.244.N.0/24"}]],"routes":[{"dst":"0.0.0.0/0"}]}},
 {"type":"loopback"}]}
```

### host-gw 与出网

```sh
# 节点 1（Pod 网段 10.244.1.0/24）上：去往节点 2 的 Pod 网段走节点 2 的地址
ip route add 10.244.2.0/24 via NODE2_IP
# 出网：集群外目的地址做源地址转换，集群内（Pod 之间）保持源 IP
iptables -t nat -A POSTROUTING -s 10.244.0.0/16 ! -d 10.244.0.0/16 -j MASQUERADE
```

节点容器位于同一个 Docker 网桥，下一跳在二层直达，宿主机不参与路由，也不做 NAT。

## 实施时要注意的点

1. 镜像导入：本地镜像通过 `docker save … | docker exec -i NODE ctr -n k8s.io images import -` 导入，
   `kk cluster up` 与一个新的 `kk image load` 命令负责。
2. e2e 中“检查 Docker 容器”的辅助函数要改为在节点内调用 CRI（或经 kubelet 查询）。
3. 交互式 exec 需要实现 CRI streaming，可先只支持 ExecSync（非交互）。
4. node IPAM 控制器为每个 Node 分配 `spec.podCIDR`；路由代理（host-gw）监视 Node 列表，
   增删到其他节点 Pod 网段的路由。
