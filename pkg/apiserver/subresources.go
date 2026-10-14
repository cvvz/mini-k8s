package apiserver

import (
	"errors"
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

// handleBinding assigns an unscheduled Pod to a node.
func (s *Server) handleBinding(w http.ResponseWriter, r *http.Request, info requestInfo) {
	body, err := readBody(r)
	if err != nil {
		writeError(w, err)
		return
	}
	var b corev1.Binding
	if err := decodeStrict(body, &b); err != nil {
		writeError(w, errBadRequest("strict decoding error: %v", err))
		return
	}
	if (b.Target.Kind != "" && b.Target.Kind != "Node") || b.Target.Name == "" {
		writeError(w, errInvalid(scheme.Pods, info.name, []string{"target: Invalid value: a Node name is required"}))
		return
	}
	for attempt := 0; attempt < 3; attempt++ {
		obj, err := s.get(r.Context(), scheme.Pods, info.namespace, info.name)
		if err != nil {
			writeError(w, err)
			return
		}
		pod := obj.(*corev1.Pod)
		if pod.Spec.NodeName != "" {
			writeError(w, newStatus(http.StatusConflict, metav1.ReasonConflict, "pod %s is already assigned to node %q", info.name, pod.Spec.NodeName))
			return
		}
		if pod.DeletionTimestamp != nil {
			writeError(w, newStatus(http.StatusConflict, metav1.ReasonConflict, "pod %s is being deleted", info.name))
			return
		}
		pod.Spec.NodeName = b.Target.Name
		pod.Status.SetCondition(corev1.PodCondition{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Now()})
		_, err = s.update(r.Context(), scheme.Pods, pod)
		var se *statusError
		if errors.As(err, &se) && se.status.Reason == metav1.ReasonConflict {
			continue
		}
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: metav1.StatusSuccess, Code: http.StatusCreated})
		return
	}
	writeError(w, errConflict(scheme.Pods, info.name))
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
	container := q.Get("container")
	if container == "" {
		if len(pod.Spec.Containers) != 1 {
			names := make([]string, len(pod.Spec.Containers))
			for i, c := range pod.Spec.Containers {
				names[i] = c.Name
			}
			writeError(w, errBadRequest("a container name must be specified for pod %s, choose one of: %v", pod.Name, names))
			return
		}
		container = pod.Spec.Containers[0].Name
	}
	if pod.Spec.NodeName == "" {
		writeError(w, errBadRequest("pod %s is not scheduled to a node yet", pod.Name))
		return
	}
	nodeObj, err := s.get(ctx, scheme.Nodes, "", pod.Spec.NodeName)
	if err != nil {
		writeError(w, err)
		return
	}
	ip := nodeObj.(*corev1.Node).InternalIP()
	if ip == "" {
		writeError(w, newStatus(http.StatusServiceUnavailable, metav1.ReasonServiceUnavail, "node %s reports no InternalIP", pod.Spec.NodeName))
		return
	}
	upstream := url.URL{
		Scheme: "http", Host: net.JoinHostPort(ip, strconv.Itoa(s.cfg.KubeletPort)),
		Path: fmt.Sprintf("/containerLogs/%s/%s/%s", pod.Namespace, pod.Name, container),
	}
	params := url.Values{}
	for _, k := range []string{"follow", "tailLines", "previous"} {
		if v := q.Get(k); v != "" {
			params.Set(k, v)
		}
	}
	upstream.RawQuery = params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream.String(), nil)
	if err != nil {
		writeError(w, err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	resp, err := s.kubelet.Do(req)
	if err != nil {
		writeError(w, newStatus(http.StatusServiceUnavailable, metav1.ReasonServiceUnavail, "kubelet on %s: %v", pod.Spec.NodeName, err))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		writeError(w, newStatus(resp.StatusCode, metav1.ReasonBadRequest, "kubelet: %s", msg))
		return
	}
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
