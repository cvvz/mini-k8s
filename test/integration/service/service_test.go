// Package service tests cluster IP and node port allocation by the API
// server and the Endpoints controller against a real etcd.
package service

import (
	"context"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/controller/endpoint"
	"mini-k8s/pkg/util/deepcopy"
	"mini-k8s/pkg/util/intstr"
	"mini-k8s/test/integration/framework"
)

func newService(name string, mutate func(*corev1.Service)) *corev1.Service {
	s := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.ServiceSpec{Selector: map[string]string{"app": name}, Ports: []corev1.ServicePort{{Port: 80}}}}
	if mutate != nil {
		mutate(s)
	}
	return s
}

func expectInvalid(t *testing.T, err error, want string) {
	t.Helper()
	if !client.IsInvalid(err) || !strings.Contains(err.Error(), want) {
		t.Fatalf("want Invalid with %q, got %v", want, err)
	}
}

func TestClusterIPAllocation(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx := context.Background()
	svcs := client.Services(c, "default")
	_, cidr, _ := net.ParseCIDR("10.96.0.0/12")
	_, static, _ := net.ParseCIDR("10.96.0.0/24")

	var mu sync.Mutex
	seen := map[string]bool{}
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			s, err := svcs.Create(ctx, newService("s"+string(rune('a'+i)), nil))
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			ip := net.ParseIP(s.Spec.ClusterIP)
			if seen[s.Spec.ClusterIP] || !cidr.Contains(ip) || static.Contains(ip) {
				t.Errorf("bad or duplicate dynamic cluster IP %s", s.Spec.ClusterIP)
			}
			seen[s.Spec.ClusterIP] = true
		})
	}
	wg.Wait()

	pinned, err := svcs.Create(ctx, newService("pinned", func(s *corev1.Service) { s.Spec.ClusterIP = "10.96.0.200" }))
	if err != nil || pinned.Spec.ClusterIP != "10.96.0.200" || pinned.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Fatalf("explicit cluster IP: %v %+v", err, pinned)
	}
	_, err = svcs.Create(ctx, newService("dup", func(s *corev1.Service) { s.Spec.ClusterIP = "10.96.0.200" }))
	expectInvalid(t, err, `spec.clusterIP: Invalid value: "10.96.0.200": failed to allocate IP 10.96.0.200: provided IP is already allocated`)
	_, err = svcs.Create(ctx, newService("outside", func(s *corev1.Service) { s.Spec.ClusterIP = "10.112.0.5" }))
	expectInvalid(t, err, "failed to allocate IP 10.112.0.5: the provided network does not match the current range")

	// An update that leaves the cluster IP out keeps it; changing it is rejected.
	update := deepcopy.Copy(pinned)
	update.Spec.ClusterIP = ""
	update.Spec.Ports[0].Port = 8080
	updated, err := svcs.Update(ctx, update)
	if err != nil || updated.Spec.ClusterIP != "10.96.0.200" {
		t.Fatalf("update without clusterIP: %v %+v", err, updated)
	}
	updated.Spec.ClusterIP = "10.96.0.201"
	_, err = svcs.Update(ctx, updated)
	expectInvalid(t, err, `spec.clusterIP: Invalid value: "10.96.0.201": may not change once set`)

	// Deleting the Service frees its address.
	if err := svcs.Delete(ctx, "pinned", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := svcs.Create(ctx, newService("again", func(s *corev1.Service) { s.Spec.ClusterIP = "10.96.0.200" })); err != nil {
		t.Fatalf("released address not reusable: %v", err)
	}
	// A failed write releases what it reserved: the name is taken, the IP stays free.
	_, err = svcs.Create(ctx, newService("again", func(s *corev1.Service) { s.Spec.ClusterIP = "10.96.0.202" }))
	if !client.IsAlreadyExists(err) {
		t.Fatalf("want AlreadyExists, got %v", err)
	}
	if _, err := svcs.Create(ctx, newService("third", func(s *corev1.Service) { s.Spec.ClusterIP = "10.96.0.202" })); err != nil {
		t.Fatalf("address reserved by a failed create leaked: %v", err)
	}
}

