package main

import (
	"context"
	"log"
	"net/http"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/dns"
	kubeletdns "mini-k8s/pkg/kubelet/dns"
)

// lameDuck is how long the DNS server keeps serving after SIGTERM.
const lameDuck = 5 * time.Second

// informerLookup serves the DNS zone from informer caches.
type informerLookup struct {
	services  *informers.Informer[corev1.Service, *corev1.Service]
	endpoints *informers.Informer[corev1.Endpoints, *corev1.Endpoints]
}

func (l informerLookup) Service(ns, name string) (*corev1.Service, bool) {
	return l.services.Get(ns, name)
}
func (l informerLookup) Endpoints(ns, name string) (*corev1.Endpoints, bool) {
	return l.endpoints.Get(ns, name)
}

// runClusterDNS serves the cluster domain on port 53 (UDP and TCP) and
// /healthz and /ready on :8080. It runs as the kube-dns Deployment with
// dnsPolicy Default, so its upstream is the node's resolver.
func runClusterDNS(ctx context.Context, c *client.Client, _ string) {
	upstream, err := kubeletdns.Upstream("/etc/resolv.conf")
	if err != nil {
		log.Fatal(err)
	}
	lookup := informerLookup{
		services:  informers.New(client.Services(c, ""), client.ListOptions{}, 0),
		endpoints: informers.New(client.Endpoints(c, ""), client.ListOptions{}, 0),
	}
	go lookup.services.Run(ctx)
	go lookup.endpoints.Run(ctx)
	server := &dns.Server{Upstream: upstream,
		Zone: &dns.Zone{Domain: env("MINI_CLUSTER_DOMAIN", "cluster.local"), TTL: 30, Data: lookup}}
	// Lame duck, as CoreDNS's health plugin: keep answering for a while
	// after SIGTERM, until kube-proxy has dropped this Pod from kube-dns.
	dnsCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
	go func() {
		<-ctx.Done()
		time.Sleep(lameDuck)
		stop()
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := server.ListenAndServe(dnsCtx, env("MINI_DNS_ADDR", ":53")); err != nil {
			log.Fatal(err)
		}
	}()
	defer func() { <-done }()
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("/ready", func(w http.ResponseWriter, _ *http.Request) {
		if !lookup.services.HasSynced() || !lookup.endpoints.HasSynced() {
			http.Error(w, "informers not synced", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	log.Printf("cluster DNS serving %s, forwarding to %s", server.Zone.Domain, upstream)
	serve(ctx, ":8080", mux)
}
