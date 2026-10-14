package iptables

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/network"
)

// Proxier keeps the node's iptables rules in line with Services and Endpoints.
type Proxier struct {
	// Exec runs iptables and iptables-save.
	Exec network.Runner
	// Restore feeds rules to iptables-restore --noflush.
	Restore func(data []byte) error
	// ClusterCIDR covers every Pod; traffic from elsewhere to a cluster IP
	// is masqueraded.
	ClusterCIDR string
	// MinSyncPeriod spaces syncs triggered by changes; SyncPeriod is the
	// full resync that also repairs rules removed by others.
	MinSyncPeriod, SyncPeriod time.Duration

	services  *informers.Informer[corev1.Service, *corev1.Service]
	endpoints *informers.Informer[corev1.Endpoints, *corev1.Endpoints]
	trigger   chan struct{}
	applied   []byte
	// udp holds the endpoints of the UDP Service ports at the last sync.
	udp map[udpPort]map[string]bool
}

// NewProxier returns a Proxier that runs the real iptables commands.
func NewProxier(c *client.Client, clusterCIDR string) *Proxier {
	p := &Proxier{
		Exec:          network.Exec,
		Restore:       restore,
		ClusterCIDR:   clusterCIDR,
		MinSyncPeriod: time.Second,
		SyncPeriod:    30 * time.Second,
		services:      informers.New(client.Services(c, ""), client.ListOptions{}, 0),
		endpoints:     informers.New(client.Endpoints(c, ""), client.ListOptions{}, 0),
		trigger:       make(chan struct{}, 1),
	}
	poke := func() {
		select {
		case p.trigger <- struct{}{}:
		default:
		}
	}
	p.services.AddHandler(informers.Handler[*corev1.Service]{OnAdd: func(*corev1.Service) { poke() },
		OnUpdate: func(_, _ *corev1.Service) { poke() }, OnDelete: func(*corev1.Service) { poke() }})
	p.endpoints.AddHandler(informers.Handler[*corev1.Endpoints]{OnAdd: func(*corev1.Endpoints) { poke() },
		OnUpdate: func(_, _ *corev1.Endpoints) { poke() }, OnDelete: func(*corev1.Endpoints) { poke() }})
	return p
}

func restore(data []byte) error {
	cmd := exec.Command("iptables-restore", "-w", "--noflush")
	cmd.Stdin = bytes.NewReader(data)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("iptables-restore: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Run syncs the rules until ctx ends.
func (p *Proxier) Run(ctx context.Context) {
	go p.services.Run(ctx)
	go p.endpoints.Run(ctx)
	informers.WaitForSync(ctx, p.services.HasSynced, p.endpoints.HasSynced)
	full := time.NewTicker(p.SyncPeriod)
	defer full.Stop()
	force := true
	for {
		if err := p.Sync(force); err != nil {
			log.Printf("kube-proxy: sync failed: %v", err)
			force = true // retry soon, on the next change or tick
		} else {
			force = false
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(p.MinSyncPeriod):
		}
		select {
		case <-ctx.Done():
			return
		case <-p.trigger:
		case <-full.C:
			force = true
		}
	}
}

// Sync writes the rules for the current Services and Endpoints; without
// force it skips the write when nothing changed since the last one.
func (p *Proxier) Sync(force bool) error {
	return p.sync(p.services.List("", nil), p.endpoints.List("", nil), force)
}

func (p *Proxier) sync(services []*corev1.Service, endpoints []*corev1.Endpoints, force bool) error {
	existing, err := p.existingChains()
	if err != nil {
		return err
	}
	data := BuildRules(services, endpoints, p.ClusterCIDR, existing)
	if !force && bytes.Equal(data, p.applied) {
		return nil
	}
	if err := p.Restore(data); err != nil {
		p.applied = nil
		return err
	}
	p.applied = data
	p.clearStaleUDP(udpEndpoints(services, endpoints))
	return p.ensureJumps()
}

// existingChains lists the per-Service chains in the nat table.
func (p *Proxier) existingChains() ([]string, error) {
	out, err := p.Exec("iptables-save", "-t", "nat")
	if err != nil {
		return nil, err
	}
	var chains []string
	s := bufio.NewScanner(strings.NewReader(out))
	for s.Scan() {
		if name, ok := strings.CutPrefix(s.Text(), ":KUBE-"); ok {
			chains = append(chains, "KUBE-"+strings.Fields(name)[0])
		}
	}
	return chains, nil
}

// ensureJumps hooks kube-proxy's chains into the built-in chains.
func (p *Proxier) ensureJumps() error {
	for _, j := range jumps {
		if _, err := p.Exec("iptables", append([]string{"-w", "-t", j.table, "-C", j.chain}, j.rule...)...); err == nil {
			continue
		}
		if _, err := p.Exec("iptables", append([]string{"-w", "-t", j.table, "-I", j.chain}, j.rule...)...); err != nil {
			return err
		}
	}
	return nil
}
