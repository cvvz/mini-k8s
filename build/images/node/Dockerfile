# A mini-k8s node: like a kind node image, it runs its own containerd and the
# kubelet (`mini node-init`). It must run privileged with a private cgroup
# namespace; see compose.yaml.
FROM ubuntu:24.04

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
       containerd runc iptables iproute2 util-linux ca-certificates curl iputils-ping procps \
    && rm -rf /var/lib/apt/lists/*

# crictl is for troubleshooting inside a node (docker exec <node> crictl ps).
ARG CRICTL_VERSION=v1.34.0
ARG TARGETARCH
RUN curl -fsSL "https://github.com/kubernetes-sigs/cri-tools/releases/download/${CRICTL_VERSION}/crictl-${CRICTL_VERSION}-linux-${TARGETARCH:-amd64}.tar.gz" \
       | tar -xz -C /usr/local/bin \
    && printf 'runtime-endpoint: unix:///run/containerd/containerd.sock\nimage-endpoint: unix:///run/containerd/containerd.sock\ntimeout: 30\n' > /etc/crictl.yaml

# One binary: node-init, kubelet and (as /opt/cni/bin/mini-cni and loopback)
# the CNI plugins.
COPY bin/mini-linux /usr/local/bin/mini
RUN mkdir -p /opt/cni/bin /etc/cni/net.d \
    && ln -s /usr/local/bin/mini /opt/cni/bin/mini-cni \
    && ln -s /usr/local/bin/mini /opt/cni/bin/loopback

ENTRYPOINT ["/usr/local/bin/mini", "node-init"]
