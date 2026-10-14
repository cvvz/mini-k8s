package main

import (
	"strings"
	"testing"

	appsv1 "mini-k8s/pkg/apis/apps/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/util/ptr"
)

func TestRolloutStatusMessagesLikeKubectl(t *testing.T) {
	d := func(gen, observed int64, replicas, updated, total, available int32) *appsv1.Deployment {
		return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Generation: gen},
			Spec:   appsv1.DeploymentSpec{Replicas: ptr.To(replicas)},
			Status: appsv1.DeploymentStatus{ObservedGeneration: observed, UpdatedReplicas: updated, Replicas: total, AvailableReplicas: available}}
	}
	for _, tc := range []struct {
		d    *appsv1.Deployment
		want string
		done bool
	}{
		{d(2, 1, 3, 3, 3, 3), "Waiting for deployment spec update to be observed...", false},
		{d(2, 2, 3, 1, 4, 3), `Waiting for deployment "web" rollout to finish: 1 out of 3 new replicas have been updated...`, false},
		{d(2, 2, 3, 3, 4, 3), `Waiting for deployment "web" rollout to finish: 1 old replicas are pending termination...`, false},
		{d(2, 2, 3, 3, 3, 2), `Waiting for deployment "web" rollout to finish: 2 of 3 updated replicas are available...`, false},
		{d(2, 2, 3, 3, 3, 3), `deployment "web" successfully rolled out`, true},
	} {
		msg, done, err := RolloutStatus(tc.d)
		if err != nil || msg != tc.want || done != tc.done {
			t.Errorf("got %q %v %v, want %q %v", msg, done, err, tc.want, tc.done)
		}
	}
	stuck := d(2, 2, 3, 1, 3, 2)
	stuck.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing, Status: "False", Reason: "ProgressDeadlineExceeded"}}
	if _, _, err := RolloutStatus(stuck); err == nil || !strings.Contains(err.Error(), "exceeded its progress deadline") {
		t.Fatalf("stuck rollout: %v", err)
	}
}
