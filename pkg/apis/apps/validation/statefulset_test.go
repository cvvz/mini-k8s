package validation

import (
	"strings"
	"testing"

	"mini-k8s/pkg/api/resource"
	appsv1 "mini-k8s/pkg/apis/apps/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/util/deepcopy"
	"mini-k8s/pkg/util/ptr"
)

func validStatefulSet() *appsv1.StatefulSet {
	labels := map[string]string{"app": "db"}
	s := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "db"}, Spec: appsv1.StatefulSetSpec{
		ServiceName: "db", Selector: &metav1.LabelSelector{MatchLabels: labels},
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "busybox"}}}},
		VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "data"},
			Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []string{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResources{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}}}},
	}}
	appsv1.SetDefaultsStatefulSet(s)
	return s
}

func expectStatefulSetError(t *testing.T, errs []string, want string) {
	t.Helper()
	for _, e := range errs {
		if strings.Contains(e, want) {
			return
		}
	}
	t.Errorf("errors %q lack %q", errs, want)
}

func TestValidateStatefulSet(t *testing.T) {
	s := validStatefulSet()
	// Containers may mount the claims of the volume templates.
	s.Spec.Template.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}
	if errs := ValidateStatefulSet(s); len(errs) != 0 {
		t.Fatalf("valid StatefulSet: %v", errs)
	}
	s.Spec.Template.Spec.Containers[0].VolumeMounts[0].Name = "logs"
	expectStatefulSetError(t, ValidateStatefulSet(s), `spec.template.spec.containers[0].volumeMounts[0].name: Not found: "logs"`)
	s.Spec.Template.Spec.Containers[0].VolumeMounts = nil
	if *s.Spec.Replicas != 1 || s.Spec.PodManagementPolicy != "OrderedReady" || *s.Spec.UpdateStrategy.RollingUpdate.Partition != 0 ||
		*s.Spec.RevisionHistoryLimit != 10 || s.Spec.PersistentVolumeClaimRetentionPolicy.WhenScaled != "Retain" ||
		*s.Spec.VolumeClaimTemplates[0].Spec.VolumeMode != corev1.PersistentVolumeFilesystem {
		t.Fatalf("defaults: %+v", s.Spec)
	}
	for _, tc := range []struct {
		change func(*appsv1.StatefulSet)
		want   string
	}{
		{func(s *appsv1.StatefulSet) { s.Spec.Replicas = ptr.To[int32](-1) }, "spec.replicas: Invalid value: -1: must be greater than or equal to 0"},
		{func(s *appsv1.StatefulSet) { s.Spec.PodManagementPolicy = "Sometimes" },
			`spec.podManagementPolicy: Invalid value: "Sometimes": must be 'OrderedReady' or 'Parallel'`},
		{func(s *appsv1.StatefulSet) { s.Spec.UpdateStrategy = appsv1.StatefulSetUpdateStrategy{Type: "Never"} },
			`spec.updateStrategy: Invalid value: {"Type":"Never","RollingUpdate":null}: must be 'RollingUpdate' or 'OnDelete'`},
		{func(s *appsv1.StatefulSet) { s.Spec.UpdateStrategy.RollingUpdate.Partition = ptr.To[int32](-2) },
			"spec.updateStrategy.rollingUpdate.partition: Invalid value: -2: must be greater than or equal to 0"},
		{func(s *appsv1.StatefulSet) { s.Spec.UpdateStrategy.Type = "OnDelete" },
			`spec.updateStrategy.rollingUpdate: Invalid value: {"Type":"OnDelete","RollingUpdate":{"Partition":0}}: only allowed for updateStrategy 'RollingUpdate'`},
		{func(s *appsv1.StatefulSet) { s.Spec.PersistentVolumeClaimRetentionPolicy.WhenDeleted = "Keep" },
			`spec.persistentVolumeClaimRetentionPolicy.whenDeleted: Unsupported value: "Keep": supported values: "Retain", "Delete"`},
		{func(s *appsv1.StatefulSet) { s.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyNever },
			`spec.template.spec.restartPolicy: Unsupported value: "Never": supported values: "Always"`},
		{func(s *appsv1.StatefulSet) { s.Spec.Template.Labels = map[string]string{"app": "other"} }, "selector does not match template labels"},
		{func(s *appsv1.StatefulSet) { s.Spec.VolumeClaimTemplates[0].Name = "" },
			"spec.volumeClaimTemplates[0].metadata.name: Required value"},
		{func(s *appsv1.StatefulSet) { s.Spec.VolumeClaimTemplates[0].Spec.AccessModes = nil },
			"spec.volumeClaimTemplates[0].spec.accessModes: Required value"},
		{func(s *appsv1.StatefulSet) { s.Spec.Ordinals = &appsv1.StatefulSetOrdinals{Start: -1} }, "spec.ordinals.start: Invalid value: -1"},
		{func(s *appsv1.StatefulSet) { s.Spec.ServiceName = "Bad_Name" }, `spec.serviceName: Invalid value: "Bad_Name"`},
	} {
		c := deepcopy.Copy(validStatefulSet())
		tc.change(c)
		expectStatefulSetError(t, ValidateStatefulSet(c), tc.want)
	}
}

func TestValidateStatefulSetUpdate(t *testing.T) {
	old := validStatefulSet()
	s := deepcopy.Copy(old)
	s.Spec.Replicas, s.Spec.MinReadySeconds, s.Spec.Ordinals = ptr.To[int32](5), 3, &appsv1.StatefulSetOrdinals{Start: 1}
	s.Spec.Template.Spec.Containers[0].Image = "nginx"
	s.Spec.UpdateStrategy.RollingUpdate.Partition = ptr.To[int32](2)
	s.Spec.PersistentVolumeClaimRetentionPolicy.WhenScaled = "Delete"
	if errs := ValidateStatefulSetUpdate(s, old); len(errs) != 0 {
		t.Fatalf("allowed changes: %v", errs)
	}
	for _, change := range []func(*appsv1.StatefulSet){
		func(s *appsv1.StatefulSet) { s.Spec.ServiceName = "other" },
		func(s *appsv1.StatefulSet) { s.Spec.PodManagementPolicy = "Parallel" },
		func(s *appsv1.StatefulSet) { s.Spec.VolumeClaimTemplates[0].Name = "logs" },
		func(s *appsv1.StatefulSet) { s.Spec.Selector.MatchLabels["tier"] = "x" },
	} {
		s := deepcopy.Copy(old)
		change(s)
		expectStatefulSetError(t, ValidateStatefulSetUpdate(s, old), "spec: Forbidden: updates to statefulset spec for fields other than "+
			"'replicas', 'ordinals', 'template', 'updateStrategy', 'revisionHistoryLimit', 'persistentVolumeClaimRetentionPolicy' "+
			"and 'minReadySeconds' are forbidden")
	}
}
