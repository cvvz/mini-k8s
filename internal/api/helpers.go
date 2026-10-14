package api

import "time"

// NodeHeartbeatGrace is how stale a Ready heartbeat may be before the node is
// treated as not ready by consumers that read Node status directly.
const NodeHeartbeatGrace = 20 * time.Second

// IsNodeReady reports whether the node's Ready condition is True and fresh.
func IsNodeReady(n *Node, now time.Time) bool {
	c := n.Status.Condition(NodeReady)
	return c != nil && c.Status == ConditionTrue && now.Sub(c.LastHeartbeatTime.Time) <= NodeHeartbeatGrace
}

// IsPodTerminal reports whether the Pod has finished for good.
func IsPodTerminal(p *Pod) bool {
	return p.Status.Phase == PodSucceeded || p.Status.Phase == PodFailed
}
