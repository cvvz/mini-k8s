package apiserver_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	appsv1 "mini-k8s/pkg/apis/apps/v1"
	autoscalingv2 "mini-k8s/pkg/apis/autoscaling/v2"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/util/ptr"
)

func TestScaleSubresource(t *testing.T) {
	c, _ := newServer(t)
	ctx := context.Background()
	labels := map[string]string{"app": "web"}
	d, err := client.Deployments(c, "default").Create(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web"},
		Spec: appsv1.DeploymentSpec{Replicas: ptr.To[int32](2), Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	path := "/apis/apps/v1/namespaces/default/deployments/web/scale"
	var sc autoscalingv2.Scale
	if err := c.Do(ctx, http.MethodGet, path, nil, "", nil, &sc); err != nil {
		t.Fatal(err)
	}
	if sc.Kind != "Scale" || sc.APIVersion != "autoscaling/v1" || sc.Spec.Replicas != 2 || sc.Status.Selector != "app=web" ||
		sc.UID != d.UID || sc.ResourceVersion != d.ResourceVersion {
		t.Fatalf("scale: %+v", sc)
	}

	// PUT with the current resourceVersion; then the stale one conflicts.
	sc.Spec.Replicas = 4
	var out autoscalingv2.Scale
	if err := c.Do(ctx, http.MethodPut, path, nil, "", &sc, &out); err != nil || out.Spec.Replicas != 4 {
		t.Fatalf("put: %+v %v", out, err)
	}
	if err := c.Do(ctx, http.MethodPut, path, nil, "", &sc, nil); !client.IsConflict(err) {
		t.Fatalf("stale put: %v", err)
	}
	got, _ := client.Deployments(c, "default").Get(ctx, "web")
	if *got.Spec.Replicas != 4 || got.Generation != d.Generation+1 {
		t.Fatalf("deployment after put: %d gen %d", *got.Spec.Replicas, got.Generation)
	}

	// A merge patch of the Scale, as kubectl scale sends.
	if err := c.Do(ctx, http.MethodPatch, path, nil, "application/merge-patch+json", []byte(`{"spec":{"replicas":1}}`), &out); err != nil ||
		out.Spec.Replicas != 1 {
		t.Fatalf("patch: %+v %v", out, err)
	}
	if err := c.Do(ctx, http.MethodPatch, path, nil, "application/merge-patch+json", []byte(`{"spec":{"replicas":-1}}`), nil); !client.IsInvalid(err) ||
		!strings.Contains(err.Error(), "spec.replicas: Invalid value: -1: must be greater than or equal to 0") {
		t.Fatalf("negative: %v", err)
	}
	if err := c.Do(ctx, http.MethodGet, "/api/v1/namespaces/default/pods/x/scale", nil, "", nil, nil); !client.IsNotFound(err) {
		t.Fatalf("pods have no scale: %v", err)
	}
	if err := c.Do(ctx, http.MethodGet, "/apis/apps/v1/namespaces/default/deployments/ghost/scale", nil, "", nil, nil); !client.IsNotFound(err) ||
		!strings.Contains(err.Error(), `deployments.apps "ghost" not found`) {
		t.Fatalf("missing target: %v", err)
	}
}
