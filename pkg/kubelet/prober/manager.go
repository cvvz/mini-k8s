package prober

import (
	"context"
	"fmt"
	"sync"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/util/deepcopy"
)

// Recorder records Events about a Pod.
type Recorder interface {
	Eventf(obj metav1.Object, eventType, reason, format string, args ...any)
}

// Manager keeps one worker per running container instance and probe type.
type Manager struct {
	// Run performs one check; Executor.Run in production.
	Run func(ctx context.Context, pod *corev1.Pod, podIP string, c corev1.Container, p *corev1.Probe) (bool, string)
	// Changed is called when a readiness or startup result changes, so the
	// kubelet can report the new status.
	Changed func(pod *corev1.Pod)
	// Failed is called when a liveness or startup probe fails for good; the
	// kubelet stops the container, which then restarts per its policy.
	Failed   func(pod *corev1.Pod, containerID, message string)
	Recorder Recorder
	// Second is the length of a probe second (shortened in tests).
	Second time.Duration

	mu      sync.Mutex
	workers map[key]*worker
	results map[key]result
}

type key struct {
	uid, container string
	probe          ProbeType
}

type result struct {
	containerID string
	ok          bool
}

type worker struct {
	containerID string
	stop        chan struct{}
}

// NewManager returns a manager running checks with exec for exec probes.
func NewManager(exec ExecFunc, recorder Recorder) *Manager {
	return &Manager{Run: Executor{Exec: exec}.Run, Recorder: recorder, Second: time.Second,
		Changed: func(*corev1.Pod) {}, Failed: func(*corev1.Pod, string, string) {},
		workers: map[key]*worker{}, results: map[key]result{}}
}

// Sync starts workers for the probes of the Pod's running containers and
// stops those whose container instance is gone. The kubelet calls it with
// every status it computes.
func (m *Manager) Sync(pod *corev1.Pod, status corev1.PodStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range pod.Spec.Containers {
		var cs *corev1.ContainerStatus
		for i := range status.ContainerStatuses {
			if status.ContainerStatuses[i].Name == c.Name {
				cs = &status.ContainerStatuses[i]
			}
		}
		for t, p := range map[ProbeType]*corev1.Probe{Liveness: c.LivenessProbe, Readiness: c.ReadinessProbe, Startup: c.StartupProbe} {
			if p == nil {
				continue
			}
			k := key{pod.UID, c.Name, t}
			w := m.workers[k]
			if cs == nil || cs.State.Running == nil {
				if w != nil {
					close(w.stop)
					delete(m.workers, k)
				}
				continue
			}
			if w != nil && w.containerID == cs.ContainerID {
				continue
			}
			if w != nil {
				close(w.stop)
			}
			if r, ok := m.results[k]; ok && r.containerID != cs.ContainerID {
				delete(m.results, k) // a new instance starts unready and not started
			}
			w = &worker{containerID: cs.ContainerID, stop: make(chan struct{})}
			m.workers[k] = w
			go m.run(w, k, deepcopy.Copy(pod), status.PodIP, c, p, cs.State.Running.StartedAt.Time)
		}
	}
}

// Remove stops every worker of a Pod and forgets its results.
func (m *Manager) Remove(uid string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, w := range m.workers {
		if k.uid == uid {
			close(w.stop)
			delete(m.workers, k)
		}
	}
	for k := range m.results {
		if k.uid == uid {
			delete(m.results, k)
		}
	}
}

// Ready reports whether the readiness probe of the container instance has
// succeeded (and not failed since).
func (m *Manager) Ready(uid, container, containerID string) bool {
	return m.ok(key{uid, container, Readiness}, containerID)
}

// Started reports whether the startup probe of the container instance has
// succeeded.
func (m *Manager) Started(uid, container, containerID string) bool {
	return m.ok(key{uid, container, Startup}, containerID)
}

func (m *Manager) ok(k key, containerID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, found := m.results[k]
	return found && r.containerID == containerID && r.ok
}

// current reports whether w is still the worker for k.
func (m *Manager) current(w *worker, k key) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.workers[k] == w
}

// set records a result and reports whether it changed.
func (m *Manager) set(w *worker, k key, ok bool) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.workers[k] != w {
		return false // superseded
	}
	prev, found := m.results[k]
	m.results[k] = result{containerID: w.containerID, ok: ok}
	return !found || prev.ok != ok || prev.containerID != w.containerID
}

// run probes one container instance until stopped. Timing follows the
// probe: the first check after initialDelaySeconds from the container's
// start, then every periodSeconds; a result flips after successThreshold
// successes or failureThreshold failures in a row.
func (m *Manager) run(w *worker, k key, pod *corev1.Pod, podIP string, c corev1.Container, p *corev1.Probe, startedAt time.Time) {
	sleep := func(d time.Duration) bool {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-w.stop:
			return false
		case <-t.C:
			return true
		}
	}
	scale := float64(m.Second) / float64(time.Second)
	delay := time.Duration(float64(time.Until(startedAt.Add(time.Duration(p.InitialDelaySeconds)*time.Second))) * scale)
	if delay > 0 && !sleep(delay) {
		return
	}
	period := time.Duration(p.PeriodSeconds) * m.Second
	successes, failures := 0, 0
	for {
		// Liveness and readiness wait for the startup probe to pass.
		if k.probe == Startup || c.StartupProbe == nil || m.Started(k.uid, k.container, w.containerID) {
			ok, msg := m.Run(context.Background(), pod, podIP, c, p)
			if ok {
				successes, failures = successes+1, 0
			} else {
				successes, failures = 0, failures+1
				m.Recorder.Eventf(pod, corev1.EventTypeWarning, "Unhealthy", "%s probe failed: %s", k.probe, msg)
			}
			switch {
			case k.probe == Readiness && ok && successes >= int(p.SuccessThreshold),
				k.probe == Readiness && !ok && failures >= int(p.FailureThreshold):
				if m.set(w, k, ok) {
					m.Changed(pod)
				}
			case k.probe == Startup && ok:
				if m.set(w, k, true) {
					m.Changed(pod)
				}
				return // a started container is not probed again
			case k.probe != Readiness && !ok && failures >= int(p.FailureThreshold):
				if !m.current(w, k) {
					return // superseded while probing: the result is not this instance's
				}
				m.Failed(pod, w.containerID, fmt.Sprintf("Container %s failed %s probe, will be restarted",
					c.Name, map[ProbeType]string{Liveness: "liveness", Startup: "startup"}[k.probe]))
				// Normally the instance exits and Sync stops this worker. Should
				// stopping it fail, probe again after its grace period, so the
				// container is not left without liveness checks.
				grace := int64(30)
				if pod.Spec.TerminationGracePeriodSeconds != nil {
					grace = *pod.Spec.TerminationGracePeriodSeconds
				}
				if !sleep(time.Duration(grace) * m.Second) {
					return
				}
				successes, failures = 0, 0
			}
		}
		if !sleep(period) {
			return
		}
	}
}
