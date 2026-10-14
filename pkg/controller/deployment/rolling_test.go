package deployment

import (
	"fmt"
	"testing"
	"time"

	appsv1 "mini-k8s/pkg/apis/apps/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/util/intstr"
	"mini-k8s/pkg/util/ptr"
)

func TestResolveFenceposts(t *testing.T) {
	pct := func(s string) *intstr.IntOrString { v := intstr.FromString(s); return &v }
	num := func(n int) *intstr.IntOrString { v := intstr.FromInt(n); return &v }
	for _, tc := range []struct {
		surge, unavailable *intstr.IntOrString
		desired            int32
		wantSurge, wantU   int32
	}{
		{pct("25%"), pct("25%"), 3, 1, 0},
		{pct("25%"), pct("25%"), 10, 3, 2},
		{num(0), num(0), 3, 0, 1}, // never stuck
		{num(1), num(0), 4, 1, 0},
		{pct("0%"), pct("50%"), 3, 0, 1},
	} {
		s, u := ResolveFenceposts(tc.surge, tc.unavailable, tc.desired)
		if s != tc.wantSurge || u != tc.wantU {
			t.Errorf("%v/%v of %d: %d/%d, want %d/%d", tc.surge, tc.unavailable, tc.desired, s, u, tc.wantSurge, tc.wantU)
		}
	}
}

func deploymentWith(replicas int32, surge, unavailable intstr.IntOrString) *appsv1.Deployment {
	return &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Replicas: ptr.To(replicas), Strategy: appsv1.DeploymentStrategy{
		Type: appsv1.RollingUpdateDeploymentStrategyType, RollingUpdate: &appsv1.RollingUpdateDeployment{MaxSurge: &surge, MaxUnavailable: &unavailable}}}}
}

func rs(name string, size, available int32, created int) *appsv1.ReplicaSet {
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(time.Unix(int64(created), 0))},
		Spec:       appsv1.ReplicaSetSpec{Replicas: ptr.To(size)},
		Status:     appsv1.ReplicaSetStatus{Replicas: size, AvailableReplicas: available},
	}
}

// TestRollingUpdateSimulation runs the controller's arithmetic step by step
// against ReplicaSets whose Pods become available instantly, checking the
// surge and availability bounds at every step.
func TestRollingUpdateSimulation(t *testing.T) {
	for _, tc := range []struct {
		replicas           int32
		surge, unavailable intstr.IntOrString
	}{
		{4, intstr.FromInt(1), intstr.FromInt(0)},
		{4, intstr.FromString("25%"), intstr.FromString("25%")},
		{10, intstr.FromString("25%"), intstr.FromString("25%")},
		{3, intstr.FromInt(0), intstr.FromInt(1)},
		{5, intstr.FromString("100%"), intstr.FromInt(0)},
	} {
		name := fmt.Sprintf("%d/%v/%v", tc.replicas, tc.surge, tc.unavailable)
		d := deploymentWith(tc.replicas, tc.surge, tc.unavailable)
		surge, unavailable := ResolveFenceposts(&tc.surge, &tc.unavailable, tc.replicas)
		older := rs("older", 1, 1, 1) // leftovers of an earlier, unfinished rollout
		old := rs("old", tc.replicas-1, tc.replicas-1, 2)
		newRS := rs("new", 0, 0, 3)
		for step := 0; ; step++ {
			if step > 50 {
				t.Fatalf("%s: no convergence", name)
			}
			if total := specReplicas([]*appsv1.ReplicaSet{older, old, newRS}); total > tc.replicas+surge {
				t.Fatalf("%s step %d: %d Pods exceed replicas+maxSurge", name, step, total)
			}
			if avail := older.Status.AvailableReplicas + old.Status.AvailableReplicas + newRS.Status.AvailableReplicas; avail < tc.replicas-unavailable {
				t.Fatalf("%s step %d: only %d available", name, step, avail)
			}
			if replicas(newRS) == tc.replicas && replicas(old)+replicas(older) == 0 {
				break
			}
			// The controller grows the new ReplicaSet first...
			if grown := NewRSNewReplicas(d, []*appsv1.ReplicaSet{older, old, newRS}, newRS); grown != replicas(newRS) {
				newRS.Spec.Replicas = ptr.To(grown)
			} else {
				// ...then shrinks the old ones.
				for n, size := range OldScaleDown(d, newRS, []*appsv1.ReplicaSet{older, old}) {
					target := map[string]*appsv1.ReplicaSet{"old": old, "older": older}[n]
					target.Spec.Replicas = ptr.To(size)
				}
			}
			// The ReplicaSet controllers converge; new Pods become available.
			for _, r := range []*appsv1.ReplicaSet{older, old, newRS} {
				r.Status.Replicas, r.Status.AvailableReplicas = replicas(r), replicas(r)
			}
		}
	}
}

func TestOldScaleDownRemovesUnavailablePodsFirst(t *testing.T) {
	d := deploymentWith(4, intstr.FromInt(0), intstr.FromInt(1))
	newRS := rs("new", 0, 0, 3)
	broken := rs("broken", 2, 0, 1) // Pods never became available
	healthy := rs("healthy", 4, 4, 2)
	plan := OldScaleDown(d, newRS, []*appsv1.ReplicaSet{broken, healthy})
	if plan["broken"] != 0 || plan["healthy"] != 3 {
		t.Fatalf("plan: %v", plan)
	}
}
