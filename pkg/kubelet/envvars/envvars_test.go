package envvars

import (
	"strings"
	"testing"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
)

func TestForPodMatchesKubelet(t *testing.T) {
	svc := func(ns, name, ip string, ports ...corev1.ServicePort) *corev1.Service {
		return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: corev1.ServiceSpec{ClusterIP: ip, Ports: ports}}
	}
	services := []*corev1.Service{
		svc("default", "kubernetes", "10.96.0.1", corev1.ServicePort{Name: "https", Port: 443, Protocol: "TCP"}),
		svc("team", "my-web", "10.96.4.26",
			corev1.ServicePort{Name: "http", Port: 80, Protocol: "TCP"}, corev1.ServicePort{Name: "dns", Port: 53, Protocol: "UDP"}),
		svc("team", "headless", corev1.ClusterIPNone, corev1.ServicePort{Port: 80, Protocol: "TCP"}),
		svc("other", "elsewhere", "10.96.9.9", corev1.ServicePort{Port: 80, Protocol: "TCP"}),
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "team"}}
	var got []string
	for _, e := range ForPod(pod, services) {
		got = append(got, e.Name+"="+e.Value)
	}
	want := []string{
		"KUBERNETES_SERVICE_HOST=10.96.0.1", "KUBERNETES_SERVICE_PORT=443", "KUBERNETES_SERVICE_PORT_HTTPS=443",
		"KUBERNETES_PORT=tcp://10.96.0.1:443", "KUBERNETES_PORT_443_TCP=tcp://10.96.0.1:443",
		"KUBERNETES_PORT_443_TCP_PROTO=tcp", "KUBERNETES_PORT_443_TCP_PORT=443", "KUBERNETES_PORT_443_TCP_ADDR=10.96.0.1",
		"MY_WEB_SERVICE_HOST=10.96.4.26", "MY_WEB_SERVICE_PORT=80", "MY_WEB_SERVICE_PORT_HTTP=80", "MY_WEB_SERVICE_PORT_DNS=53",
		"MY_WEB_PORT=tcp://10.96.4.26:80", "MY_WEB_PORT_80_TCP=tcp://10.96.4.26:80", "MY_WEB_PORT_80_TCP_PROTO=tcp",
		"MY_WEB_PORT_80_TCP_PORT=80", "MY_WEB_PORT_80_TCP_ADDR=10.96.4.26", "MY_WEB_PORT_53_UDP=udp://10.96.4.26:53",
		"MY_WEB_PORT_53_UDP_PROTO=udp", "MY_WEB_PORT_53_UDP_PORT=53", "MY_WEB_PORT_53_UDP_ADDR=10.96.4.26",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	off := false
	pod.Spec.EnableServiceLinks = &off
	if got := ForPod(pod, services); len(got) != 8 || got[0].Name != "KUBERNETES_SERVICE_HOST" {
		t.Fatalf("without service links only the kubernetes Service remains: %v", got)
	}
}
