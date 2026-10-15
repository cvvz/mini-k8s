package apiserver

import (
	"context"
	"encoding/json"
	"net/http"

	autoscalingv2 "mini-k8s/pkg/apis/autoscaling/v2"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/scheme"
)

// scalable lists the resources with a scale subresource: those with
// spec.replicas, a selector and status.replicas.
var scalable = map[string]bool{scheme.Deployments.Resource: true, scheme.ReplicaSets.Resource: true,
	scheme.StatefulSets.Resource: true}

// scaleOf reads an object's replica counts and selector as a Scale (the
// autoscaling/v1 kind in Kubernetes).
func scaleOf(obj metav1.Object) (*autoscalingv2.Scale, error) {
	data, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	var fields struct {
		Spec struct {
			Replicas *int32                `json:"replicas"`
			Selector *metav1.LabelSelector `json:"selector"`
		} `json:"spec"`
		Status struct {
			Replicas int32 `json:"replicas"`
		} `json:"status"`
	}
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	m := obj.GetObjectMeta()
	sc := &autoscalingv2.Scale{TypeMeta: metav1.TypeMeta{Kind: "Scale", APIVersion: "autoscaling/v1"},
		ObjectMeta: metav1.ObjectMeta{Name: m.Name, Namespace: m.Namespace, UID: m.UID, ResourceVersion: m.ResourceVersion,
			CreationTimestamp: m.CreationTimestamp},
		Status: autoscalingv2.ScaleStatus{Replicas: fields.Status.Replicas}}
	if fields.Spec.Replicas != nil {
		sc.Spec.Replicas = *fields.Spec.Replicas
	}
	if ls := fields.Spec.Selector; ls != nil {
		sel, err := ls.AsSelector()
		if err != nil {
			return nil, err
		}
		sc.Status.Selector = sel.String()
	}
	return sc, nil
}

// handleScale serves GET, PUT and PATCH on {resource}/{name}/scale; a
// change of spec.replicas updates the object through its strategy.
func (s *Server) handleScale(w http.ResponseWriter, r *http.Request, info requestInfo, dryRun bool) {
	ctx := r.Context()
	if r.Method == http.MethodGet {
		obj, err := s.get(ctx, info.res, info.namespace, info.name)
		if err != nil {
			writeError(w, err)
			return
		}
		sc, err := scaleOf(obj)
		respond(w, http.StatusOK, sc, err)
		return
	}
	body, err := readBody(r)
	if err != nil {
		writeError(w, err)
		return
	}
	target := info
	target.subresource = ""
	for attempt := 1; ; attempt++ {
		updated, conditional, err := s.scaleOnce(ctx, r.Method, target, body, dryRun)
		if err == nil || conditional || !isStaleWrite(err) || attempt == maxUnconditionalAttempts {
			if err != nil {
				writeError(w, err)
				return
			}
			sc, err := scaleOf(updated)
			respond(w, http.StatusOK, sc, err)
			return
		}
	}
}

// scaleOnce applies a Scale (PUT) or a merge patch of the Scale (PATCH)
// to the object's spec.replicas; a resourceVersion in it is a
// precondition.
func (s *Server) scaleOnce(ctx context.Context, method string, info requestInfo, body []byte, dryRun bool) (metav1.Object, bool, error) {
	obj, err := s.get(ctx, info.res, info.namespace, info.name)
	if err != nil {
		return nil, false, err
	}
	var want autoscalingv2.Scale
	switch method {
	case http.MethodPut:
		if err := decodeStrict(body, &want); err != nil {
			return nil, false, errBadRequest("strict decoding error: %v", err)
		}
	case http.MethodPatch:
		current, err := scaleOf(obj)
		if err != nil {
			return nil, false, err
		}
		currentJSON, _ := json.Marshal(current)
		merged, err := mergePatch(currentJSON, body)
		if err != nil {
			return nil, false, errBadRequest("invalid merge patch: %v", err)
		}
		if err := decodeStrict(merged, &want); err != nil {
			return nil, false, errBadRequest("strict decoding error: %v", err)
		}
		if !patchHasResourceVersion(body) {
			want.ResourceVersion = ""
		}
	default:
		return nil, false, newStatus(http.StatusMethodNotAllowed, metav1.ReasonMethodNotAllowed, "%s is not supported on this path", method)
	}
	if want.Spec.Replicas < 0 {
		return nil, false, errInvalid(info.res, info.name, []string{"spec.replicas: Invalid value: " +
			jsonNumber(want.Spec.Replicas) + ": must be greater than or equal to 0"})
	}
	meta := map[string]any{}
	if want.ResourceVersion != "" {
		meta["resourceVersion"] = want.ResourceVersion
	}
	patch, _ := json.Marshal(map[string]any{"metadata": meta, "spec": map[string]any{"replicas": want.Spec.Replicas}})
	updated, err := s.patchOnce(ctx, info, patch, dryRun)
	return updated, want.ResourceVersion != "", err
}

func jsonNumber(n int32) string {
	data, _ := json.Marshal(n)
	return string(data)
}
