package allocator

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
)

// IPs allocates the host addresses of an IPv4 CIDR (the Service CIDR).
type IPs struct {
	r    Range
	base uint32 // the network address
	cidr *net.IPNet
}

// NewIPs returns an allocator for cidr whose state lives at key. As in
// Kubernetes, dynamic allocations avoid the first n/16 addresses (16 to
// 256), which stay free for addresses users pick (10.96.0.10 for DNS).
func NewIPs(store Store, key, cidr string) (*IPs, error) {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil || network.IP.To4() == nil {
		return nil, fmt.Errorf("invalid IPv4 Service CIDR %q", cidr)
	}
	ones, bits := network.Mask.Size()
	size := 1<<(bits-ones) - 2 // without the network and broadcast addresses
	if size < 1 {
		return nil, fmt.Errorf("Service CIDR %q is too small", cidr)
	}
	return &IPs{
		r:    Range{Store: store, Key: key, Name: network.String(), Size: size, Static: staticBand(size, 16, 256)},
		base: binary.BigEndian.Uint32(network.IP.To4()),
		cidr: network,
	}, nil
}

// CIDR returns the range, e.g. 10.96.0.0/12.
func (a *IPs) CIDR() string { return a.cidr.String() }

func (a *IPs) offset(ip string) (int, error) {
	parsed := net.ParseIP(ip).To4()
	if parsed == nil || !a.cidr.Contains(parsed) {
		return 0, errors.New("the provided network does not match the current range")
	}
	o := int(binary.BigEndian.Uint32(parsed)-a.base) - 1
	if o < 0 || o >= a.r.Size {
		return 0, errors.New("the provided IP is the network or broadcast address of the range")
	}
	return o, nil
}

func (a *IPs) ip(offset int) string {
	b := make(net.IP, 4)
	binary.BigEndian.PutUint32(b, a.base+uint32(offset)+1)
	return b.String()
}

// Allocate reserves ip; the error reads like Kubernetes' ("provided IP is
// already allocated", "the provided network does not match the current range").
func (a *IPs) Allocate(ctx context.Context, ip string) error {
	o, err := a.offset(ip)
	if err != nil {
		return err
	}
	if err := a.r.Allocate(ctx, o); errors.Is(err, ErrAllocated) {
		return errors.New("provided IP is already allocated")
	} else if err != nil {
		return err
	}
	return nil
}

// Check reports whether Allocate(ip) would succeed, without reserving it
// (for dry runs).
func (a *IPs) Check(ctx context.Context, ip string) error {
	o, err := a.offset(ip)
	if err != nil {
		return err
	}
	set, err := a.r.Allocated(ctx)
	if err == nil && set[o] {
		return errors.New("provided IP is already allocated")
	}
	return err
}

// AllocateNext reserves a free address.
func (a *IPs) AllocateNext(ctx context.Context) (string, error) {
	o, err := a.r.AllocateNext(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to allocate a serviceIP: %w", err)
	}
	return a.ip(o), nil
}

// Release frees addresses; addresses outside the range are ignored.
func (a *IPs) Release(ctx context.Context, ips ...string) error {
	var offsets []int
	for _, ip := range ips {
		if o, err := a.offset(ip); err == nil {
			offsets = append(offsets, o)
		}
	}
	return a.r.Release(ctx, offsets...)
}

// Allocated returns the allocated addresses.
func (a *IPs) Allocated(ctx context.Context) (map[string]bool, error) {
	set, err := a.r.Allocated(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(set))
	for o := range set {
		out[a.ip(o)] = true
	}
	return out, nil
}

// Ports allocates node ports from Base to Base+Size-1.
type Ports struct {
	r    Range
	base int
}

// NewPorts returns an allocator for the node ports first..last whose state
// lives at key; dynamic allocations avoid the first n/32 ports (16 to 128),
// as in Kubernetes.
func NewPorts(store Store, key string, first, last int) *Ports {
	size := last - first + 1
	return &Ports{r: Range{Store: store, Key: key, Name: fmt.Sprintf("%d-%d", first, last), Size: size,
		Static: staticBand(size, 32, 128)}, base: first}
}

// staticBand is Kubernetes' calculateRangeOffset: no band for ranges of
// fewer than 16 values, otherwise size/step clamped to [16, limit].
func staticBand(size, step, limit int) int {
	if size < 16 {
		return 0
	}
	return min(max(size/step, 16), limit)
}

// Range returns the range, e.g. 30000-32767.
func (a *Ports) Range() string { return a.r.Name }

func (a *Ports) offset(port int32) (int, error) {
	o := int(port) - a.base
	if o < 0 || o >= a.r.Size {
		return 0, fmt.Errorf("provided port is not in the valid range. The range of valid ports is %s", a.r.Name)
	}
	return o, nil
}

// Allocate reserves port.
func (a *Ports) Allocate(ctx context.Context, port int32) error {
	o, err := a.offset(port)
	if err != nil {
		return err
	}
	if err := a.r.Allocate(ctx, o); errors.Is(err, ErrAllocated) {
		return errors.New("provided port is already allocated")
	} else if err != nil {
		return err
	}
	return nil
}

// Check reports whether Allocate(port) would succeed, without reserving it.
func (a *Ports) Check(ctx context.Context, port int32) error {
	o, err := a.offset(port)
	if err != nil {
		return err
	}
	set, err := a.r.Allocated(ctx)
	if err == nil && set[o] {
		return errors.New("provided port is already allocated")
	}
	return err
}

// AllocateNext reserves a free port.
func (a *Ports) AllocateNext(ctx context.Context) (int32, error) {
	o, err := a.r.AllocateNext(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to allocate a nodePort: %w", err)
	}
	return int32(a.base + o), nil
}

// Release frees ports; ports outside the range are ignored.
func (a *Ports) Release(ctx context.Context, ports ...int32) error {
	var offsets []int
	for _, p := range ports {
		if o, err := a.offset(p); err == nil {
			offsets = append(offsets, o)
		}
	}
	return a.r.Release(ctx, offsets...)
}

// Allocated returns the allocated ports.
func (a *Ports) Allocated(ctx context.Context) (map[int32]bool, error) {
	set, err := a.r.Allocated(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[int32]bool, len(set))
	for o := range set {
		out[int32(a.base+o)] = true
	}
	return out, nil
}
