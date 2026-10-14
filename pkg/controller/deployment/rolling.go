package deployment

import (
	"sort"
	"strconv"

	appsv1 "mini-k8s/pkg/apis/apps/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/util/intstr"
)

// ResolveFenceposts turns maxSurge (rounded up) and maxUnavailable (rounded
// down) into Pod counts for desired replicas; if both are zero,
// maxUnavailable is one, so a rollout can always make progress.
func ResolveFenceposts(maxSurge, maxUnavailable *intstr.IntOrString, desired int32) (int32, int32) {
	surge, _ := intstr.GetScaledValue(maxSurge, int(desired), true)
	unavailable, _ := intstr.GetScaledValue(maxUnavailable, int(desired), false)
	if surge == 0 && unavailable == 0 {
		unavailable = 1
	}
	return int32(surge), int32(unavailable)
}

// fenceposts returns the Deployment's surge and unavailability bounds (both
// zero for Recreate).
func fenceposts(d *appsv1.Deployment) (surge, unavailable int32) {
	ru := d.Spec.Strategy.RollingUpdate
	if d.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType || ru == nil {
		return 0, 0
	}
	surge, unavailable = ResolveFenceposts(ru.MaxSurge, ru.MaxUnavailable, *d.Spec.Replicas)
	return surge, min(unavailable, *d.Spec.Replicas)
}

func replicas(rs *appsv1.ReplicaSet) int32 {
	if rs == nil || rs.Spec.Replicas == nil {
		return 0
	}
	return *rs.Spec.Replicas
}

// specReplicas sums spec.replicas over ReplicaSets (nil entries skipped).
func specReplicas(rss []*appsv1.ReplicaSet) int32 {
	n := int32(0)
	for _, rs := range rss {
		n += replicas(rs)
	}
	return n
}

// NewRSNewReplicas is the size the new ReplicaSet may grow to in this step of
// a rolling update: all Pods together stay within replicas + maxSurge.
func NewRSNewReplicas(d *appsv1.Deployment, all []*appsv1.ReplicaSet, newRS *appsv1.ReplicaSet) int32 {
	desired := *d.Spec.Replicas
	if d.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType {
		return desired
	}
	surge, _ := fenceposts(d)
	current := specReplicas(all)
	maxTotal := desired + surge
	if current >= maxTotal {
		return replicas(newRS)
	}
	scaleUp := min(maxTotal-current, desired-replicas(newRS))
	return replicas(newRS) + scaleUp
}

// byCreation orders ReplicaSets oldest first.
func byCreation(rss []*appsv1.ReplicaSet) {
	sort.SliceStable(rss, func(i, j int) bool {
		if !rss[i].CreationTimestamp.Equal(rss[j].CreationTimestamp.Time) {
			return rss[i].CreationTimestamp.Before(rss[j].CreationTimestamp.Time)
		}
		return rss[i].Name < rss[j].Name
	})
}

// OldScaleDown plans how far each old ReplicaSet (oldest first) shrinks in
// one step of a rolling update, keeping replicas - maxUnavailable Pods
// available: first Pods that are not available anyway, then available ones.
// It returns the new size of every old ReplicaSet that shrinks.
func OldScaleDown(d *appsv1.Deployment, newRS *appsv1.ReplicaSet, old []*appsv1.ReplicaSet) map[string]int32 {
	plan := map[string]int32{}
	size := func(rs *appsv1.ReplicaSet) int32 {
		if n, ok := plan[rs.Name]; ok {
			return n
		}
		return replicas(rs)
	}
	if specReplicas(old) == 0 {
		return plan
	}
	_, maxUnavailable := fenceposts(d)
	minAvailable := *d.Spec.Replicas - maxUnavailable
	all := append([]*appsv1.ReplicaSet{newRS}, old...)
	newUnavailable := replicas(newRS)
	if newRS != nil {
		newUnavailable -= newRS.Status.AvailableReplicas
	}
	maxScaledDown := specReplicas(all) - minAvailable - newUnavailable
	if maxScaledDown <= 0 {
		return plan
	}
	sorted := append([]*appsv1.ReplicaSet(nil), old...)
	byCreation(sorted)
	// Pods that are not available can go without reducing availability.
	cleaned := int32(0)
	for _, rs := range sorted {
		unhealthy := size(rs) - rs.Status.AvailableReplicas
		if cleaned >= maxScaledDown || size(rs) == 0 || unhealthy <= 0 {
			continue
		}
		n := min(maxScaledDown-cleaned, unhealthy)
		plan[rs.Name] = size(rs) - n
		cleaned += n
	}
	// Then available Pods, as long as minAvailable remain.
	available := int32(0)
	for _, rs := range all {
		if rs != nil {
			available += rs.Status.AvailableReplicas
		}
	}
	scaleDown := available - minAvailable
	done := int32(0)
	for _, rs := range sorted {
		if done >= scaleDown {
			break
		}
		if size(rs) == 0 {
			continue
		}
		n := min(size(rs), scaleDown-done)
		plan[rs.Name] = size(rs) - n
		done += n
	}
	return plan
}

// Revision returns an object's deployment.kubernetes.io/revision (0 if unset).
func Revision(obj metav1.Object) int64 {
	n, _ := strconv.ParseInt(obj.GetObjectMeta().Annotations[appsv1.RevisionAnnotation], 10, 64)
	return n
}
