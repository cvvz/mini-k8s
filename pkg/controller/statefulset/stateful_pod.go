package statefulset

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"

	appsv1 "mini-k8s/pkg/apis/apps/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/util/deepcopy"
	"mini-k8s/pkg/util/ptr"
)

var ordinalPattern = regexp.MustCompile(`^(.*)-([0-9]+)$`)

// getOrdinal returns the ordinal of a set's Pod from its name, or -1.
func getOrdinal(set *appsv1.StatefulSet, p *corev1.Pod) int {
	m := ordinalPattern.FindStringSubmatch(p.Name)
	if m == nil || m[1] != set.Name {
		return -1
	}
	i, err := strconv.Atoi(m[2])
	if err != nil {
		return -1
	}
	return i
}

func podName(set *appsv1.StatefulSet, ordinal int) string {
	return fmt.Sprintf("%s-%d", set.Name, ordinal)
}

// claimName names the claim of a volume template for one Pod.
func claimName(set *appsv1.StatefulSet, tpl *corev1.PersistentVolumeClaim, ordinal int) string {
	return fmt.Sprintf("%s-%s-%d", tpl.Name, set.Name, ordinal)
}

// newPod builds the Pod of an ordinal from the set's template (the
// template of the given revision): its stable name, identity labels, host
// name under the governing Service and its claims as volumes.
func newPod(set *appsv1.StatefulSet, tpl *corev1.PodTemplateSpec, revision string, ordinal int) *corev1.Pod {
	t := deepcopy.Copy(tpl)
	name := podName(set, ordinal)
	labels := map[string]string{}
	for k, v := range t.Labels {
		labels[k] = v
	}
	labels[appsv1.StatefulSetPodNameLabel] = name
	labels[appsv1.PodIndexLabel] = strconv.Itoa(ordinal)
	labels[appsv1.ControllerRevisionHashLabelKey] = revision
	annotations := t.Annotations
	if len(annotations) == 0 {
		annotations = nil
	}
	spec := t.Spec
	spec.Hostname, spec.Subdomain = name, set.Spec.ServiceName
	for i := range set.Spec.VolumeClaimTemplates {
		c := &set.Spec.VolumeClaimTemplates[i]
		vol := corev1.Volume{Name: c.Name, VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claimName(set, c, ordinal)}}}
		replaced := false
		for j := range spec.Volumes {
			if spec.Volumes[j].Name == c.Name {
				spec.Volumes[j], replaced = vol, true
			}
		}
		if !replaced {
			spec.Volumes = append(spec.Volumes, vol)
		}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, GenerateName: set.Name + "-", Namespace: set.Namespace, Labels: labels,
			Annotations: annotations, OwnerReferences: []metav1.OwnerReference{metav1.NewControllerRef(set)}},
		Spec: spec,
	}
}

// identityMatches reports whether a Pod carries the identity labels of its
// ordinal (adopted Pods may not).
func identityMatches(set *appsv1.StatefulSet, p *corev1.Pod, ordinal int) bool {
	return p.Labels[appsv1.StatefulSetPodNameLabel] == podName(set, ordinal) && p.Labels[appsv1.PodIndexLabel] == strconv.Itoa(ordinal)
}

// newClaim builds the claim of a volume template for one Pod: the
// template's labels plus the set's selector labels.
func newClaim(set *appsv1.StatefulSet, tpl *corev1.PersistentVolumeClaim, ordinal int) *corev1.PersistentVolumeClaim {
	c := deepcopy.Copy(tpl)
	labels := map[string]string{}
	for k, v := range c.Labels {
		labels[k] = v
	}
	for k, v := range set.Spec.Selector.MatchLabels {
		labels[k] = v
	}
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: claimName(set, tpl, ordinal), Namespace: set.Namespace, Labels: labels,
			Annotations: c.Annotations},
		Spec: c.Spec,
	}
}

// claimOwnerRefs returns a claim's owner references as the retention
// policy wants them (like Kubernetes' updateClaimOwnerRefForSetAndPod):
// owned by the set when it is deleted with its claims, and by a scaled
// down Pod when claims go with such Pods. Other references are kept.
func claimOwnerRefs(set *appsv1.StatefulSet, pod *corev1.Pod, scaledDown bool, refs []metav1.OwnerReference) []metav1.OwnerReference {
	policy := set.Spec.PersistentVolumeClaimRetentionPolicy
	deleteWithSet := policy != nil && policy.WhenDeleted == appsv1.DeletePersistentVolumeClaimRetentionPolicyType
	deleteWhenScaled := policy != nil && policy.WhenScaled == appsv1.DeletePersistentVolumeClaimRetentionPolicyType
	wantSet := deleteWithSet && !(deleteWhenScaled && scaledDown)
	wantPod := deleteWhenScaled && scaledDown && pod != nil
	var out []metav1.OwnerReference
	for _, r := range refs {
		if r.UID != set.UID && (pod == nil || r.UID != pod.UID) {
			out = append(out, r)
		}
	}
	if wantSet {
		out = append(out, metav1.NewControllerRef(set))
	}
	if wantPod {
		out = append(out, metav1.OwnerReference{APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID})
	}
	return out
}

// sameOwners compares owner references ignoring their order.
func sameOwners(a, b []metav1.OwnerReference) bool {
	if len(a) != len(b) {
		return false
	}
	key := func(r metav1.OwnerReference) string {
		return fmt.Sprintf("%s/%s/%t", r.UID, r.Kind, ptr.Deref(r.Controller, false))
	}
	keys := func(refs []metav1.OwnerReference) []string {
		var out []string
		for _, r := range refs {
			out = append(out, key(r))
		}
		sort.Strings(out)
		return out
	}
	ka, kb := keys(a), keys(b)
	for i := range ka {
		if ka[i] != kb[i] {
			return false
		}
	}
	return true
}

func isTerminating(p *corev1.Pod) bool { return p.DeletionTimestamp != nil }

func isRunningAndReady(p *corev1.Pod) bool {
	return p.Status.Phase == corev1.PodRunning && corev1.IsPodReady(p)
}

func isHealthy(p *corev1.Pod) bool { return isRunningAndReady(p) && !isTerminating(p) }

func isFinished(p *corev1.Pod) bool {
	return p.Status.Phase == corev1.PodFailed || p.Status.Phase == corev1.PodSucceeded
}

func podRevision(p *corev1.Pod) string { return p.Labels[appsv1.ControllerRevisionHashLabelKey] }
