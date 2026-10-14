package service

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/storage/etcd3"
	etcdtesting "mini-k8s/pkg/storage/etcd3/testing"
)

func TestRepairMigratesLegacyServicesAndReclaimsLeaks(t *testing.T) {
	ctx := context.Background()
	store, prefix := etcd3.New(etcdtesting.URL(t)), etcdtesting.Prefix(t)
	alloc := NewAllocators(store, prefix)
	repair := NewRepair(store, prefix, alloc)

	// A Service stored before cluster IPs existed, and one whose IP the
	// allocation record does not know about.
	legacy := corev1.Service{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: "default", UID: "u1"},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "web"}, Ports: []corev1.ServicePort{{Port: 80}}}}
	unrecorded := legacy
	unrecorded.Name, unrecorded.UID = "unrecorded", "u2"
	unrecorded.Spec.ClusterIP = "10.96.0.50"
	for _, s := range []corev1.Service{legacy, unrecorded} {
		data, _ := json.Marshal(s)
		if _, err := store.Create(ctx, prefix+"/services/default/"+s.Name, data, 0); err != nil {
			t.Fatal(err)
		}
	}
	// A leaked address: allocated, but no Service uses it.
	if err := alloc.IPs.Allocate(ctx, "10.96.0.77"); err != nil {
		t.Fatal(err)
	}

	if err := repair.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	kv, err := store.Get(ctx, prefix+"/services/default/legacy")
	if err != nil {
		t.Fatal(err)
	}
	var migrated corev1.Service
	if err := json.Unmarshal(kv.Value, &migrated); err != nil {
		t.Fatal(err)
	}
	if migrated.Spec.ClusterIP == "" || migrated.Spec.Type != corev1.ServiceTypeClusterIP || migrated.Spec.Ports[0].Protocol != corev1.ProtocolTCP {
		t.Fatalf("legacy Service not migrated: %+v", migrated.Spec)
	}
	ips, _ := alloc.IPs.Allocated(ctx)
	for _, ip := range []string{migrated.Spec.ClusterIP, "10.96.0.50", "10.96.0.77"} {
		if !ips[ip] {
			t.Errorf("%s should be allocated after one round: %v", ip, ips)
		}
	}
	// The leak is released only after it went unused for three rounds.
	for range 2 {
		if err := repair.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	ips, _ = alloc.IPs.Allocated(ctx)
	if ips["10.96.0.77"] || !ips["10.96.0.50"] || !ips[migrated.Spec.ClusterIP] {
		t.Fatalf("after three rounds only the leak is gone: %v", ips)
	}
}
