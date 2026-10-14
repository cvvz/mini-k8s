// Package service holds the API strategy for Services: besides defaults and
// validation it allocates cluster IPs and node ports when Services are
// written and releases them when they are removed, like the Service REST
// storage of the Kubernetes API server.
package service

import (
	"context"
	"fmt"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/apis/core/validation"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/apiserver"
	"mini-k8s/pkg/registry/core/service/allocator"
)

// Defaults of the Service CIDR and the node port range, as in kubeadm clusters.
const (
	DefaultServiceCIDR   = "10.96.0.0/12"
	DefaultNodePortFirst = 30000
	DefaultNodePortLast  = 32767
)

// Allocators hands out cluster IPs and node ports.
type Allocators struct {
	IPs   *allocator.IPs
	Ports *allocator.Ports
}

// NewAllocators keeps the allocation state under prefix + "/ranges/".
func NewAllocators(store allocator.Store, prefix string) *Allocators {
	ips, err := allocator.NewIPs(store, prefix+"/ranges/serviceips", DefaultServiceCIDR)
	if err != nil {
		panic(err) // the constant CIDR is valid
	}
	return &Allocators{IPs: ips, Ports: allocator.NewPorts(store, prefix+"/ranges/servicenodeports", DefaultNodePortFirst, DefaultNodePortLast)}
}

// NewStrategy returns the Service strategy backed by a.
func NewStrategy(a *Allocators) apiserver.Strategy {
	return apiserver.Strategy{
		NameIsDNSLabel: true,
		Defaults:       func(obj metav1.Object) { corev1.SetDefaultsServiceSpec(&obj.(*corev1.Service).Spec) },
		PrepareForUpdate: func(obj, old metav1.Object) {
			PrepareForUpdate(&obj.(*corev1.Service).Spec, &old.(*corev1.Service).Spec)
		},
		Validate: func(obj metav1.Object) []string { return validation.ValidateServiceSpec(&obj.(*corev1.Service).Spec) },
		ValidateUpdate: func(obj, old metav1.Object) []string {
			return validation.ValidateServiceUpdate(&obj.(*corev1.Service).Spec, &old.(*corev1.Service).Spec)
		},
		BeginCreate: func(ctx context.Context, obj metav1.Object, dryRun bool) (apiserver.FinishFunc, error) {
			return a.allocate(ctx, &obj.(*corev1.Service).Spec, nil, dryRun)
		},
		BeginUpdate: func(ctx context.Context, obj, old metav1.Object, dryRun bool) (apiserver.FinishFunc, error) {
			return a.allocate(ctx, &obj.(*corev1.Service).Spec, &old.(*corev1.Service).Spec, dryRun)
		},
		AfterDelete: func(ctx context.Context, obj metav1.Object) {
			// A failed release leaves a leak that the repair loop reclaims.
			spec := &obj.(*corev1.Service).Spec
			_ = a.IPs.Release(ctx, spec.ClusterIP)
			_ = a.Ports.Release(ctx, nodePorts(spec)...)
		},
	}
}

// PrepareForUpdate keeps what was allocated when an update leaves it out,
// as patchAllocatedValues does in Kubernetes: the cluster IP, and the node
// port of every port that still exists. Switching to type ClusterIP drops
// node ports the update did not change.
func PrepareForUpdate(spec, old *corev1.ServiceSpec) {
	if spec.ClusterIP == "" {
		spec.ClusterIP = old.ClusterIP
	}
	switch {
	case spec.Type == corev1.ServiceTypeNodePort && old.Type == corev1.ServiceTypeNodePort:
		used := map[int32]bool{}
		for _, p := range spec.Ports {
			used[p.NodePort] = true
		}
		for i := range spec.Ports {
			p := &spec.Ports[i]
			if o := matchingPort(old, p); p.NodePort == 0 && o != nil && !used[o.NodePort] {
				p.NodePort = o.NodePort
				used[o.NodePort] = true
			}
		}
	case spec.Type == corev1.ServiceTypeClusterIP && old.Type == corev1.ServiceTypeNodePort:
		for i := range spec.Ports {
			p := &spec.Ports[i]
			if o := matchingPort(old, p); o != nil && p.NodePort == o.NodePort {
				p.NodePort = 0
			}
		}
	}
}

