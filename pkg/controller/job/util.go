package job

import (
	"sort"
	"strconv"
	"strings"
	"time"

	batchv1 "mini-k8s/pkg/apis/batch/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/util/deepcopy"
)

// Backoff before replacing failed Pods: 10s doubling to 6 minutes.
const (
	podFailureBackoff    = 10 * time.Second
	maxPodFailureBackoff = 6 * time.Minute
)

// NewPod builds a Pod of the Job; index is its completion index for
// Indexed Jobs (-1 otherwise), which names the Pod, labels and annotates
// it, sets its hostname and the JOB_COMPLETION_INDEX variable.
func NewPod(j *batchv1.Job, index int) *corev1.Pod {
	tpl := deepcopy.Copy(&j.Spec.Template)
	labels, annotations := map[string]string{}, map[string]string{}
	for k, v := range tpl.Labels {
		labels[k] = v
	}
	for k, v := range tpl.Annotations {
		annotations[k] = v
	}
	name := j.Name + "-"
	spec := tpl.Spec
	if index >= 0 {
		i := strconv.Itoa(index)
		name += i + "-"
		labels[batchv1.JobCompletionIndexKey], annotations[batchv1.JobCompletionIndexKey] = i, i
		if spec.Hostname == "" {
			spec.Hostname = j.Name + "-" + i
		}
		env := corev1.EnvVar{Name: batchv1.JobCompletionIndexEnv, ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.labels['" + batchv1.JobCompletionIndexKey + "']"}}}
		for _, list := range [][]corev1.Container{spec.InitContainers, spec.Containers} {
			for k := range list {
				list[k].Env = append(list[k].Env, env)
			}
		}
	}
	if len(annotations) == 0 {
		annotations = nil
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{GenerateName: name, Namespace: j.Namespace, Labels: labels, Annotations: annotations,
			OwnerReferences: []metav1.OwnerReference{metav1.NewControllerRef(j)}},
		Spec: spec,
	}
}

// podIndex returns an Indexed Job Pod's completion index, or -1.
func podIndex(p *corev1.Pod) int {
	i, err := strconv.Atoi(p.Labels[batchv1.JobCompletionIndexKey])
	if err != nil {
		return -1
	}
	return i
}

// FormatIndexes lists indexes the way status.completedIndexes does:
// "1,3-5,7,8" (two neighbours with a comma, longer runs as ranges).
func FormatIndexes(indexes []int) string {
	sort.Ints(indexes)
	var b strings.Builder
	for i := 0; i < len(indexes); {
		j := i
		for j+1 < len(indexes) && indexes[j+1] <= indexes[j]+1 {
			j++
		}
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		first, last := indexes[i], indexes[j]
		b.WriteString(strconv.Itoa(first))
		switch {
		case last == first+1:
			b.WriteString("," + strconv.Itoa(last))
		case last > first+1:
			b.WriteString("-" + strconv.Itoa(last))
		}
		i = j + 1
	}
	return b.String()
}

// finishedAt is when a terminated Pod's last container ended.
func finishedAt(p *corev1.Pod) time.Time {
	var at time.Time
	for _, cs := range p.Status.ContainerStatuses {
		if t := cs.State.Terminated; t != nil && t.FinishedAt.After(at) {
			at = t.FinishedAt.Time
		}
	}
	if at.IsZero() {
		at = p.CreationTimestamp.Time
	}
	return at
}

// failureBackoff is how long after the last failure the next Pod waits.
func failureBackoff(failures int) time.Duration {
	if failures <= 0 {
		return 0
	}
	return min(podFailureBackoff<<min(failures-1, 10), maxPodFailureBackoff)
}

// rankForDeletion orders active Pods so the cheapest to lose come first:
// unscheduled, then not ready, then the newest.
func rankForDeletion(pods []*corev1.Pod) {
	score := func(p *corev1.Pod) int {
		switch {
		case p.Spec.NodeName == "":
			return 0
		case !corev1.IsPodReady(p):
			return 1
		}
		return 2
	}
	sort.SliceStable(pods, func(i, j int) bool {
		if si, sj := score(pods[i]), score(pods[j]); si != sj {
			return si < sj
		}
		return pods[i].CreationTimestamp.After(pods[j].CreationTimestamp.Time)
	})
}

// pastBackoffLimitOnFailure reports whether the containers of an
// OnFailure Job restarted in place as often as its backoffLimit allows.
func pastBackoffLimitOnFailure(j *batchv1.Job, active []*corev1.Pod) bool {
	if j.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyOnFailure {
		return false
	}
	restarts := int32(0)
	for _, p := range active {
		for _, list := range [][]corev1.ContainerStatus{p.Status.InitContainerStatuses, p.Status.ContainerStatuses} {
			for _, cs := range list {
				restarts += cs.RestartCount
			}
		}
	}
	if *j.Spec.BackoffLimit == 0 {
		return restarts > 0
	}
	return restarts >= *j.Spec.BackoffLimit
}

// recentFailures counts the failures since the last success and tells
// when the latest of them happened.
func recentFailures(succeeded, failed []*corev1.Pod) (int, time.Time) {
	var lastSuccess, lastFailure time.Time
	for _, p := range succeeded {
		if t := finishedAt(p); t.After(lastSuccess) {
			lastSuccess = t
		}
	}
	n := 0
	for _, p := range failed {
		if t := finishedAt(p); t.After(lastSuccess) {
			n++
			if t.After(lastFailure) {
				lastFailure = t
			}
		}
	}
	return n, lastFailure
}