func TestNodePortAllocation(t *testing.T) {
	c, reg := framework.StartTestServerWithRegistry(t)
	ctx := context.Background()
	svcs := client.Services(c, "default")
	nodePort := func(name string, port int32) *corev1.Service {
		return newService(name, func(s *corev1.Service) {
			s.Spec.Type = corev1.ServiceTypeNodePort
			s.Spec.Ports[0].NodePort = port
		})
	}
	auto, err := svcs.Create(ctx, nodePort("auto", 0))
	if err != nil || auto.Spec.Ports[0].NodePort < 30086 || auto.Spec.Ports[0].NodePort > 32767 {
		t.Fatalf("dynamic node port: %v %+v", err, auto)
	}
	if _, err := svcs.Create(ctx, nodePort("fixed", 30010)); err != nil {
		t.Fatal(err)
	}
	_, err = svcs.Create(ctx, nodePort("dup", 30010))
	expectInvalid(t, err, "spec.ports[0].nodePort: Invalid value: 30010: provided port is already allocated")
	_, err = svcs.Create(ctx, nodePort("range", 40000))
	expectInvalid(t, err, "spec.ports[0].nodePort: Invalid value: 40000: provided port is not in the valid range. The range of valid ports is 30000-32767")
	_, err = svcs.Create(ctx, newService("clusterip", func(s *corev1.Service) { s.Spec.Ports[0].NodePort = 30011 }))
	expectInvalid(t, err, "spec.ports[0].nodePort: Forbidden: may not be used when `type` is 'ClusterIP'")

	// Re-applying the manifest (a merge patch without nodePort) keeps the port.
	patched, err := svcs.Patch(ctx, "auto", map[string]any{"spec": map[string]any{"ports": []any{map[string]any{"port": 80}}}})
	if err != nil || patched.Spec.Ports[0].NodePort != auto.Spec.Ports[0].NodePort {
		t.Fatalf("patch without nodePort: %v %+v", err, patched)
	}
	// Switching to ClusterIP drops and releases the node port.
	switched, err := svcs.Patch(ctx, "fixed", map[string]any{"spec": map[string]any{"type": "ClusterIP"}})
	if err != nil || switched.Spec.Ports[0].NodePort != 0 {
		t.Fatalf("switch to ClusterIP: %v %+v", err, switched)
	}
	if _, err := svcs.Create(ctx, nodePort("reuse", 30010)); err != nil {
		t.Fatalf("released node port not reusable: %v", err)
	}
	// A dry run allocates nothing.
	if err := c.Do(ctx, "POST", "/api/v1/namespaces/default/services", map[string][]string{"dryRun": {"All"}}, "",
		nodePort("dry", 30020), nil); err != nil {
		t.Fatal(err)
	}
	ports, err := reg.ServiceAllocators.Ports.Allocated(ctx)
	if err != nil || ports[30020] {
		t.Fatalf("dry run reserved a port: %v %v", err, ports)
	}
	// Deleting releases.
	if err := svcs.Delete(ctx, "reuse", nil); err != nil {
		t.Fatal(err)
	}
	if ports, _ := reg.ServiceAllocators.Ports.Allocated(ctx); ports[30010] {
		t.Fatal("node port of a deleted Service still allocated")
	}
}

// startEndpointsController runs the controller with its informers.
func startEndpointsController(t *testing.T, c *client.Client) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	services := informers.New(client.Services(c, ""), client.ListOptions{}, 0)
	pods := informers.New(client.Pods(c, ""), client.ListOptions{}, 0)
	endpoints := informers.New(client.Endpoints(c, ""), client.ListOptions{}, 0)
	ec := endpoint.NewController(c, services, pods, endpoints)
	for _, run := range []func(context.Context){services.Run, pods.Run, endpoints.Run} {
		go run(ctx)
	}
	informers.WaitForSync(ctx, services.HasSynced, pods.HasSynced, endpoints.HasSynced)
	go ec.Run(ctx, 2)
}

