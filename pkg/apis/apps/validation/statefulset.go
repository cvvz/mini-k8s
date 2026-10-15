package validation

import (
	"encoding/json"
	"fmt"
	"strings"

	appsv1 "mini-k8s/pkg/apis/apps/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	corevalidation "mini-k8s/pkg/apis/core/validation"
	"mini-k8s/pkg/util/deepcopy"
	"mini-k8s/pkg/util/equality"
	"mini-k8s/pkg/util/validation"
)

// ValidateStatefulSet validates a complete StatefulSet, with Kubernetes'
// messages.
func ValidateStatefulSet(s *appsv1.StatefulSet) []string {
	var errs []string
	if r := s.Spec.Replicas; r != nil && *r < 0 {
		errs = append(errs, fmt.Sprintf("spec.replicas: Invalid value: %d: must be greater than or equal to 0", *r))
	}
	errs = append(errs, ValidateSelectorMatchesTemplate("spec", s.Spec.Selector, &s.Spec.Template)...)
	// Containers may mount the claims of the volume templates.
	tpl := deepcopy.Copy(&s.Spec.Template)
	for _, c := range s.Spec.VolumeClaimTemplates {
		tpl.Spec.Volumes = append(tpl.Spec.Volumes, corev1.Volume{Name: c.Name, VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: c.Name}}})
	}
	errs = append(errs, validatePodTemplate(tpl)...)
	if s.Spec.ServiceName != "" && !validation.IsDNS1123Label(s.Spec.ServiceName) {
		errs = append(errs, fmt.Sprintf("spec.serviceName: Invalid value: %q: a lowercase RFC 1123 label must consist of lower case "+
			"alphanumeric characters or '-', and must start and end with an alphanumeric character", s.Spec.ServiceName))
	}
	switch s.Spec.PodManagementPolicy {
	case appsv1.OrderedReadyPodManagement, appsv1.ParallelPodManagement:
	default:
		errs = append(errs, fmt.Sprintf("spec.podManagementPolicy: Invalid value: %q: must be 'OrderedReady' or 'Parallel'", s.Spec.PodManagementPolicy))
	}
	u := s.Spec.UpdateStrategy
	switch u.Type {
	case appsv1.RollingUpdateStatefulSetStrategyType:
		if ru := u.RollingUpdate; ru != nil && ru.Partition != nil && *ru.Partition < 0 {
			errs = append(errs, fmt.Sprintf("spec.updateStrategy.rollingUpdate.partition: Invalid value: %d: must be greater than or equal to 0", *ru.Partition))
		}
	case appsv1.OnDeleteStatefulSetStrategyType:
		if u.RollingUpdate != nil {
			errs = append(errs, fmt.Sprintf("spec.updateStrategy.rollingUpdate: Invalid value: %s: only allowed for updateStrategy 'RollingUpdate'",
				strategyString(u)))
		}
	default:
		errs = append(errs, fmt.Sprintf("spec.updateStrategy: Invalid value: %s: must be 'RollingUpdate' or 'OnDelete'", strategyString(u)))
	}
	if l := s.Spec.RevisionHistoryLimit; l != nil && *l < 0 {
		errs = append(errs, fmt.Sprintf("spec.revisionHistoryLimit: Invalid value: %d: must be greater than or equal to 0", *l))
	}
	if s.Spec.MinReadySeconds < 0 {
		errs = append(errs, fmt.Sprintf("spec.minReadySeconds: Invalid value: %d: must be greater than or equal to 0", s.Spec.MinReadySeconds))
	}
	if o := s.Spec.Ordinals; o != nil && o.Start < 0 {
		errs = append(errs, fmt.Sprintf("spec.ordinals.start: Invalid value: %d: must be greater than or equal to 0", o.Start))
	}
	if p := s.Spec.PersistentVolumeClaimRetentionPolicy; p != nil {
		for field, v := range map[string]string{"whenDeleted": p.WhenDeleted, "whenScaled": p.WhenScaled} {
			if v != appsv1.RetainPersistentVolumeClaimRetentionPolicyType && v != appsv1.DeletePersistentVolumeClaimRetentionPolicyType {
				errs = append(errs, fmt.Sprintf(`spec.persistentVolumeClaimRetentionPolicy.%s: Unsupported value: %q: supported values: "Retain", "Delete"`, field, v))
			}
		}
	}
	names := map[string]bool{}
	for i := range s.Spec.VolumeClaimTemplates {
		c := &s.Spec.VolumeClaimTemplates[i]
		path := fmt.Sprintf("spec.volumeClaimTemplates[%d]", i)
		switch {
		case c.Name == "":
			errs = append(errs, path+".metadata.name: Required value: name or generateName is required")
		case !validation.IsDNS1123Subdomain(c.Name):
			errs = append(errs, fmt.Sprintf("%s.metadata.name: Invalid value: %q: must be a lowercase RFC 1123 subdomain", path, c.Name))
		case names[c.Name]:
			errs = append(errs, fmt.Sprintf("%s.metadata.name: Duplicate value: %q", path, c.Name))
		}
		names[c.Name] = true
		for _, e := range corevalidation.ValidatePersistentVolumeClaim(c) {
			errs = append(errs, path+"."+e)
		}
	}
	return errs
}

// strategyString renders a strategy as Kubernetes prints its internal
// struct in messages.
func strategyString(u appsv1.StatefulSetUpdateStrategy) string {
	type rolling struct{ Partition int32 }
	v := struct {
		Type          string
		RollingUpdate *rolling
	}{Type: u.Type}
	if ru := u.RollingUpdate; ru != nil {
		v.RollingUpdate = &rolling{}
		if ru.Partition != nil {
			v.RollingUpdate.Partition = *ru.Partition
		}
	}
	data, _ := json.Marshal(v)
	return string(data)
}

// ValidateStatefulSetUpdate allows changes to the few fields Kubernetes
// allows.
func ValidateStatefulSetUpdate(s, old *appsv1.StatefulSet) []string {
	spec := deepcopy.Copy(&s.Spec)
	spec.Replicas, spec.Ordinals, spec.Template = old.Spec.Replicas, old.Spec.Ordinals, old.Spec.Template
	spec.UpdateStrategy, spec.RevisionHistoryLimit = old.Spec.UpdateStrategy, old.Spec.RevisionHistoryLimit
	spec.PersistentVolumeClaimRetentionPolicy, spec.MinReadySeconds = old.Spec.PersistentVolumeClaimRetentionPolicy, old.Spec.MinReadySeconds
	if !equality.Semantic(spec, &old.Spec) {
		return []string{"spec: Forbidden: updates to statefulset spec for fields other than " + strings.Join([]string{"'replicas'",
			"'ordinals'", "'template'", "'updateStrategy'", "'revisionHistoryLimit'", "'persistentVolumeClaimRetentionPolicy'"}, ", ") +
			" and 'minReadySeconds' are forbidden"}
	}
	return nil
}
