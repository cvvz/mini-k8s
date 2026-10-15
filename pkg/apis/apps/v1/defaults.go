package v1

import (
	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/util/intstr"
	"mini-k8s/pkg/util/ptr"
)

// SetDefaultsDeployment fills omitted fields with Kubernetes' defaults: one
// replica, a RollingUpdate strategy with 25% surge and 25% unavailability, ten
// old ReplicaSets kept and a 600-second progress deadline.
func SetDefaultsDeployment(d *Deployment) {
	if d.Spec.Replicas == nil {
		d.Spec.Replicas = ptr.To[int32](1)
	}
	if d.Spec.Strategy.Type == "" {
		d.Spec.Strategy.Type = RollingUpdateDeploymentStrategyType
	}
	if d.Spec.Strategy.Type == RollingUpdateDeploymentStrategyType {
		if d.Spec.Strategy.RollingUpdate == nil {
			d.Spec.Strategy.RollingUpdate = &RollingUpdateDeployment{}
		}
		ru := d.Spec.Strategy.RollingUpdate
		if ru.MaxUnavailable == nil {
			v := intstr.FromString("25%")
			ru.MaxUnavailable = &v
		}
		if ru.MaxSurge == nil {
			v := intstr.FromString("25%")
			ru.MaxSurge = &v
		}
	}
	if d.Spec.RevisionHistoryLimit == nil {
		d.Spec.RevisionHistoryLimit = ptr.To[int32](10)
	}
	if d.Spec.ProgressDeadlineSeconds == nil {
		d.Spec.ProgressDeadlineSeconds = ptr.To[int32](600)
	}
	corev1.SetDefaultsPodSpec(&d.Spec.Template.Spec)
}

// SetDefaultsReplicaSet fills omitted fields: one replica.
func SetDefaultsReplicaSet(rs *ReplicaSet) {
	if rs.Spec.Replicas == nil {
		rs.Spec.Replicas = ptr.To[int32](1)
	}
	corev1.SetDefaultsPodSpec(&rs.Spec.Template.Spec)
}

// SetDefaultsDaemonSet fills omitted fields: a RollingUpdate strategy
// replacing one node at a time without surge, and ten old revisions kept.
func SetDefaultsDaemonSet(ds *DaemonSet) {
	if ds.Spec.UpdateStrategy.Type == "" {
		ds.Spec.UpdateStrategy.Type = RollingUpdateDaemonSetStrategyType
	}
	if ds.Spec.UpdateStrategy.Type == RollingUpdateDaemonSetStrategyType {
		if ds.Spec.UpdateStrategy.RollingUpdate == nil {
			ds.Spec.UpdateStrategy.RollingUpdate = &RollingUpdateDaemonSet{}
		}
		ru := ds.Spec.UpdateStrategy.RollingUpdate
		if ru.MaxUnavailable == nil {
			v := intstr.FromInt(1)
			ru.MaxUnavailable = &v
		}
		if ru.MaxSurge == nil {
			v := intstr.FromInt(0)
			ru.MaxSurge = &v
		}
	}
	if ds.Spec.RevisionHistoryLimit == nil {
		ds.Spec.RevisionHistoryLimit = ptr.To[int32](10)
	}
	corev1.SetDefaultsPodSpec(&ds.Spec.Template.Spec)
}