// runningPod creates a Pod on a node with an IP, ready or not.
func runningPod(t *testing.T, c *client.Client, name, ip string, ready bool, labels map[string]string, ports ...corev1.ContainerPort) *corev1.Pod {
	t.Helper()
	ctx := context.Background()
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Spec: corev1.PodSpec{NodeName: "n1", Containers: []corev1.Container{{Name: "c", Image: "img", Ports: ports}}}}
	p, err := client.Pods(c, "default").Create(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	return setReady(t, c, p, ip, ready)
}

func setReady(t *testing.T, c *client.Client, p *corev1.Pod, ip string, ready bool) *corev1.Pod {
	t.Helper()
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	p.Status.Phase, p.Status.PodIP = corev1.PodRunning, ip
	p.Status.SetCondition(corev1.PodCondition{Type: corev1.PodReady, Status: status})
	p, err := client.Pods(c, "default").UpdateStatus(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

type epView struct{ Ready, NotReady, Ports string }

func view(e *corev1.Endpoints) []epView {
	var out []epView
	for _, ss := range e.Subsets {
		var v epView
		for _, a := range ss.Addresses {
			v.Ready += a.IP + "(" + a.TargetRef.Name + ") "
		}
		for _, a := range ss.NotReadyAddresses {
			v.NotReady += a.IP + " "
		}
		for _, p := range ss.Ports {
			v.Ports += p.Name + ":" + strconv.Itoa(int(p.Port)) + " "
		}
		out = append(out, v)
	}
	return out
}

func waitEndpoints(t *testing.T, c *client.Client, name string, want []epView) *corev1.Endpoints {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last any
	for time.Now().Before(deadline) {
		e, err := client.Endpoints(c, "default").Get(context.Background(), name)
		if err == nil && reflect.DeepEqual(view(e), want) {
			return e
		}
		if err == nil {
			last = view(e)
		} else {
			last = err
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("Endpoints %s: want %+v, last %+v", name, want, last)
	return nil
}

func TestEndpointsFollowPodsAndReadiness(t *testing.T) {
	c := framework.StartTestServer(t)
	startEndpointsController(t, c)
	ctx := context.Background()
	labels := map[string]string{"app": "web"}
	http := corev1.ContainerPort{Name: "http", ContainerPort: 8081}
	svc := newService("web", func(s *corev1.Service) {
		s.Labels = map[string]string{"team": "a"}
		s.Spec.Ports = []corev1.ServicePort{
			{Name: "http", Port: 80, TargetPort: intstr.FromString("http")},
			{Name: "metrics", Port: 9090, TargetPort: intstr.FromInt(9092)},
		}
	})
	if _, err := client.Services(c, "default").Create(ctx, svc); err != nil {
		t.Fatal(err)
	}
	a := runningPod(t, c, "a", "10.244.1.5", true, labels, http)
	b := runningPod(t, c, "b", "10.244.2.5", false, labels, http)
	runningPod(t, c, "noname", "10.244.1.6", true, labels)                                 // no "http" port: only metrics
	runningPod(t, c, "other", "10.244.1.7", true, map[string]string{"app": "other"}, http) // not selected
	e := waitEndpoints(t, c, "web", []epView{
		{Ready: "10.244.1.5(a) ", NotReady: "10.244.2.5 ", Ports: "http:8081 metrics:9092 "},
		{Ready: "10.244.1.6(noname) ", Ports: "metrics:9092 "},
	})
	if e.Labels["team"] != "a" || e.Labels[endpoint.ManagedByLabel] != endpoint.ManagedByValue {
		t.Fatalf("labels: %v", e.Labels)
	}
	if *e.Subsets[0].Addresses[0].NodeName != "n1" || e.Subsets[0].Addresses[0].TargetRef.UID != a.UID {
		t.Fatalf("address details: %+v", e.Subsets[0].Addresses[0])
	}

	setReady(t, c, b, "10.244.2.5", true)
	waitEndpoints(t, c, "web", []epView{
		{Ready: "10.244.1.5(a) 10.244.2.5(b) ", Ports: "http:8081 metrics:9092 "},
		{Ready: "10.244.1.6(noname) ", Ports: "metrics:9092 "},
	})
	// Terminating Pods leave at once.
	if err := client.Pods(c, "default").Delete(ctx, "a", nil); err != nil {
		t.Fatal(err)
	}
	waitEndpoints(t, c, "web", []epView{
		{Ready: "10.244.2.5(b) ", Ports: "http:8081 metrics:9092 "},
		{Ready: "10.244.1.6(noname) ", Ports: "metrics:9092 "},
	})

	// The Endpoints go away with the Service.
	if err := client.Services(c, "default").Delete(ctx, "web", nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err := client.Endpoints(c, "default").Get(ctx, "web")
		if client.IsNotFound(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Endpoints not deleted: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestServicesWithoutSelectorKeepUserEndpoints(t *testing.T) {
	c := framework.StartTestServer(t)
	startEndpointsController(t, c)
	ctx := context.Background()
	external := newService("external", func(s *corev1.Service) { s.Spec.Selector = nil })
	if _, err := client.Services(c, "default").Create(ctx, external); err != nil {
		t.Fatal(err)
	}
	manual := &corev1.Endpoints{ObjectMeta: metav1.ObjectMeta{Name: "external"}, Subsets: []corev1.EndpointSubset{{
		Addresses: []corev1.EndpointAddress{{IP: "192.168.1.10"}}, Ports: []corev1.EndpointPort{{Port: 80}}}}}
	if _, err := client.Endpoints(c, "default").Create(ctx, manual); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	e, err := client.Endpoints(c, "default").Get(ctx, "external")
	if err != nil || e.Subsets[0].Addresses[0].IP != "192.168.1.10" || e.Subsets[0].Ports[0].Protocol != corev1.ProtocolTCP {
		t.Fatalf("user Endpoints changed: %v %+v", err, e)
	}
}

func TestHeadlessAndExternalNameServices(t *testing.T) {
	c, reg := framework.StartTestServerWithRegistry(t)
	startEndpointsController(t, c)
	ctx := context.Background()
	svcs := client.Services(c, "default")

	headless, err := svcs.Create(ctx, newService("db", func(s *corev1.Service) {
		s.Spec.ClusterIP = corev1.ClusterIPNone
		s.Spec.PublishNotReadyAddresses = true
	}))
	if err != nil || headless.Spec.ClusterIP != corev1.ClusterIPNone {
		t.Fatalf("headless: %v %+v", err, headless)
	}
	labels := map[string]string{"app": "db"}
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "db-0", Labels: labels},
		Spec: corev1.PodSpec{NodeName: "n1", Hostname: "db-0", Subdomain: "db", Containers: []corev1.Container{{Name: "c", Image: "img"}}}}
	p, err = client.Pods(c, "default").Create(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	setReady(t, c, p, "10.244.1.9", false)
	e := waitEndpoints(t, c, "db", []epView{{Ready: "10.244.1.9(db-0) ", Ports: ":80 "}})
	if e.Subsets[0].Addresses[0].Hostname != "db-0" {
		t.Fatalf("hostname: %+v", e.Subsets[0].Addresses[0])
	}

	// A ClusterIP Service turned into an ExternalName gives its address back,
	// and gets a new one when it turns back.
	web, err := svcs.Create(ctx, newService("web", nil))
	if err != nil {
		t.Fatal(err)
	}
	external, err := svcs.Patch(ctx, "web", map[string]any{"spec": map[string]any{"type": "ExternalName", "externalName": "web.example.com"}})
	if err != nil || external.Spec.ClusterIP != "" {
		t.Fatalf("to ExternalName: %v %+v", err, external)
	}
	if ips, _ := reg.ServiceAllocators.IPs.Allocated(ctx); ips[web.Spec.ClusterIP] {
		t.Fatalf("%s still allocated", web.Spec.ClusterIP)
	}
	back, err := svcs.Patch(ctx, "web", map[string]any{"spec": map[string]any{"type": "ClusterIP"}})
	if err != nil || back.Spec.ClusterIP == "" || back.Spec.ExternalName != "" {
		t.Fatalf("back to ClusterIP: %v %+v", err, back)
	}
}
