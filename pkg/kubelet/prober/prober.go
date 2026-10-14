// Package prober runs container probes for the kubelet, like Kubernetes'
// pkg/kubelet/prober: one worker per running container instance and probe
// type checks the container every periodSeconds and keeps the result the
// kubelet uses for the container's ready and started fields. Liveness and
// startup failures are handed back to the kubelet, which restarts the
// container.
package prober

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
)

// ProbeType names a kind of probe.
type ProbeType string

// Probe types.
const (
	Liveness  ProbeType = "Liveness"
	Readiness ProbeType = "Readiness"
	Startup   ProbeType = "Startup"
)

// ExecFunc runs a command in a container (CRI ExecSync).
type ExecFunc func(ctx context.Context, pod *corev1.Pod, container string, cmd []string, timeout time.Duration) (*corev1.PodExecResult, error)

// Executor runs one probe check.
type Executor struct {
	Exec ExecFunc
}

// Run performs the probe's check against a container of the Pod whose IP is
// podIP. It reports success, and on failure a message like Kubernetes'
// ("HTTP probe failed with statuscode: 500").
func (e Executor) Run(ctx context.Context, pod *corev1.Pod, podIP string, c corev1.Container, p *corev1.Probe) (bool, string) {
	timeout := time.Duration(p.TimeoutSeconds) * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	switch {
	case p.HTTPGet != nil:
		port, err := resolvePort(p.HTTPGet.Port.IsString, p.HTTPGet.Port.IntVal, p.HTTPGet.Port.StrVal, c)
		if err != nil {
			return false, err.Error()
		}
		host := p.HTTPGet.Host
		if host == "" {
			host = podIP
		}
		path := p.HTTPGet.Path
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		url := fmt.Sprintf("%s://%s%s", strings.ToLower(p.HTTPGet.Scheme), net.JoinHostPort(host, strconv.Itoa(port)), path)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return false, err.Error()
		}
		req.Header.Set("User-Agent", "kube-probe/mini")
		for _, h := range p.HTTPGet.HTTPHeaders {
			req.Header.Set(h.Name, h.Value)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false, err.Error()
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 10<<10))
		if resp.StatusCode < 200 || resp.StatusCode >= 400 {
			return false, fmt.Sprintf("HTTP probe failed with statuscode: %d", resp.StatusCode)
		}
		return true, ""
	case p.TCPSocket != nil:
		port, err := resolvePort(p.TCPSocket.Port.IsString, p.TCPSocket.Port.IntVal, p.TCPSocket.Port.StrVal, c)
		if err != nil {
			return false, err.Error()
		}
		host := p.TCPSocket.Host
		if host == "" {
			host = podIP
		}
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			return false, err.Error()
		}
		_ = conn.Close()
		return true, ""
	case p.Exec != nil:
		res, err := e.Exec(ctx, pod, c.Name, p.Exec.Command, timeout)
		if err != nil {
			return false, err.Error()
		}
		if res.ExitCode != 0 {
			out := strings.TrimSpace(string(res.Stdout) + string(res.Stderr))
			if out == "" {
				out = fmt.Sprintf("command %q exited with %d", strings.Join(p.Exec.Command, " "), res.ExitCode)
			}
			return false, out
		}
		return true, ""
	}
	return false, "probe has no handler"
}

// resolvePort returns a port number, looking names up in the container's ports.
func resolvePort(named bool, number int32, name string, c corev1.Container) (int, error) {
	if !named {
		return int(number), nil
	}
	for _, p := range c.Ports {
		if p.Name == name {
			return int(p.ContainerPort), nil
		}
	}
	return 0, fmt.Errorf("couldn't find port %q in container %s", name, c.Name)
}
