package kubelet

import (
	"context"
	"log"
	"reflect"
	"runtime"
	"time"

	coordinationv1 "mini-k8s/pkg/apis/coordination/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/util/ptr"
)

// Heartbeat timing, faster than Kubernetes' defaults (Lease renewed every
// 10s for 40s; status every 5m) so a laptop cluster reacts quickly.
const (
	// LeaseDurationSeconds is how long a renewal vouches for the node.
	LeaseDurationSeconds = 15
	// LeaseRenewInterval is how often the kubelet renews its Lease.
	LeaseRenewInterval = 2 * time.Second
	// StatusReportInterval is how often an unchanged Node status is
	// written anyway.
	StatusReportInterval = 10 * time.Second
)

func (k *Kubelet) heartbeatLoop(ctx context.Context) {
	for {
		node, err := k.syncNodeStatus(ctx)
		if err == nil {
			err = k.renewLease(ctx, node)
		}
		if err != nil && ctx.Err() == nil {
			log.Printf("kubelet: heartbeat: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(LeaseRenewInterval):
		}
	}
}

// renewLease renews kube-node-lease/<node>, the node's liveness signal,
// creating it (owned by the Node) on first use.
func (k *Kubelet) renewLease(ctx context.Context, node *corev1.Node) error {
	leases := client.Leases(k.client, corev1.NamespaceNodeLease)
	now := metav1.NowMicro()
	lease, err := leases.Get(ctx, k.cfg.NodeName)
	if client.IsNotFound(err) {
		_, err = leases.Create(ctx, &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: k.cfg.NodeName, Namespace: corev1.NamespaceNodeLease,
				OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Node", Name: node.Name, UID: node.UID}}},
			Spec: coordinationv1.LeaseSpec{HolderIdentity: ptr.To(k.cfg.NodeName),
				LeaseDurationSeconds: ptr.To[int32](LeaseDurationSeconds), RenewTime: &now},
		})
		return err
	}
	if err != nil {
		return err
	}
	lease.Spec.RenewTime = &now
	lease.Spec.HolderIdentity = ptr.To(k.cfg.NodeName)
	lease.Spec.LeaseDurationSeconds = ptr.To[int32](LeaseDurationSeconds)
	_, err = leases.Update(ctx, lease)
	return err
}

// syncNodeStatus registers the Node if needed and writes its status when
// it changed (including when the node lifecycle controller marked it
// Unknown) or every StatusReportInterval. Ready is True only when the
// runtime, its network plugin and the sandbox image are ready.
func (k *Kubelet) syncNodeStatus(ctx context.Context) (*corev1.Node, error) {
	nodes := client.Nodes(k.client)
	node, err := nodes.Get(ctx, k.cfg.NodeName)
	if client.IsNotFound(err) {
		labels := map[string]string{
			corev1.LabelHostname: k.cfg.NodeName, corev1.LabelOSStable: "linux", corev1.LabelArchStable: runtime.GOARCH,
		}
		// Registered not ready, as the kubelet does: the node lifecycle
		// controller lifts the taint once the node reports Ready.
		taints := []corev1.Taint{{Key: corev1.TaintNodeNotReady, Effect: corev1.TaintEffectNoSchedule}}
		if k.cfg.Role != "" {
			labels[corev1.LabelNodeRolePrefix+k.cfg.Role] = ""
		}
		if k.cfg.Role == "control-plane" {
			taints = append(taints, corev1.Taint{Key: corev1.TaintNodeRoleControlPlane, Effect: corev1.TaintEffectNoSchedule})
		}
		node, err = nodes.Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: k.cfg.NodeName, Labels: labels},
			Spec: corev1.NodeSpec{Taints: taints}})
	}
	if err != nil {
		return nil, err
	}
	readiness := k.runtime.Status(ctx, k.cfg.SandboxImage)
	now := metav1.Now()
	ready := corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionTrue, Reason: "KubeletReady",
		Message: "kubelet is posting ready status", LastHeartbeatTime: now, LastTransitionTime: now}
	if !readiness.Ready {
		ready.Status, ready.Reason, ready.Message = corev1.ConditionFalse, "KubeletNotReady", readiness.Message
	}
	prev := node.Status.Condition(corev1.NodeReady)
	if prev != nil && prev.Status == ready.Status {
		ready.LastTransitionTime = prev.LastTransitionTime
	}
	runtimeVersion, _ := k.runtime.Version(ctx)
	capacity := NodeCapacity("/proc/meminfo")
	status := corev1.NodeStatus{
		Capacity: capacity, Allocatable: capacity,
		Conditions: []corev1.NodeCondition{ready},
		Addresses:  []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: k.cfg.Address}, {Type: corev1.NodeHostName, Address: k.cfg.NodeName}},
		NodeInfo: corev1.NodeSystemInfo{KubeletVersion: Version, ContainerRuntimeVersion: runtimeVersion,
			OperatingSystem: "linux", Architecture: runtime.GOARCH},
	}
	if prev != nil && now.Sub(prev.LastHeartbeatTime.Time) < StatusReportInterval && sameNodeStatus(node.Status, status) {
		return node, nil
	}
	node.Status = status
	return nodes.UpdateStatus(ctx, node)
}

// sameNodeStatus compares statuses ignoring heartbeat times.
func sameNodeStatus(a, b corev1.NodeStatus) bool {
	strip := func(s corev1.NodeStatus) corev1.NodeStatus {
		s.Conditions = append([]corev1.NodeCondition(nil), s.Conditions...)
		for i := range s.Conditions {
			s.Conditions[i].LastHeartbeatTime = metav1.Time{}
		}
		return s
	}
	return reflect.DeepEqual(strip(a), strip(b))
}
