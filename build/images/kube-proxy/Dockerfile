# kube-proxy needs the iptables tools; like the real kube-proxy image
# (distroless-iptables) it ships them next to the binary. Ubuntu 24.04's
# iptables uses the nf_tables backend, the same as the node image, so the
# kube-proxy rules and the kubelet's host-gw rules live in the same tables.
FROM ubuntu:24.04

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends iptables conntrack \
    && rm -rf /var/lib/apt/lists/*

COPY bin/mini-linux /usr/local/bin/mini

ENTRYPOINT ["/usr/local/bin/mini", "kube-proxy"]