// matchingPort finds p's counterpart in old: the port of the same name, or
// for unnamed ports the same number and protocol.
func matchingPort(old *corev1.ServiceSpec, p *corev1.ServicePort) *corev1.ServicePort {
	for i := range old.Ports {
		o := &old.Ports[i]
		if o.Protocol == p.Protocol && ((p.Name != "" && o.Name == p.Name) || (p.Name == "" && o.Port == p.Port)) {
			return o
		}
	}
	return nil
}

// nodePorts returns the distinct node ports of spec (TCP and UDP ports may
// share one).
func nodePorts(spec *corev1.ServiceSpec) []int32 {
	var out []int32
	seen := map[int32]bool{}
	for _, p := range spec.Ports {
		if p.NodePort != 0 && !seen[p.NodePort] {
			seen[p.NodePort] = true
			out = append(out, p.NodePort)
		}
	}
	return out
}

// allocate reserves the cluster IP and node ports spec needs beyond what
// old (nil on create) already holds. The returned function releases the new
// reservations if the write fails, and the ones spec gave up if it succeeds.
func (a *Allocators) allocate(ctx context.Context, spec, old *corev1.ServiceSpec, dryRun bool) (apiserver.FinishFunc, error) {
	if old == nil {
		old = &corev1.ServiceSpec{}
	}
	var ips []string
	var ports []int32
	undo := func(ctx context.Context) {
		_ = a.IPs.Release(ctx, ips...)
		_ = a.Ports.Release(ctx, ports...)
	}
	fail := func(err error) (apiserver.FinishFunc, error) {
		undo(ctx)
		return nil, err
	}
	if spec.ClusterIP == "" && !dryRun {
		ip, err := a.IPs.AllocateNext(ctx)
		if err != nil {
			return fail(err)
		}
		spec.ClusterIP = ip
		ips = append(ips, ip)
	} else if spec.ClusterIP != "" && spec.ClusterIP != old.ClusterIP {
		reserve := a.IPs.Allocate
		if dryRun {
			reserve = a.IPs.Check
		}
		if err := reserve(ctx, spec.ClusterIP); err != nil {
			return fail(apiserver.FieldErrors{fmt.Sprintf("spec.clusterIP: Invalid value: %q: failed to allocate IP %s: %v", spec.ClusterIP, spec.ClusterIP, err)})
		}
		if !dryRun {
			ips = append(ips, spec.ClusterIP)
		}
	}
	held := map[int32]bool{}
	for _, p := range nodePorts(old) {
		held[p] = true
	}
	if spec.Type == corev1.ServiceTypeNodePort {
		// Explicit node ports first, as initNodePorts does in Kubernetes: a
		// dynamic allocation for an earlier port must not take a value a
		// later port asks for.
		for i := range spec.Ports {
			p := &spec.Ports[i]
			if p.NodePort == 0 || held[p.NodePort] {
				continue
			}
			reserve := a.Ports.Allocate
			if dryRun {
				reserve = a.Ports.Check
			}
			if err := reserve(ctx, p.NodePort); err != nil {
				return fail(apiserver.FieldErrors{fmt.Sprintf("spec.ports[%d].nodePort: Invalid value: %d: %v", i, p.NodePort, err)})
			}
			held[p.NodePort] = true
			if !dryRun {
				ports = append(ports, p.NodePort)
			}
		}
		for i := range spec.Ports {
			if p := &spec.Ports[i]; p.NodePort == 0 && !dryRun {
				n, err := a.Ports.AllocateNext(ctx)
				if err != nil {
					return fail(err)
				}
				p.NodePort = n
				ports = append(ports, n)
			}
		}
	}
	return func(ctx context.Context, success bool) {
		if !success {
			undo(ctx)
			return
		}
		kept := map[int32]bool{}
		for _, p := range nodePorts(spec) {
			kept[p] = true
		}
		var released []int32
		for _, p := range nodePorts(old) {
			if !kept[p] {
				released = append(released, p)
			}
		}
		_ = a.Ports.Release(ctx, released...)
	}, nil
}
