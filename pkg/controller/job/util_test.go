package job

import (
	"testing"
	"time"

	batchv1 "mini-k8s/pkg/apis/batch/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/util/ptr"
)

func TestFormatIndexes(t *testing.T) {
	for _, tc := range []struct {
		in   []int
		want string
	}{
		{nil, ""},
		{[]int{0}, "0"},
		{[]int{1, 0}, "0,1"},
		{[]int{0, 1, 2}, "0-2"},
		{[]int{7, 1, 3, 4, 5, 8}, "1,3-5,7,8"},
		{[]int{2, 2, 3}, "2,3"},
	} {
		if got := FormatIndexes(tc.in); got != tc.want {
			t.Errorf("%v: %q, want %q", tc.in, got, tc.want)
		}
	}
}

func testJob() *batchv1.Job {
	j := &batchv1.Job{TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{Name: "work", Namespace: "ns", UID: "uid-1"},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "w"}},
			Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever,
				InitContainers: []corev1.Container{{Name: "init", Image: "busybox"}},
				Containers:     []corev1.Container{{Name: "c", Image: "busybox"}}}}}}
	batchv1.SetDefaultsJobSpec(&j.Spec)
	return j
}

func TestNewPod(t *testing.T) {
	j := testJob()
	p := NewPod(j, -1)
	if p.GenerateName != "work-" || p.Namespace != "ns" || p.Labels["app"] != "w" || p.Annotations != nil || p.Spec.Hostname != "" {
		t.Fatalf("non-indexed Pod: %+v", p.ObjectMeta)
	}
	if ref := metav1.ControllerOf(&p.ObjectMeta); ref == nil || ref.UID != "uid-1" || ref.Kind != "Job" {
		t.Fatalf("owner: %+v", p.OwnerReferences)
	}
	if len(p.Spec.Containers[0].Env) != 0 {
		t.Fatal("non-indexed Pods get no index")
	}

	p = NewPod(j, 3)
	if p.GenerateName != "work-3-" || p.Labels[batchv1.JobCompletionIndexKey] != "3" ||
		p.Annotations[batchv1.JobCompletionIndexKey] != "3" || p.Spec.Hostname != "work-3" {
		t.Fatalf("indexed Pod: %+v %s", p.ObjectMeta, p.Spec.Hostname)
	}
	for _, c := range append(p.Spec.InitContainers, p.Spec.Containers...) {
		if len(c.Env) != 1 || c.Env[0].Name != "JOB_COMPLETION_INDEX" ||
			c.Env[0].ValueFrom.FieldRef.FieldPath != "metadata.labels['batch.kubernetes.io/job-completion-index']" {
			t.Fatalf("%s env: %+v", c.Name, c.Env)
		}
	}
	if len(j.Spec.Template.Spec.Containers[0].Env) != 0 || j.Spec.Template.Labels[batchv1.JobCompletionIndexKey] != "" {
		t.Fatal("the template must not change")
	}
	if podIndex(p) != 3 || podIndex(NewPod(j, -1)) != -1 {
		t.Fatal("podIndex")
	}
}

func TestFailureBackoff(t *testing.T) {
	for n, want := range map[int]time.Duration{0: 0, 1: 10 * time.Second, 2: 20 * time.Second, 4: 80 * time.Second,
		6: 320 * time.Second, 7: 6 * time.Minute, 60: 6 * time.Minute} {
		if got := failureBackoff(n); got != want {
			t.Errorf("%d failures: %v, want %v", n, got, want)
		}
	}
}

func terminated(phase string, at time.Time) *corev1.Pod {
	return &corev1.Pod{Status: corev1.PodStatus{Phase: phase, ContainerStatuses: []corev1.ContainerStatus{
		{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{FinishedAt: metav1.Time{Time: at}}}}}}}
}

func TestRecentFailures(t *testing.T) {
	base := time.Date(2026, 10, 15, 8, 0, 0, 0, time.UTC)
	failed := []*corev1.Pod{terminated(corev1.PodFailed, base), terminated(corev1.PodFailed, base.Add(2*time.Minute)),
		terminated(corev1.PodFailed, base.Add(time.Minute))}
	if n, last := recentFailures(nil, failed); n != 3 || !last.Equal(base.Add(2*time.Minute)) {
		t.Fatalf("no success: %d %v", n, last)
	}
	// A success resets the count: only later failures delay new Pods.
	ok := []*corev1.Pod{terminated(corev1.PodSucceeded, base.Add(90*time.Second))}
	if n, last := recentFailures(ok, failed); n != 1 || !last.Equal(base.Add(2*time.Minute)) {
		t.Fatalf("after a success: %d %v", n, last)
	}
}

func TestPastBackoffLimitOnFailure(t *testing.T) {
	j := testJob()
	restarted := func(n int32) []*corev1.Pod {
		return []*corev1.Pod{{Status: corev1.PodStatus{Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{RestartCount: n}}}}}
	}
	j.Spec.BackoffLimit = ptr.To[int32](2)
	if pastBackoffLimitOnFailure(j, restarted(5)) {
		t.Fatal("Never: restarts do not count")
	}
	j.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyOnFailure
	if pastBackoffLimitOnFailure(j, restarted(1)) || !pastBackoffLimitOnFailure(j, restarted(2)) {
		t.Fatal("OnFailure: the limit is reached at backoffLimit restarts")
	}
	j.Spec.BackoffLimit = ptr.To[int32](0)
	if pastBackoffLimitOnFailure(j, restarted(0)) || !pastBackoffLimitOnFailure(j, restarted(1)) {
		t.Fatal("a limit of 0 tolerates no restart")
	}
}

func TestRankForDeletion(t *testing.T) {
	now := time.Now()
	pod := func(name, node string, ready bool, age time.Duration) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.Time{Time: now.Add(-age)}},
			Spec: corev1.PodSpec{NodeName: node}}
		if ready {
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		}
		return p
	}
	pods := []*corev1.Pod{pod("ready-old", "n", true, time.Hour), pod("ready-new", "n", true, time.Minute),
		pod("unready", "n", false, time.Hour), pod("pending", "", false, time.Hour)}
	rankForDeletion(pods)
	var got []string
	for _, p := range pods {
		got = append(got, p.Name)
	}
	if want := []string{"pending", "unready", "ready-new", "ready-old"}; len(got) != 4 || got[0] != want[0] ||
		got[1] != want[1] || got[2] != want[2] || got[3] != want[3] {
		t.Fatalf("order %v, want %v", got, want)
	}
}
