package tainteviction

import (
	"testing"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
)

func TestEvictionTime(t *testing.T) {
	now := time.Unix(1000, 0)
	added := metav1.NewTime(now.Add(-100 * time.Second))
	unreachable := corev1.Taint{Key: corev1.TaintNodeUnreachable, Effect: corev1.TaintEffectNoExecute, TimeAdded: &added}
	soft := corev1.Taint{Key: "soft", Effect: corev1.TaintEffectNoSchedule}
	secs := func(s int64) *int64 { return &s }
	tolerate := func(key string, s *int64) corev1.Toleration {
		return corev1.Toleration{Key: key, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: s}
	}
	longAgo := metav1.NewTime(now.Add(-48 * time.Hour))
	for name, tc := range map[string]struct {
		taints      []corev1.Taint
		tolerations []corev1.Toleration
		at          time.Time
		evict       bool
	}{
		"no NoExecute taint":   {[]corev1.Taint{soft}, nil, time.Time{}, false},
		"untolerated":          {[]corev1.Taint{unreachable}, nil, now, true},
		"tolerated for good":   {[]corev1.Taint{unreachable}, []corev1.Toleration{tolerate(corev1.TaintNodeUnreachable, nil)}, time.Time{}, false},
		"tolerated for 300s":   {[]corev1.Taint{unreachable}, []corev1.Toleration{tolerate(corev1.TaintNodeUnreachable, secs(300))}, now.Add(200 * time.Second), true},
		"tolerated for 30s":    {[]corev1.Taint{unreachable}, []corev1.Toleration{tolerate(corev1.TaintNodeUnreachable, secs(30))}, now.Add(-70 * time.Second), true},
		"negative means now":   {[]corev1.Taint{unreachable}, []corev1.Toleration{tolerate(corev1.TaintNodeUnreachable, secs(-1))}, now.Add(-100 * time.Second), true},
		"another taint is not": {[]corev1.Taint{unreachable, {Key: "x", Effect: corev1.TaintEffectNoExecute}}, []corev1.Toleration{tolerate(corev1.TaintNodeUnreachable, nil)}, now, true},
		"earliest expiry wins": {[]corev1.Taint{unreachable, {Key: "x", Effect: corev1.TaintEffectNoExecute, TimeAdded: &added}},
			[]corev1.Toleration{tolerate(corev1.TaintNodeUnreachable, secs(300)), tolerate("x", secs(150))}, now.Add(50 * time.Second), true},
		// A taint tolerated for good, added long ago, does not shorten the
		// clock of a new one.
		"old permanent taint": {[]corev1.Taint{{Key: "dedicated", Effect: corev1.TaintEffectNoExecute, TimeAdded: &longAgo}, unreachable},
			[]corev1.Toleration{tolerate("dedicated", nil), tolerate(corev1.TaintNodeUnreachable, secs(300))}, now.Add(200 * time.Second), true},
	} {
		at, evict := EvictionTime(tc.taints, tc.tolerations, now, time.Time{})
		if evict != tc.evict || !at.Equal(tc.at) {
			t.Errorf("%s: got %v %v, want %v %v", name, at, evict, tc.at, tc.evict)
		}
	}
	// A taint left from before the controller started (say, across
	// cluster down/up) counts from the start, not from its old timeAdded.
	started := now.Add(-10 * time.Second)
	at, evict := EvictionTime([]corev1.Taint{{Key: corev1.TaintNodeUnreachable, Effect: corev1.TaintEffectNoExecute, TimeAdded: &longAgo}},
		[]corev1.Toleration{tolerate(corev1.TaintNodeUnreachable, secs(300))}, now, started)
	if !evict || !at.Equal(started.Add(300*time.Second)) {
		t.Errorf("stale taint: %v %v", at, evict)
	}
}
