package cni

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// IPAM allocates addresses of a node's Pod subnet. Each allocation is a file
// named after the address and containing the container ID, guarded by a
// directory-wide flock (the scheme of the host-local plugin). The directory
// lives on tmpfs, so a node restart, which recreates every sandbox, also
// clears every allocation.
type IPAM struct {
	Dir string
}

func (s IPAM) lock(network string) (func(), error) {
	dir := filepath.Join(s.Dir, network)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// Allocate returns the address held by containerID or reserves the lowest
// free host address, skipping the network, gateway (first host) and
// broadcast addresses.
func (s IPAM) Allocate(network, containerID string, subnet *net.IPNet) (net.IP, error) {
	unlock, err := s.lock(network)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if ip, ok := s.lookup(network, containerID); ok {
		return ip, nil
	}
	base := ipToUint(subnet.IP)
	ones, bits := subnet.Mask.Size()
	size := uint32(1) << (bits - ones)
	for offset := uint32(2); offset < size-1; offset++ {
		ip := uintToIP(base + offset)
		f, err := os.OpenFile(filepath.Join(s.Dir, network, ip.String()), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		_, werr := f.WriteString(containerID)
		cerr := f.Close()
		if err := errors.Join(werr, cerr); err != nil {
			return nil, err
		}
		return ip, nil
	}
	return nil, fmt.Errorf("no free address in %s", subnet)
}

// Release frees the address held by containerID; a missing allocation is fine.
func (s IPAM) Release(network, containerID string) error {
	unlock, err := s.lock(network)
	if err != nil {
		return err
	}
	defer unlock()
	if ip, ok := s.lookup(network, containerID); ok {
		return os.Remove(filepath.Join(s.Dir, network, ip.String()))
	}
	return nil
}

// Lookup returns the address held by containerID.
func (s IPAM) Lookup(network, containerID string) (net.IP, bool) {
	unlock, err := s.lock(network)
	if err != nil {
		return nil, false
	}
	defer unlock()
	return s.lookup(network, containerID)
}

func (s IPAM) lookup(network, containerID string) (net.IP, bool) {
	entries, err := os.ReadDir(filepath.Join(s.Dir, network))
	if err != nil {
		return nil, false
	}
	for _, e := range entries {
		ip := net.ParseIP(e.Name())
		if ip == nil {
			continue
		}
		owner, err := os.ReadFile(filepath.Join(s.Dir, network, e.Name()))
		if err == nil && strings.TrimSpace(string(owner)) == containerID {
			return ip, true
		}
	}
	return nil, false
}

func ipToUint(ip net.IP) uint32 {
	v4 := ip.To4()
	return uint32(v4[0])<<24 | uint32(v4[1])<<16 | uint32(v4[2])<<8 | uint32(v4[3])
}

func uintToIP(v uint32) net.IP {
	return net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v)).To4()
}
