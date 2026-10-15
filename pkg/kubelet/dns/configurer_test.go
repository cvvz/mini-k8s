package dns

import (
	"reflect"
	"testing"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
)

func TestConfigurerFollowsDNSPolicy(t *testing.T) {
	c := &Configurer{ClusterDNS: []string{"10.96.0.10"}, ClusterDomain: "cluster.local",
		HostServers: []string{"172.18.0.3"}, HostSearches: []string{"corp.example"}}
	pod := func(policy string, hostNetwork bool, extra *corev1.PodDNSConfig) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "team"},
			Spec: corev1.PodSpec{DNSPolicy: policy, HostNetwork: hostNetwork, DNSConfig: extra}}
	}
	cluster := &runtimeapi.DNSConfig{Servers: []string{"10.96.0.10"},
		Searches: []string{"team.svc.cluster.local", "svc.cluster.local", "cluster.local", "corp.example"}, Options: []string{"ndots:5"}}
	host := &runtimeapi.DNSConfig{Servers: []string{"172.18.0.3"}, Searches: []string{"corp.example"}}
	two := "2"
	for name, tc := range map[string]struct {
		pod  *corev1.Pod
		want *runtimeapi.DNSConfig
	}{
		"ClusterFirst":                     {pod(corev1.DNSClusterFirst, false, nil), cluster},
		"ClusterFirst on the host network": {pod(corev1.DNSClusterFirst, true, nil), nil},
		"ClusterFirstWithHostNet":          {pod(corev1.DNSClusterFirstWithHostNet, true, nil), cluster},
		"Default":                          {pod(corev1.DNSDefault, false, nil), host},
		"None": {pod(corev1.DNSNone, false, &corev1.PodDNSConfig{Nameservers: []string{"1.1.1.1"}}),
			&runtimeapi.DNSConfig{Servers: []string{"1.1.1.1"}}},
		"merged dnsConfig": {pod(corev1.DNSClusterFirst, false, &corev1.PodDNSConfig{Nameservers: []string{"1.1.1.1"},
			Searches: []string{"cluster.local", "extra.example"}, Options: []corev1.PodDNSConfigOption{{Name: "ndots", Value: &two}, {Name: "edns0"}}}),
			&runtimeapi.DNSConfig{Servers: []string{"10.96.0.10", "1.1.1.1"},
				Searches: []string{"team.svc.cluster.local", "svc.cluster.local", "cluster.local", "corp.example", "extra.example"},
				Options:  []string{"ndots:2", "edns0"}}},
	} {
		if got := c.For(tc.pod); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %+v, want %+v", name, got, tc.want)
		}
	}
	noCluster := &Configurer{HostServers: []string{"172.18.0.3"}}
	if got := noCluster.For(pod(corev1.DNSClusterFirst, false, nil)); !reflect.DeepEqual(got.Servers, []string{"172.18.0.3"}) {
		t.Errorf("without a cluster DNS ClusterFirst uses the node's resolver: %+v", got)
	}
}
