package service

import (
	"context"
	"testing"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/registry/core/service/allocator"
	"mini-k8s/pkg/storage/etcd3"
	etcdtesting "mini-k8s/pkg/storage/etcd3/testing"
)

// A dynamic node port must never take the value a later port of the same
// Service asks for explicitly.
func TestExplicitNodePortsAreReservedBeforeDynamicOnes(t *testing.T) {
	ctx := context.Background()
	store, prefix := etcd3.New(etcdtesting.URL(t)), etcdtesting.Prefix(t)
	ips, err := allocator.NewIPs(store, prefix+"/ranges/serviceips", DefaultServiceCIDR)
	if err != nil {
		t.Fatal(err)
	}
	// Two node ports only: with the wrong order the dynamic pick would
	// collide half of the time.
	a := &Allocators{IPs: ips, Ports: allocator.NewPorts(store, prefix+"/ranges/servicenodeports", 30000, 30001)}
	for range 10 {
		spec := &corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort, Ports: []corev1.ServicePort{
			{Name: "a", Port: 80, Protocol: corev1.ProtocolTCP},
			{Name: "b", Port: 81, Protocol: corev1.ProtocolTCP, NodePort: 30001},
		}}
		finish, err := a.allocate(ctx, spec, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		if spec.Ports[0].NodePort != 30000 || spec.Ports[1].NodePort != 30001 {
			t.Fatalf("node ports %d and %d", spec.Ports[0].NodePort, spec.Ports[1].NodePort)
		}
		finish(ctx, false) // release both for the next round
	}
}
