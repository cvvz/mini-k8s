// Package framework is the scheduling framework: a snapshot of the nodes
// with the Pods placed on them, Filter and Score plugin interfaces, and one
// scheduling cycle (filter every node, score the feasible ones, pick the
// best), like k8s.io/kubernetes/pkg/scheduler/framework.
package framework

import (
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"

	corev1 "mini-k8s/pkg/apis/core/v1"
)

// Resource is an amount of the resources the scheduler accounts for.
type Resource struct {
	MilliCPU, Memory, Pods int64
}

func (r *Resource) add(o Resource) {
	r.MilliCPU += o.MilliCPU
	r.Memory += o.Memory
	r.Pods += o.Pods
}

// Defaults used when scoring containers without requests (Kubernetes'
// non-zero requests), so that empty Pods still spread by load.
const (
	DefaultMilliCPURequest = 100
	DefaultMemoryRequest   = 200 << 20
)

// PodRequests is a Pod's effective request: the larger of the sum over its
// containers and its largest init container, per resource.
func PodRequests(p *corev1.Pod, nonZero bool) Resource {
	get := func(c corev1.Container) (int64, int64) {
		var cpu, mem int64
		if q, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
			cpu = q.MilliValue()
		} else if nonZero {
			cpu = DefaultMilliCPURequest
		}
		if q, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
			mem = q.Value()
		} else if nonZero {
			mem = DefaultMemoryRequest
		}
		return cpu, mem
	}
	r := Resource{Pods: 1}
	for _, c := range p.Spec.Containers {
		cpu, mem := get(c)
		r.MilliCPU += cpu
		r.Memory += mem
	}
	for _, c := range p.Spec.InitContainers {
		cpu, mem := get(c)
		r.MilliCPU, r.Memory = max(r.MilliCPU, cpu), max(r.Memory, mem)
	}
	return r
}

// Allocatable returns what a node offers.
func Allocatable(n *corev1.Node) Resource {
	a := n.Status.Allocatable
	r := Resource{}
	if q, ok := a[corev1.ResourceCPU]; ok {
		r.MilliCPU = q.MilliValue()
	}
	if q, ok := a[corev1.ResourceMemory]; ok {
		r.Memory = q.Value()
	}
	if q, ok := a[corev1.ResourcePods]; ok {
		r.Pods = q.Value()
	}
	return r
}

// NodeInfo is a node with the Pods placed on it and what they request.
type NodeInfo struct {
	Node *corev1.Node
	Pods []*corev1.Pod
	// Requested sums the Pods' requests; NonZeroRequested uses the
	// defaults for containers without requests (for scoring).
	Requested, NonZeroRequested Resource
}

// AddPod accounts for a Pod placed on the node.
func (n *NodeInfo) AddPod(p *corev1.Pod) {
	n.Pods = append(n.Pods, p)
	n.Requested.add(PodRequests(p, false))
	n.NonZeroRequested.add(PodRequests(p, true))
}

// Snapshot is the cluster as one scheduling cycle sees it.
type Snapshot struct {
	Nodes  []*NodeInfo
	byName map[string]*NodeInfo
}

// NewSnapshot places the Pods (bound or assumed, not finished) on the nodes.
func NewSnapshot(nodes []*corev1.Node, pods []*corev1.Pod) *Snapshot {
	s := &Snapshot{byName: map[string]*NodeInfo{}}
	for _, n := range nodes {
		info := &NodeInfo{Node: n}
		s.Nodes = append(s.Nodes, info)
		s.byName[n.Name] = info
	}
	sort.Slice(s.Nodes, func(i, j int) bool { return s.Nodes[i].Node.Name < s.Nodes[j].Node.Name })
	for _, p := range pods {
		if info := s.byName[p.Spec.NodeName]; info != nil && !corev1.IsPodTerminal(p) {
			info.AddPod(p)
		}
	}
	return s
}

// Get returns a node's info.
func (s *Snapshot) Get(name string) *NodeInfo { return s.byName[name] }

// FilterPlugin rejects nodes a Pod cannot run on, with the reasons
// (several, as "Insufficient cpu" and "Insufficient memory"); none means fit.
type FilterPlugin interface {
	Name() string
	Filter(pod *corev1.Pod, node *NodeInfo, snapshot *Snapshot) []string
}

// ScorePlugin ranks the feasible nodes from 0 (worst) to MaxScore.
type ScorePlugin interface {
	Name() string
	Weight() int64
	Score(pod *corev1.Pod, nodes []*NodeInfo, snapshot *Snapshot) map[string]int64
}

// MaxScore is the best score a plugin gives.
const MaxScore = 100

// Framework runs the plugins of a profile.
type Framework struct {
	Filters []FilterPlugin
	Scores  []ScorePlugin
	// Rand picks among nodes with the same best score (nil: math/rand).
	Rand func(n int) int
}

// Schedule returns the node for pod, or "" and the reason no node fits,
// like the scheduler's FailedScheduling message.
func (f *Framework) Schedule(pod *corev1.Pod, snapshot *Snapshot) (string, string) {
	var feasible []*NodeInfo
	reasons := map[string]int{}
	for _, node := range snapshot.Nodes {
		fits := true
		for _, plugin := range f.Filters {
			if failed := plugin.Filter(pod, node, snapshot); len(failed) > 0 {
				for _, r := range failed {
					reasons[r]++
				}
				fits = false
				break
			}
		}
		if fits {
			feasible = append(feasible, node)
		}
	}
	if len(feasible) == 0 {
		return "", UnschedulableMessage(len(snapshot.Nodes), reasons)
	}
	total := map[string]int64{}
	for _, plugin := range f.Scores {
		for name, score := range plugin.Score(pod, feasible, snapshot) {
			total[name] += score * plugin.Weight()
		}
	}
	var best []string
	for _, n := range feasible {
		switch {
		case len(best) == 0 || total[n.Node.Name] > total[best[0]]:
			best = []string{n.Node.Name}
		case total[n.Node.Name] == total[best[0]]:
			best = append(best, n.Node.Name)
		}
	}
	pick := rand.IntN
	if f.Rand != nil {
		pick = f.Rand
	}
	return best[pick(len(best))], ""
}

// UnschedulableMessage explains why no node fits, like the Kubernetes
// scheduler: "0/3 nodes are available: 1 node(s) had untolerated taint
// {node-role.kubernetes.io/control-plane: }, 2 Insufficient cpu."
func UnschedulableMessage(total int, reasons map[string]int) string {
	list := make([]string, 0, len(reasons))
	for reason, n := range reasons {
		list = append(list, fmt.Sprintf("%d %s", n, reason))
	}
	sort.Strings(list) // as FitError.Error does, count included
	if len(list) == 0 {
		return fmt.Sprintf("0/%d nodes are available.", total)
	}
	return fmt.Sprintf("0/%d nodes are available: %s.", total, strings.Join(list, ", "))
}

// NormalizeMax scales raw scores so the largest becomes MaxScore; with
// reverse, the smallest does (fewer is better).
func NormalizeMax(raw map[string]int64, reverse bool) map[string]int64 {
	var top int64
	for _, v := range raw {
		top = max(top, v)
	}
	out := make(map[string]int64, len(raw))
	for k, v := range raw {
		switch {
		case top == 0 && reverse:
			out[k] = MaxScore
		case top == 0:
			out[k] = 0
		case reverse:
			out[k] = MaxScore * (top - v) / top
		default:
			out[k] = MaxScore * v / top
		}
	}
	return out
}
