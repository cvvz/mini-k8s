// Package nodeipam assigns each Node a Pod address range (spec.podCIDR) from
// the cluster CIDR, like the range allocator of Kubernetes' node IPAM
// controller.
package nodeipam

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/client/record"
	"mini-k8s/pkg/client/workqueue"
)

// DefaultClusterCIDR is the Pod address space; every node receives a /24.
const DefaultClusterCIDR = "10.244.0.0/16"

// Allocator hands out fixed-size subnets of a cluster CIDR.
type Allocator struct {
	cluster  *net.IPNet
	maskBits int

	mu   sync.Mutex
	used map[string]bool
}

// NewAllocator splits clusterCIDR into subnets of maskBits.
func NewAllocator(clusterCIDR string, maskBits int) (*Allocator, error) {
	_, cluster, err := net.ParseCIDR(clusterCIDR)
	if err != nil {
		return nil, err
	}
	ones, bits := cluster.Mask.Size()
	if bits != 32 || maskBits <= ones || maskBits > 30 {
		return nil, fmt.Errorf("cannot split %s into /%d subnets", clusterCIDR, maskBits)
	}
	return &Allocator{cluster: cluster, maskBits: maskBits, used: map[string]bool{}}, nil
}

// Occupy marks a subnet as taken (for ranges assigned earlier).
func (a *Allocator) Occupy(cidr string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.used[cidr] = true
}

// Release frees a subnet.
func (a *Allocator) Release(cidr string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.used, cidr)
}

// Next reserves the lowest free subnet. Subnet 0 is skipped so node N tends
// to get 10.244.N.0/24, which is easy to read.
func (a *Allocator) Next() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ones, _ := a.cluster.Mask.Size()
	count := 1 << (a.maskBits - ones)
	base := ipToUint(a.cluster.IP)
	size := uint32(1) << (32 - a.maskBits)
	for i := 1; i < count; i++ {
		cidr := fmt.Sprintf("%s/%d", uintToIP(base+uint32(i)*size), a.maskBits)
		if !a.used[cidr] {
			a.used[cidr] = true
			return cidr, nil
		}
	}
	return "", errors.New("pod CIDR space exhausted")
}

func ipToUint(ip net.IP) uint32 {
	v4 := ip.To4()
	return uint32(v4[0])<<24 | uint32(v4[1])<<16 | uint32(v4[2])<<8 | uint32(v4[3])
}

func uintToIP(v uint32) net.IP {
	return net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

// Controller watches Nodes and patches spec.podCIDR onto nodes without one.
type Controller struct {
	client    *client.Client
	nodes     *informers.Informer[corev1.Node, *corev1.Node]
	allocator *Allocator
	recorder  *record.Recorder
	queue     *workqueue.Queue
}

// NewController wires the controller onto a Node informer.
func NewController(c *client.Client, nodes *informers.Informer[corev1.Node, *corev1.Node], allocator *Allocator, recorder *record.Recorder) *Controller {
	ctl := &Controller{client: c, nodes: nodes, allocator: allocator, recorder: recorder, queue: workqueue.New()}
	nodes.AddHandler(informers.Handler[*corev1.Node]{
		OnAdd:    ctl.observe,
		OnUpdate: func(_, n *corev1.Node) { ctl.observe(n) },
		OnDelete: func(n *corev1.Node) {
			if n.Spec.PodCIDR != "" {
				allocator.Release(n.Spec.PodCIDR)
			}
		},
	})
	return ctl
}

func (c *Controller) observe(n *corev1.Node) {
	if n.Spec.PodCIDR != "" {
		c.allocator.Occupy(n.Spec.PodCIDR)
		return
	}
	c.queue.Add(n.Name)
}

// Run assigns ranges until ctx ends. Call after the informer has synced so
// existing assignments are known before new ones are made.
func (c *Controller) Run(ctx context.Context) {
	for _, n := range c.nodes.List("", nil) {
		c.observe(n)
	}
	workqueue.Run(ctx, c.queue, "nodeipam", 1, c.sync)
}

func (c *Controller) sync(ctx context.Context, name string) error {
	node, ok := c.nodes.Get("", name)
	if !ok || node.Spec.PodCIDR != "" {
		return nil
	}
	cidr, err := c.allocator.Next()
	if err != nil {
		c.recorder.Eventf(node, corev1.EventTypeWarning, "CIDRNotAvailable", "%v", err)
		return err
	}
	patch := map[string]any{"metadata": map[string]any{"resourceVersion": node.ResourceVersion}, "spec": map[string]any{"podCIDR": cidr}}
	if _, err := client.Nodes(c.client).Patch(ctx, name, patch); err != nil {
		c.allocator.Release(cidr)
		if client.IsNotFound(err) {
			return nil
		}
		return err // conflicts retry against the refreshed cache
	}
	log.Printf("nodeipam: assigned %s to node %s", cidr, name)
	c.recorder.Eventf(node, corev1.EventTypeNormal, "CIDRAssignmentSucceeded", "Node %s assigned Pod CIDR %s", name, cidr)
	return nil
}
