// Package endpoint implements the Endpoints controller: for every Service
// with a selector it keeps an Endpoints object of the same name listing the
// selected Pods, ready ones under addresses and the others under
// notReadyAddresses, like k8s.io/kubernetes/pkg/controller/endpoint.
// kube-proxy and the Ingress controller route to the ready addresses.
package endpoint

import (
	"context"
	"maps"
	"reflect"
	"sort"
	"strconv"
	"strings"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/client/workqueue"
	"mini-k8s/pkg/labels"
	"mini-k8s/pkg/util/deepcopy"
)

// The label Kubernetes puts on the Endpoints its controller maintains.
const (
	ManagedByLabel = "endpoints.kubernetes.io/managed-by"
	ManagedByValue = "endpoint-controller"
)

// Controller keeps Endpoints in line with Services and Pods.
type Controller struct {
	client    *client.Client
	services  *informers.Informer[corev1.Service, *corev1.Service]
	pods      *informers.Informer[corev1.Pod, *corev1.Pod]
	endpoints *informers.Informer[corev1.Endpoints, *corev1.Endpoints]
	queue     *workqueue.Queue
}

// NewController wires event handlers onto the shared informers.
func NewController(c *client.Client, services *informers.Informer[corev1.Service, *corev1.Service],
	pods *informers.Informer[corev1.Pod, *corev1.Pod], endpoints *informers.Informer[corev1.Endpoints, *corev1.Endpoints]) *Controller {
	ec := &Controller{client: c, services: services, pods: pods, endpoints: endpoints, queue: workqueue.New()}
	enqueueService := func(s *corev1.Service) { ec.queue.Add(informers.Key(s)) }
	services.AddHandler(informers.Handler[*corev1.Service]{
		OnAdd: enqueueService, OnUpdate: func(_, s *corev1.Service) { enqueueService(s) }, OnDelete: enqueueService,
	})
	podChanged := func(pods ...*corev1.Pod) {
		for _, p := range pods {
			for _, s := range ec.services.List(p.Namespace, nil) {
				if len(s.Spec.Selector) > 0 && labels.SelectorFromSet(s.Spec.Selector).Matches(p.Labels) {
					ec.queue.Add(informers.Key(s))
				}
			}
		}
	}
	pods.AddHandler(informers.Handler[*corev1.Pod]{
		OnAdd: func(p *corev1.Pod) { podChanged(p) }, OnUpdate: func(old, cur *corev1.Pod) { podChanged(old, cur) },
		OnDelete: func(p *corev1.Pod) { podChanged(p) },
	})
	// Endpoints events catch objects left behind by deleted Services and
	// edits of managed objects.
	enqueueEndpoints := func(e *corev1.Endpoints) { ec.queue.Add(informers.Key(e)) }
	endpoints.AddHandler(informers.Handler[*corev1.Endpoints]{
		OnAdd: enqueueEndpoints, OnUpdate: func(_, e *corev1.Endpoints) { enqueueEndpoints(e) }, OnDelete: enqueueEndpoints,
	})
	return ec
}

// Run processes Services until ctx ends.
func (ec *Controller) Run(ctx context.Context, workers int) {
	workqueue.Run(ctx, ec.queue, "endpoint", workers, ec.sync)
}

func (ec *Controller) sync(ctx context.Context, key string) error {
	ns, name := informers.SplitKey(key)
	current, exists := ec.endpoints.Get(ns, name)
	svc, ok := ec.services.Get(ns, name)
	if !ok {
		if exists && current.Labels[ManagedByLabel] == ManagedByValue {
			if err := client.Endpoints(ec.client, ns).Delete(ctx, name, nil); err != nil && !client.IsNotFound(err) {
				return err
			}
		}
		return nil
	}
	if len(svc.Spec.Selector) == 0 {
		return nil // the user maintains the Endpoints of Services without a selector
	}
	subsets := ComputeSubsets(svc, ec.pods.List(ns, labels.SelectorFromSet(svc.Spec.Selector)))
	want := map[string]string{}
	maps.Copy(want, svc.Labels)
	want[ManagedByLabel] = ManagedByValue
	if !exists {
		ep := &corev1.Endpoints{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: want}, Subsets: subsets}
		_, err := client.Endpoints(ec.client, ns).Create(ctx, ep)
		if client.IsAlreadyExists(err) {
			return nil // the informer has not seen it yet; its event requeues the Service
		}
		return err
	}
	if reflect.DeepEqual(current.Subsets, subsets) && reflect.DeepEqual(current.Labels, want) {
		return nil
	}
	ep := deepcopy.Copy(current)
	ep.Labels, ep.Subsets = want, subsets
	_, err := client.Endpoints(ec.client, ns).Update(ctx, ep)
	return err
}

// ComputeSubsets lists the Pods that back svc, grouped by their resolved
// ports as RepackSubsets does: Pods without an IP, finished Pods and
// terminating Pods are left out; a named targetPort a Pod does not declare
// leaves that Pod out of that port. Addresses are sorted by IP.
func ComputeSubsets(svc *corev1.Service, pods []*corev1.Pod) []corev1.EndpointSubset {
	groups := map[string]*corev1.EndpointSubset{}
	for _, p := range pods {
		if p.Status.PodIP == "" || p.DeletionTimestamp != nil || corev1.IsPodTerminal(p) {
			continue
		}
		var ports []corev1.EndpointPort
		for i := range svc.Spec.Ports {
			sp := &svc.Spec.Ports[i]
			if n, err := corev1.FindPort(p, sp); err == nil {
				ports = append(ports, corev1.EndpointPort{Name: sp.Name, Port: n, Protocol: sp.Protocol})
			}
		}
		if len(ports) == 0 {
			continue
		}
		sort.Slice(ports, func(i, j int) bool { return ports[i].Name < ports[j].Name })
		key := portsKey(ports)
		ss := groups[key]
		if ss == nil {
			ss = &corev1.EndpointSubset{Ports: ports}
			groups[key] = ss
		}
		node := p.Spec.NodeName
		addr := corev1.EndpointAddress{IP: p.Status.PodIP, NodeName: &node,
			TargetRef: &corev1.ObjectReference{Kind: "Pod", Namespace: p.Namespace, Name: p.Name, UID: p.UID}}
		if corev1.IsPodReady(p) {
			ss.Addresses = append(ss.Addresses, addr)
		} else {
			ss.NotReadyAddresses = append(ss.NotReadyAddresses, addr)
		}
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []corev1.EndpointSubset
	for _, k := range keys {
		ss := groups[k]
		byIP := func(list []corev1.EndpointAddress) {
			sort.Slice(list, func(i, j int) bool { return list[i].IP < list[j].IP })
		}
		byIP(ss.Addresses)
		byIP(ss.NotReadyAddresses)
		out = append(out, *ss)
	}
	return out
}

func portsKey(ports []corev1.EndpointPort) string {
	var b strings.Builder
	for _, p := range ports {
		b.WriteString(p.Name + "/" + p.Protocol + "/" + strconv.Itoa(int(p.Port)) + ";")
	}
	return b.String()
}
