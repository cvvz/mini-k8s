package metricsserver

import (
	"sync"
	"time"
)

// usage is a rate computed from two samples.
type usage struct {
	nanoCores int64
	memory    int64
	at        time.Time
	window    time.Duration
}

// rate is the usage between two samples, or false when they do not belong
// to one running instance (a restart, or a clock going back).
func rate(prev, last point) (usage, bool) {
	window := last.at.Sub(prev.at)
	if window <= 0 || last.cpu < prev.cpu || !last.start.Equal(prev.start) {
		return usage{}, false
	}
	return usage{nanoCores: int64((last.cpu - prev.cpu) / window.Seconds() * 1e9), memory: int64(last.mem), at: last.at,
		window: window}, true
}

// storage keeps the previous and the last sample of every node and
// container; what a scrape no longer reports is forgotten.
type storage struct {
	mu                   sync.RWMutex
	prevNodes, lastNodes map[string]point
	prevPods, lastPods   map[podKey]map[string]point
}

func newStorage() *storage {
	return &storage{prevNodes: map[string]point{}, lastNodes: map[string]point{}, prevPods: map[podKey]map[string]point{},
		lastPods: map[podKey]map[string]point{}}
}

// store takes a new scrape: a newer sample pushes the last one to prev.
func (s *storage) store(b *batch) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prevNodes, lastNodes := map[string]point{}, map[string]point{}
	for name, p := range b.nodes {
		lastNodes[name] = p
		if old, ok := s.lastNodes[name]; ok && p.at.After(old.at) {
			prevNodes[name] = old
		} else if old, ok := s.prevNodes[name]; ok && p.at.After(old.at) {
			prevNodes[name] = old
		}
	}
	prevPods, lastPods := map[podKey]map[string]point{}, map[podKey]map[string]point{}
	for pod, containers := range b.pods {
		lastPods[pod], prevPods[pod] = containers, map[string]point{}
		for name, p := range containers {
			if old, ok := s.lastPods[pod][name]; ok && p.at.After(old.at) {
				prevPods[pod][name] = old
			} else if old, ok := s.prevPods[pod][name]; ok && p.at.After(old.at) {
				prevPods[pod][name] = old
			}
		}
	}
	s.prevNodes, s.lastNodes, s.prevPods, s.lastPods = prevNodes, lastNodes, prevPods, lastPods
}

func (s *storage) node(name string) (usage, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	prev, ok := s.prevNodes[name]
	if !ok {
		return usage{}, false
	}
	return rate(prev, s.lastNodes[name])
}

// containerUsage is the usage of one container of a Pod.
type containerUsage struct {
	name string
	usage
}

// pod returns the usage of every container of a Pod, or false unless all
// of them have two samples.
func (s *storage) pod(namespace, name string) ([]containerUsage, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	k := podKey{namespace, name}
	last := s.lastPods[k]
	if len(last) == 0 {
		return nil, false
	}
	var out []containerUsage
	for c, p := range last {
		prev, ok := s.prevPods[k][c]
		if !ok {
			return nil, false
		}
		u, ok := rate(prev, p)
		if !ok {
			return nil, false
		}
		out = append(out, containerUsage{c, u})
	}
	return out, true
}
