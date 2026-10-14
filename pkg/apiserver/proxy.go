package apiserver

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/scheme"
)

// podContainer resolves the container a log or exec request names; an empty
// name selects the only container of a single-container Pod.
func podContainer(pod *corev1.Pod, name string) (string, error) {
	names := func(cs []corev1.Container) []string {
		out := make([]string, len(cs))
		for i, c := range cs {
			out[i] = c.Name
		}
		return out
	}
	if name == "" {
		if len(pod.Spec.Containers) == 1 {
			return pod.Spec.Containers[0].Name, nil
		}
		msg := fmt.Sprintf("a container name must be specified for pod %s, choose one of: %v", pod.Name, names(pod.Spec.Containers))
		if len(pod.Spec.InitContainers) > 0 {
			msg += fmt.Sprintf(" or one of the init containers: %v", names(pod.Spec.InitContainers))
		}
		return "", errBadRequest("%s", msg)
	}
	for _, c := range append(append([]corev1.Container{}, pod.Spec.InitContainers...), pod.Spec.Containers...) {
		if c.Name == name {
			return name, nil
		}
	}
	return "", errBadRequest("container %s is not valid for pod %s", name, pod.Name)
}

// kubeletRequest builds an authenticated request to path on the kubelet of
// the Pod's node.
func (s *Server) kubeletRequest(ctx context.Context, method string, pod *corev1.Pod, path string, query url.Values) (*http.Request, error) {
	if pod.Spec.NodeName == "" {
		return nil, errBadRequest("pod %s is not scheduled to a node yet", pod.Name)
	}
	nodeObj, err := s.get(ctx, scheme.Nodes, "", pod.Spec.NodeName)
	if err != nil {
		return nil, err
	}
	ip := nodeObj.(*corev1.Node).InternalIP()
	if ip == "" {
		return nil, newStatus(http.StatusServiceUnavailable, metav1.ReasonServiceUnavail, "node %s reports no InternalIP", pod.Spec.NodeName)
	}
	u := url.URL{Scheme: "http", Host: net.JoinHostPort(ip, strconv.Itoa(s.cfg.KubeletPort)), Path: path, RawQuery: query.Encode()}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	return req, nil
}

// doKubelet sends req and returns the response if the kubelet answered 200.
func (s *Server) doKubelet(req *http.Request, node string) (*http.Response, error) {
	resp, err := s.kubelet.Do(req)
	if err != nil {
		return nil, newStatus(http.StatusServiceUnavailable, metav1.ReasonServiceUnavail, "kubelet on %s: %v", node, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, newStatus(resp.StatusCode, metav1.ReasonBadRequest, "kubelet: %s", msg)
	}
	return resp, nil
}

// handleLog proxies a container log stream from the Pod's kubelet.
func (s *Server) handleLog(w http.ResponseWriter, r *http.Request, info requestInfo) {
	ctx := r.Context()
	obj, err := s.get(ctx, scheme.Pods, info.namespace, info.name)
	if err != nil {
		writeError(w, err)
		return
	}
	pod := obj.(*corev1.Pod)
	q := r.URL.Query()
	container, err := podContainer(pod, q.Get("container"))
	if err != nil {
		writeError(w, err)
		return
	}
	params := url.Values{}
	for _, k := range []string{"follow", "tailLines", "previous"} {
		if v := q.Get(k); v != "" {
			params.Set(k, v)
		}
	}
	req, err := s.kubeletRequest(ctx, http.MethodGet, pod, fmt.Sprintf("/containerLogs/%s/%s/%s", pod.Namespace, pod.Name, container), params)
	if err != nil {
		writeError(w, err)
		return
	}
	resp, err := s.doKubelet(req, pod.Spec.NodeName)
	if err != nil {
		writeError(w, err)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// handleExec runs a command in a container through the Pod's kubelet and
// returns a PodExecResult. Unlike Kubernetes, the session is not streamed:
// the command runs to completion without stdin or a terminal.
func (s *Server) handleExec(w http.ResponseWriter, r *http.Request, info requestInfo) {
	ctx := r.Context()
	obj, err := s.get(ctx, scheme.Pods, info.namespace, info.name)
	if err != nil {
		writeError(w, err)
		return
	}
	pod := obj.(*corev1.Pod)
	q := r.URL.Query()
	if len(q["command"]) == 0 {
		writeError(w, errBadRequest("you must specify at least 1 command"))
		return
	}
	container, err := podContainer(pod, q.Get("container"))
	if err != nil {
		writeError(w, err)
		return
	}
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		writeError(w, errBadRequest("cannot exec into a container in a completed pod; current phase is %s", pod.Status.Phase))
		return
	}
	params := url.Values{"command": q["command"]}
	if t := q.Get("timeout"); t != "" {
		params.Set("timeout", t)
	}
	req, err := s.kubeletRequest(ctx, http.MethodPost, pod, fmt.Sprintf("/exec/%s/%s/%s", pod.Namespace, pod.Name, container), params)
	if err != nil {
		writeError(w, err)
		return
	}
	resp, err := s.doKubelet(req, pod.Spec.NodeName)
	if err != nil {
		writeError(w, err)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, resp.Body)
}
