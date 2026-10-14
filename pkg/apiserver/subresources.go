package apiserver

import (
	"errors"
	"net/http"

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
